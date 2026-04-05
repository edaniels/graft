// Package slogor provides a colorful slog handler.
package slogor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"unicode"
	"unicode/utf8"
)

type GroupOrAttrs struct {
	attr  slog.Attr
	group string
}

// Handler is a slog.Handler that writes Records to an io.Writer as a sequence of colorful time, message, and pairs separated by spaces and followed by a newline.
type Handler struct {
	Writer  io.Writer      // Writer is the destination for the log records.
	Mutex   *sync.Mutex    // Mutex for handling concurrent access to the handler.
	Options options        // Options is the configuration for the log handler.
	goa     []GroupOrAttrs // goa for handling group or attributes.
}

// NewHandler creates a Handler that writes to w with the provided Options.
func NewHandler(writer io.Writer, fns ...OptionFn) *Handler {
	opt := getDefaultOptions().ConsumeFnOpt(fns...)

	// Return a new Handler with the provided writer and Options.
	return &Handler{
		Mutex:   &sync.Mutex{},
		Writer:  writer,
		Options: *opt,
		goa:     []GroupOrAttrs{},
	}
}

// Enabled reports whether the handler handles records at the given level.
// The handler ignores records whose level is lower.
func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level.Level() >= h.Options.level.Level()
}

// WithGroup returns a new handler with the new group attached to it.
func (h *Handler) WithGroup(group string) slog.Handler {
	return &Handler{
		Mutex:   h.Mutex, // we share the mutex from the parent handler
		Writer:  h.Writer,
		Options: h.Options,
		goa:     append(h.goa, GroupOrAttrs{group: group}),
	}
}

// WithAttrs returns a new handler with the attrs attached to it.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]GroupOrAttrs, len(attrs))

	for i, attr := range attrs {
		newAttrs[i] = GroupOrAttrs{attr: attr}
	}

	return &Handler{
		Mutex:   h.Mutex, // we share the mutex from the parent handler
		Writer:  h.Writer,
		Options: h.Options,
		goa:     append(h.goa, newAttrs...),
	}
}

// Handle processes the log record and writes it to the writer with appropriate formatting.
func (h *Handler) Handle(_ context.Context, record slog.Record) error {
	bufp := allocBuf() // Allocate a buffer for writing the log record.
	buf := *bufp

	defer func() {
		*bufp = buf
		freeBuf(bufp)
	}()

	// Write time if the record has a valid time and TimeFormat is specified.
	if h.Options.timeFormat != "" && !record.Time.IsZero() {
		// Format and append time information to the buffer.
		buf = h.Options.addColorToBuf(buf, faint)
		buf = append(buf, record.Time.Format(h.Options.timeFormat)...)
		buf = h.Options.addColorToBuf(buf, normalIntensity)
		buf = append(buf, ' ')
	}

	buf = h.addLogLevel(buf, record)

	var senti error

	// If configured, write the source file and line information.
	for h.Options.showSource {
		buf = h.Options.addColorToBuf(buf, fgBlue)
		buf = h.Options.addColorToBuf(buf, underline)

		frame, _ := runtime.CallersFrames([]uintptr{record.PC}).Next()

		dir, file := filepath.Split(frame.File)

		rootDir, err := os.Getwd()
		if err != nil {
			senti = fmt.Errorf("failed to get the root directory: %w", err)

			break
		}

		// Trim the root directory prefix to get the relative directory of the source file
		relativeDir, err := filepath.Rel(rootDir, filepath.Dir(dir))
		if err != nil {
			senti = fmt.Errorf("failed to get the relative directory: %w", err)

			buf = append(buf, file...)
			buf = append(buf, ':')
			buf = strconv.AppendInt(buf, int64(frame.Line), 10)
			buf = h.Options.addColorToBuf(buf, reset)
			buf = append(buf, ' ')

			break
		}

		buf = append(buf, filepath.Join(relativeDir, file)...)
		buf = append(buf, ':')
		buf = strconv.AppendInt(buf, int64(frame.Line), 10)
		buf = h.Options.addColorToBuf(buf, reset)
		buf = append(buf, ' ')

		break
	}

	// Write the log message.
	buf = append(buf, record.Message...)
	buf = append(buf, ' ')

	lastGroup := ""
	for _, goa := range h.goa {
		switch {
		case goa.group != "":
			lastGroup += goa.group + "."
		default:
			attr := goa.attr
			if lastGroup != "" {
				attr.Key = lastGroup + attr.Key
			}

			buf = appendAttr(buf, attr, h.Options.addColorToBuf)
		}
	}

	// If there are additional attributes, append them to the log record.
	if record.NumAttrs() > 0 {
		record.Attrs(func(attr slog.Attr) bool {
			if lastGroup != "" {
				attr.Key = lastGroup + attr.Key
			}
			buf = appendAttr(buf, attr, h.Options.addColorToBuf)

			return true
		})
	}

	// Replace the latest space by an EOL.
	buf[len(buf)-1] = '\n'

	// Lock the handler for writing and unlock once finished.
	h.Mutex.Lock()
	defer h.Mutex.Unlock()

	// Write the buffer to the writer.

	if _, err := h.Writer.Write(buf); err != nil {
		return fmt.Errorf("failed to write buffer: %w", err)
	}

	return senti
}

