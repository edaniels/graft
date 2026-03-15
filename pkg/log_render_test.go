package graft

import (
	"bytes"
	"strings"
	"testing"

	"go.viam.com/test"
)

func jsonLogLine(parts ...string) string {
	return strings.Join(parts, "")
}

func TestRenderJSONLogLineInfo(t *testing.T) {
	line := jsonLogLine(
		`{"time":"2025-01-24T15:30:46.000Z","level":"INFO",`,
		`"source":{"function":"pkg.(*Server).Run",`,
		`"file":"/Users/x/graft/pkg/server.go","line":42},`,
		`"msg":"Connection restored","name":"myserver"}`,
	)

	var buf bytes.Buffer
	RenderJSONLogLine(&buf, line)

	output := buf.String()

	test.That(t, output, test.ShouldContainSubstring, ansiFgGreen+"INFO ")
	test.That(t, output, test.ShouldContainSubstring,
		ansiFgBlue+ansiUnderline+"pkg/server.go:42"+ansiReset)
	test.That(t, output, test.ShouldContainSubstring, "Connection restored")
	test.That(t, output, test.ShouldContainSubstring, "name=")
	test.That(t, output, test.ShouldContainSubstring, "myserver")
	test.That(t, output, test.ShouldEndWith, "\n")
}

func TestRenderJSONLogLineWarn(t *testing.T) {
	line := jsonLogLine(
		`{"time":"2025-01-24T15:30:45.000Z","level":"WARN",`,
		`"source":{"function":"pkg.loadConfig",`,
		`"file":"/Users/x/graft/pkg/config.go","line":123},`,
		`"msg":"Config reload failed",`,
		`"error":"connection refused"}`,
	)

	var buf bytes.Buffer
	RenderJSONLogLine(&buf, line)

	output := buf.String()

	test.That(t, output, test.ShouldContainSubstring, ansiFgYellow+"WARN ")
	test.That(t, output, test.ShouldContainSubstring, ansiFgRed+"error=")
	test.That(t, output, test.ShouldContainSubstring, "connection refused")
}

func TestRenderJSONLogLineError(t *testing.T) {
	line := jsonLogLine(
		`{"time":"2025-01-24T15:30:45.000Z","level":"ERROR",`,
		`"source":{"function":"main",`,
		`"file":"/Users/x/graft/cmd/graft/main.go","line":10},`,
		`"msg":"fatal error"}`,
	)

	var buf bytes.Buffer
	RenderJSONLogLine(&buf, line)

	output := buf.String()

	test.That(t, output, test.ShouldContainSubstring, ansiFgRed+"ERROR")
}

func TestRenderJSONLogLineDebug(t *testing.T) {
	line := jsonLogLine(
		`{"time":"2025-01-24T15:30:45.000Z","level":"DEBUG",`,
		`"source":{"function":"pkg.foo",`,
		`"file":"pkg/foo.go","line":1},"msg":"trace"}`,
	)

	var buf bytes.Buffer
	RenderJSONLogLine(&buf, line)

	output := buf.String()

	test.That(t, output, test.ShouldContainSubstring, ansiFgMagenta+"DEBUG")
}

func TestRenderJSONLogLineEmptyLine(t *testing.T) {
	var buf bytes.Buffer
	RenderJSONLogLine(&buf, "")

	test.That(t, buf.String(), test.ShouldBeEmpty)
}

func TestRenderJSONLogLineNonJSON(t *testing.T) {
	var buf bytes.Buffer
	RenderJSONLogLine(&buf, "this is not json at all")

	test.That(t, buf.String(), test.ShouldContainSubstring,
		"this is not json at all")
}

func TestRenderJSONLogLineMultipleAttrs(t *testing.T) {
	line := jsonLogLine(
		`{"time":"2025-01-24T15:30:45.000Z","level":"INFO",`,
		`"source":{"function":"pkg.Run",`,
		`"file":"pkg/run.go","line":5},`,
		`"msg":"starting","host":"example.com","port":8080}`,
	)

	var buf bytes.Buffer
	RenderJSONLogLine(&buf, line)

	output := buf.String()

	test.That(t, output, test.ShouldContainSubstring, "host=")
	test.That(t, output, test.ShouldContainSubstring, "example.com")
	test.That(t, output, test.ShouldContainSubstring, "port=")
	test.That(t, output, test.ShouldContainSubstring, "8080")
}

func TestRenderJSONLogLineTimeParsing(t *testing.T) {
	line := jsonLogLine(
		`{"time":"2025-01-24T15:30:45.123456789Z",`,
		`"level":"INFO","source":{"function":"",`,
		`"file":"x.go","line":1},"msg":"hi"}`,
	)

	var buf bytes.Buffer
	RenderJSONLogLine(&buf, line)

	output := buf.String()

	// time.Stamp format: "Jan _2 15:04:05"
	test.That(t, output, test.ShouldContainSubstring, "Jan 24 15:30:45")
}

func TestShortSourcePath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			"/Users/x/go/src/github.com/edaniels/graft/pkg/server.go",
			"pkg/server.go",
		},
		{"pkg/server.go", "pkg/server.go"},
		{"server.go", "server.go"},
		{"/a/b/c/d.go", "c/d.go"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			test.That(t, shortSourcePath(tc.input),
				test.ShouldEqual, tc.expected)
		})
	}
}
