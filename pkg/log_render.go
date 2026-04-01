package graft

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ANSI SGR codes matching slogor's formatting.
const (
	ansiReset           = "\033[0m"
	ansiFaint           = "\033[2m"
	ansiBold            = "\033[1m"
	ansiNormalIntensity = "\033[22m"
	ansiUnderline       = "\033[4m"
	ansiFgRed           = "\033[31m"
	ansiFgGreen         = "\033[32m"
	ansiFgYellow        = "\033[33m"
	ansiFgBlue          = "\033[34m"
	ansiFgMagenta       = "\033[35m"
	ansiFgCyan          = "\033[36m"

	logStringError = "error"
)

// RenderJSONLogLine parses a single JSON log line (as produced by slog.JSONHandler with
// AddSource: true) and writes slogor-style colored output to w.
func RenderJSONLogLine(w io.Writer, line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}

	// Use json.Decoder with Token() to preserve key ordering for attributes.
	dec := json.NewDecoder(strings.NewReader(line))

	// Expect opening brace.
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		// Not valid JSON; write raw line.
		fmt.Fprintln(w, line)

		return
	}

	var (
		timeStr string
		level   string
		source  string
		msg     string
		attrs   []attrPair
	)

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			break
		}

		key, ok := keyTok.(string)
		if !ok {
			break
		}

		switch key {
		case "time":
			var v string
			if dec.Decode(&v) == nil {
				if t, parseErr := time.Parse(time.RFC3339Nano, v); parseErr == nil {
					timeStr = t.Format(time.Stamp)
				} else {
					timeStr = v
				}
			}
		case "level":
			dec.Decode(&level) //nolint:errcheck
		case "source":
			source = decodeSource(dec)
		case "msg":
			dec.Decode(&msg) //nolint:errcheck
		default:
			val := decodeJSONValue(dec)
			attrs = append(attrs, attrPair{key, val})
		}
	}

	var buf strings.Builder

	// Time - faint.
	if timeStr != "" {
		buf.WriteString(ansiFaint)
		buf.WriteString(timeStr)
		buf.WriteString(ansiNormalIntensity)
		buf.WriteByte(' ')
	}

	// Level - colored, padded to 5 chars.
	levelColor := levelANSIColor(level)
	buf.WriteString(levelColor)
	buf.WriteString(padLevel(level))
	buf.WriteString(ansiReset)
	buf.WriteByte(' ')

	// Source - blue + underline.
	if source != "" {
		buf.WriteString(ansiFgBlue)
		buf.WriteString(ansiUnderline)
		buf.WriteString(source)
		buf.WriteString(ansiReset)
		buf.WriteByte(' ')
	}

	// Message - plain.
	buf.WriteString(msg)
	buf.WriteByte(' ')

	// Attributes - faint+bold key, cyan/red value.
	for _, a := range attrs {
		isErr := a.key == logStringError

		buf.WriteString(ansiFaint)
		buf.WriteString(ansiBold)

		if isErr {
			buf.WriteString(ansiFgRed)
		}

		buf.WriteString(a.key)
		buf.WriteByte('=')
		buf.WriteString(ansiNormalIntensity)

		if !isErr {
			buf.WriteString(ansiFgCyan)
		}

		buf.WriteString(a.val)
		buf.WriteString(ansiReset)
		buf.WriteByte(' ')
	}

	// Replace trailing space with newline (matching slogor).
	s := buf.String()
	if len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}

	fmt.Fprintln(w, s)
}

type attrPair struct {
	key string
	val string
}

// decodeSource reads the "source" JSON object and returns "file:line".
func decodeSource(dec *json.Decoder) string {
	tok, err := dec.Token()
	if err != nil {
		return ""
	}

	// If it's not an object, try to treat it as a string.
	if tok != json.Delim('{') {
		if s, ok := tok.(string); ok {
			return s
		}

		return fmt.Sprintf("%v", tok)
	}

	var (
		file string
		line float64
	)

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			break
		}

		key, _ := keyTok.(string)

		switch key {
		case "file":
			dec.Decode(&file) //nolint:errcheck
		case "line":
			dec.Decode(&line) //nolint:errcheck
		default:
			// Skip unknown fields (e.g. "function").
			var discard json.RawMessage
			dec.Decode(&discard) //nolint:errcheck
		}
	}

	// Consume closing brace.
	dec.Token() //nolint:errcheck

	if file == "" {
		return ""
	}

	// Make path relative-looking: strip everything up to and including the module root.
	// slog records full paths like "/Users/x/go/pkg/mod/github.com/foo/bar/pkg/server.go".
	// We want just "pkg/server.go" so use the last two path components.
	file = shortSourcePath(file)

	return fmt.Sprintf("%s:%d", file, int(line))
}

// shortSourcePath returns a short relative path for display. It keeps the last directory
// component and filename (e.g. "pkg/server.go" from "/full/path/to/pkg/server.go").
func shortSourcePath(fullPath string) string {
	dir, base := filepath.Split(fullPath)
	if dir == "" {
		return base
	}

	dir = strings.TrimRight(dir, string(filepath.Separator))
	parent := filepath.Base(dir)

	return filepath.Join(parent, base)
}

// decodeJSONValue reads the next JSON value from the decoder and returns it as a string.
func decodeJSONValue(dec *json.Decoder) string {
	tok, err := dec.Token()
	if err != nil {
		return ""
	}

	switch v := tok.(type) {
	case string:
		return v
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}

		return fmt.Sprintf("%g", v)
	case bool:
		return strconv.FormatBool(v)
	case nil:
		return "null"
	case json.Delim:
		// Object or array; consume it and return raw.
		return consumeComplex(dec, v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// consumeComplex consumes a JSON object or array and returns a compact string representation.
func consumeComplex(dec *json.Decoder, opening json.Delim) string {
	var depth int

	var parts []string

	parts = append(parts, string(opening))

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}

		switch v := tok.(type) {
		case json.Delim:
			parts = append(parts, string(v))

			if v == '{' || v == '[' {
				depth++
			} else {
				if depth == 0 {
					return strings.Join(parts, "")
				}

				depth--
			}
		case string:
			parts = append(parts, fmt.Sprintf("%q", v))
		default:
			parts = append(parts, fmt.Sprintf("%v", v))
		}
	}

	return strings.Join(parts, "")
}

func levelANSIColor(level string) string {
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "DEBUG":
		return ansiFgMagenta
	case "INFO":
		return ansiFgGreen
	case "WARN":
		return ansiFgYellow
	case "ERROR":
		return ansiFgRed
	default:
		return ""
	}
}

func padLevel(level string) string {
	level = strings.ToUpper(strings.TrimSpace(level))

	// Pad to 5 chars for alignment.
	for len(level) < 5 {
		level += " "
	}

	return level
}