func (h *Handler) addLogLevel(buf []byte, record slog.Record) []byte {
	// Write level with appropriate formatting and color.
	// Also append right padding depending on the log level.
	switch record.Level {
	case slog.LevelInfo:
		buf = h.Options.addColorToBuf(buf, fgGreen)
	case slog.LevelError:
		buf = h.Options.addColorToBuf(buf, fgRed)
	case slog.LevelWarn:
		buf = h.Options.addColorToBuf(buf, fgYellow)
	case slog.LevelDebug:
		buf = h.Options.addColorToBuf(buf, fgMagenta)
	}

	if str, ok := h.Options.strLvl[record.Level]; ok {
		buf = append(buf, str...)
	} else {
		buf = append(buf, "-----"...)
	}

	buf = h.Options.addColorToBuf(buf, reset)

	return append(buf, ' ')
}

// appendAttr appends the attribute to the buffer.
func appendAttr(buf []byte, attr slog.Attr, addColor func([]byte, sgrCode) []byte) []byte {
	// Resolve the Attr's value before doing anything else.
	attr.Value = attr.Value.Resolve()

	// Ignore empty Attrs.
	if attr.Equal(slog.Attr{}) {
		return buf
	}

	buf = addColor(buf, faint)
	buf = addColor(buf, bold)

	// If attr is an error, write the red color code
	_, isErr := attr.Value.Any().(error)
	if isErr {
		buf = addColor(buf, fgRed)
	}

	buf = append(buf, attr.Key...)
	buf = append(buf, '=')
	buf = addColor(buf, normalIntensity)

	// if attr is not an error, write the cyan color code
	if !isErr {
		buf = addColor(buf, fgCyan)
	}

	s := attr.Value.String()
	if needsQuoting(s) {
		buf = strconv.AppendQuote(buf, s)
	} else {
		buf = append(buf, s...)
	}

	buf = addColor(buf, reset)
	buf = append(buf, ' ')

	return buf
}

// Err creates a slog.Attr error from anything.
func Err(err any) slog.Attr {
	return slog.Any("err", err)
}

// needsQuoting is a (minor) adapted version of Go's own function, found in
// https://cs.opensource.google/go/go/+/refs/tags/go1.23.2:src/log/slog/text_handler.go;l=141-163
func needsQuoting(s string) bool {
	if len(s) == 0 {
		return true
	}
	for i := 0; i < len(s); {
		b := s[i]
		if b < utf8.RuneSelf {
			if b != '\\' && (b == ' ' || b == '=' || b == '"') {
				return true
			}
			i++

			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError || unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return true
		}
		i += size
	}

	return false
}
