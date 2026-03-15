package graft

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"gitlab.com/greyxor/slogor"
	"golang.org/x/term"

	"github.com/edaniels/graft/errors"
)

// NewLogger returns a logger that excludes logs less severe than the default, unless one is set by GRAFT_LOG_LEVEL.
func NewLogger(defaultLevel slog.Level) *slog.Logger {
	logger, _ := newLogger(defaultLevel)

	return logger
}

func newLogger(defaultLevel slog.Level) (*slog.Logger, slog.Level) {
	logLevel, ok := os.LookupEnv("GRAFT_LOG_LEVEL")
	selectedLogLevel := defaultLevel

	if ok {
		switch strings.ToLower(logLevel) {
		case "debug":
			selectedLogLevel = slog.LevelDebug
		case "info":
			selectedLogLevel = slog.LevelInfo
		case "warn":
			selectedLogLevel = slog.LevelWarn
		case logStringError:
			selectedLogLevel = slog.LevelError
		}
	}

	var handler slog.Handler

	if term.IsTerminal(int(os.Stderr.Fd())) {
		handler = slogor.NewHandler(os.Stderr,
			slogor.SetLevel(selectedLogLevel),
			slogor.SetTimeFormat(time.Stamp),
			slogor.ShowSource(),
		)
	} else {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			AddSource: true,
			Level:     selectedLogLevel,
		})
	}

	return slog.New(stackTraceHandler{handler}), selectedLogLevel
}

// NewBufferedLogger returns a logger that excludes logs less severe than the default, unless one is set by GRAFT_LOG_LEVEL.
// It also returns a buffer that retains recent logs to assist in debugging.
func NewBufferedLogger(defaultLevel slog.Level) (*slog.Logger, *BufferedLineWriter) {
	logger, level := newLogger(defaultLevel)
	buffWriter := &BufferedLineWriter{MaxLines: 10}
	handlers := []slog.Handler{logger.Handler(), slog.NewJSONHandler(buffWriter, &slog.HandlerOptions{
		AddSource: true,
		Level:     level,
	})}

	return slog.New(newCopyHandler(handlers...)), buffWriter
}

// stackTraceHandler adds an error_stack attribute to logs if an error is set, contains a stack, and is
// not already set.
type stackTraceHandler struct {
	handler slog.Handler // all the destinations
}

func (h stackTraceHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.handler.Enabled(ctx, l)
}

func (h stackTraceHandler) Handle(ctx context.Context, r slog.Record) error {
	var (
		errAttr       error
		hasErrorStack bool
	)

	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "error_stack":
			hasErrorStack = true
		case logStringError:
			if valErr, ok := a.Value.Any().(error); ok {
				errAttr = valErr
			}
		}

		return true
	})

	if errAttr != nil && !hasErrorStack {
		var errsWithStack []*errors.Error

		{
			var (
				e  errors.JoinedError
				e1 *errors.Error
			)

			switch {
			case errors.As(errAttr, &e):
				allErrs := e.Unwrap()

				errsWithStack = make([]*errors.Error, 0, len(allErrs))
				for _, err := range allErrs {
					errWithStack := &errors.Error{}
					if errors.As(err, &errWithStack) {
						errsWithStack = append(errsWithStack, errWithStack)
					}
				}
			case errors.As(errAttr, &e1):
				errsWithStack = append(errsWithStack, e1)
			}
		}

		fmtStack := func(err *errors.Error) string {
			return strings.ReplaceAll(err.ErrorStack(), "\t", "")
		}

		if len(errsWithStack) == 1 {
			r.AddAttrs(slog.String("error_stack", fmtStack(errsWithStack[0])))
		} else {
			for idx, errWithStack := range errsWithStack {
				r.AddAttrs(slog.String(fmt.Sprintf("error_stack_%d", idx), fmtStack(errWithStack)))
			}
		}
	}

	err := h.handler.Handle(ctx, r)
	if err != nil {
		return errors.Wrap(err)
	}

	return nil
}

func (h stackTraceHandler) WithGroup(name string) slog.Handler {
	return stackTraceHandler{h.handler.WithGroup(name)}
}

func (h stackTraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return stackTraceHandler{h.handler.WithAttrs(attrs)}
}

// copyHandler copies a handled record to all underlying handlers.
// from https://stackoverflow.com/questions/79259186/how-can-i-set-gos-log-slog-to-send-to-multiple-outputs-console-file-and-in-d
type copyHandler struct {
	mu  *sync.Mutex
	out []slog.Handler // all the destinations
}

func newCopyHandler(handlers ...slog.Handler) *copyHandler {
	h := &copyHandler{out: handlers, mu: &sync.Mutex{}}

	return h
}

func (h *copyHandler) Enabled(_ context.Context, _ slog.Level) bool {
	// leave the enable check to the underlying handlers
	return true
}

func (h *copyHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, destHandler := range h.out {
		if !destHandler.Enabled(ctx, r.Level) {
			continue
		}

		err := destHandler.Handle(ctx, r)
		if err != nil {
			return errors.Wrap(err)
		}
	}

	return nil
}

func (h *copyHandler) WithGroup(name string) slog.Handler {
	// call WithGroup on the underlying handlers
	// we should not make modification the receiver, we return a copy
	if name == "" {
		return h
	}

	h2 := *h

	h2.out = make([]slog.Handler, len(h.out))
	for i, h := range h.out {
		h2.out[i] = h.WithGroup(name)
	}

	return &h2
}

func (h *copyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// call WithAttrs on the underlying handlers
	// we should not make modification the receiver, we return a copy
	if len(attrs) == 0 {
		return h
	}

	h2 := *h

	h2.out = make([]slog.Handler, len(h.out))
	for i, h := range h.out {
		h2.out[i] = h.WithAttrs(attrs)
	}

	return &h2
}
