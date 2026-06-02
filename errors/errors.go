// Package errors just makes it easier to use github.com/go-errors/errors and not accidentally mix
// std-errors.
//
//nolint:revive // intentionally conflicting with stdlib
package errors

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"

	"github.com/go-errors/errors"
	"google.golang.org/grpc/status"
)

type Error = errors.Error

// ErrorLike is a stupid hack to trick wrapcheck into believing we have wrapped errors in this
// library, which we do. Without this and returning an `error` from functions, it looks like we
// are an external package to others that needs to be wrapped (a circular idea).
type ErrorLike interface {
	error
}

// ErrorMetadataFieldConnectionNameHint is used to hint that a connection name could be used to potentially
// correct the related error.
const ErrorMetadataFieldConnectionNameHint = "connectionNameHint"

// NewBare returns an error without a stacktrace.
func NewBare(e string) error {
	//nolint:err113
	return stderrors.New(e)
}

// New makes an Error from the given value. If that value is already an
// error then it will be used directly, if not, it will be passed to
// fmt.Errorf("%v"). The stacktrace will point to the line of code that
// called New.
func New(e string) ErrorLike {
	return errors.Wrap(NewBare(e), 1)
}

// Wrap makes an Error from the given value. If that value is already an *Error
// it will not be wrapped and instead will be returned without modification. If
// that value is already an error then it will be used directly and wrapped.
// Otherwise, the value will be passed to fmt.Errorf("%v") and then wrapped. To
// explicitly wrap an *Error with a new stacktrace use Errorf. The skip
// parameter indicates how far up the stack to start the stacktrace. 0 is from
// the current call, 1 from its caller, etc.
func Wrap(e any) ErrorLike {
	// TODO(erd): Verify that skip=1 produces correct caller info for wrapped errors.
	return handleGRPCStatus(e, errors.Wrap(e, 1))
}

// WrapPrefix makes an Error from the given value. If that value is already an
// *Error it will not be wrapped and instead will be returned without
// modification. If that value is already an error then it will be used
// directly and wrapped.  Otherwise, the value will be passed to
// fmt.Errorf("%v") and then wrapped. To explicitly wrap an *Error with a new
// stacktrace use Errorf. The prefix parameter is used to add a prefix to the
// error message when calling Error().
func WrapPrefix(e any, prefix string) ErrorLike {
	return handleGRPCStatus(e, errors.WrapPrefix(e, prefix, 1))
}

// WrapSuffix makes an Error from the given error. If that value is already an
// *Error it will not be wrapped and instead will be returned without
// modification. If that value is already an error then it will be used
// directly and wrapped.  Otherwise, the value will be passed to
// fmt.Errorf("%v") and then wrapped. To explicitly wrap an *Error with a new
// stacktrace use Errorf. The suffix parameter is used to add a suffix to the
// error message when calling Error().
func WrapSuffix(e error, suffix string) ErrorLike {
	return handleGRPCStatus(e, errors.Wrap(fmt.Errorf("%w: %s", e, suffix), 1))
}

// Errorf creates a new error with the given message. You can use it
// as a drop-in replacement for fmt.Errorf() to provide descriptive
// errors in return values.
func Errorf(format string, a ...any) ErrorLike {
	//nolint:err113
	return errors.Wrap(fmt.Errorf(format, a...), 2)
}

// As assigns error or any wrapped error to the value target points
// to. If there is no value of the target type of target As returns
// false.
func As(err error, target any) bool {
	return errors.As(err, target)
}

// Is detects whether the error is equal to a given error. Errors
// are considered equal by this function if they are the same object,
// or if they both contain the same error inside an errors.Error.
func Is(e error, original error) bool {
	return errors.Is(e, original)
}

// Join returns an error that wraps the given errors.
// Any nil error values are discarded.
// Join returns nil if every value in errs is nil.
// The error formats as the concatenation of the strings obtained
// by calling the Error method of each element of errs, with a newline
// between each string.
//
// A non-nil error returned by Join implements the Unwrap() []error method.
//
// For more information see stdlib errors.Join.
func Join(errs ...error) error {
	return errors.Join(errs...)
}

// Unwrap returns the result of calling the Unwrap method on err, if err's
// type contains an Unwrap method returning error.
// Otherwise, Unwrap returns nil.
//
// Unwrap only calls a method of the form "Unwrap() error".
// In particular Unwrap does not unwrap errors returned by [Join].
//
// For more information see stdlib errors.Unwrap.
func Unwrap(err error) error {
	return errors.Unwrap(err)
}

// JoinedError is the typical interface for a errors.Join'ed error.
type JoinedError interface {
	Unwrap() []error
}

// Unchecked logs the error if it is non-nil. Use this for errors you probably don't care about but don't
// necessarily want to miss out in the logs.
func Unchecked(err error) {
	if err == nil {
		return
	}

	slog.Default().InfoContext(context.Background(), "unchecked error", "error", errors.Wrap(err, 1))
}

// UncheckedFunc logs the resulting error if it is non-nil. Use this for errors you probably don't care about but don't
// necessarily want to miss out in the logs.
func UncheckedFunc(f func() error) {
	err := f()
	if err == nil {
		return
	}

	slog.Default().InfoContext(context.Background(), "unchecked error", "error", errors.Wrap(err, 1))
}

// UncheckedValue logs the error of a _, err function if it is non-nil. Use this for values you drop and
// errors you probably don't care about but don't necessarily want to miss out in the logs.
func UncheckedValue(value any, err error) {
	if err == nil {
		return
	}

	slog.Default().InfoContext(context.Background(), "unchecked error", "value", value, "error", errors.Wrap(err, 1))
}

type wrappedGRPCStatusError struct {
	wrapped *Error
	status  *status.Status
}

// Error returns the underlying error's message.
func (err wrappedGRPCStatusError) Error() string {
	return err.wrapped.Error()
}

// Stack returns the callstack formatted the same way that go does
// in runtime/debug.Stack().
func (err wrappedGRPCStatusError) Stack() []byte {
	return err.wrapped.Stack()
}

// Callers satisfies the bugsnag ErrorWithCallerS() interface
// so that the stack can be read out.
func (err wrappedGRPCStatusError) Callers() []uintptr {
	return err.wrapped.Callers()
}

// ErrorStack returns a string that contains both the
// error message and the callstack.
func (err wrappedGRPCStatusError) ErrorStack() string {
	return err.wrapped.ErrorStack()
}

// StackFrames returns an array of frames containing information about the
// stack.
func (err wrappedGRPCStatusError) StackFrames() []errors.StackFrame {
	return err.wrapped.StackFrames()
}

// TypeName returns the type this error. e.g. *errors.stringError.
func (err wrappedGRPCStatusError) TypeName() string {
	return err.wrapped.TypeName()
}

// Unwrap returns the wrapped error (implements api for As function).
func (err wrappedGRPCStatusError) Unwrap() error {
	//nolint:wrapcheck
	return err.wrapped.Unwrap()
}

// GRPCStatus returns the wrapped gRPC status.
func (err wrappedGRPCStatusError) GRPCStatus() *status.Status {
	return err.status
}

func handleGRPCStatus(original any, wrapped *Error) error {
	if err, ok := original.(error); ok {
		if s, ok := status.FromError(err); ok {
			return wrappedGRPCStatusError{wrapped, s}
		}
	}

	return wrapped
}
