package errors_test

import (
	"testing"

	"go.viam.com/test"

	"github.com/edaniels/graft/errors"
)

type nilPointerError struct{}

func (e *nilPointerError) Error() string { return "boom" }

// A nil passed into the wrappers must come back as an untyped nil error, not
// a typed-nil *Error inside a non-nil interface. A typed nil escapes every
// `err != nil` guard and then panics the first time something walks its
// Unwrap chain (e.g. grpc's status.FromError); see the `graft ps` crash.
func TestWrapNilReturnsUntypedNil(t *testing.T) {
	test.That(t, errors.Wrap(nil) == nil, test.ShouldBeTrue)

	var typedNil *nilPointerError

	test.That(t, errors.Wrap(typedNil) == nil, test.ShouldBeTrue)
}

func TestWrapPrefixNilReturnsUntypedNil(t *testing.T) {
	test.That(t, errors.WrapPrefix(nil, "prefix") == nil, test.ShouldBeTrue)

	var typedNil *nilPointerError

	test.That(t, errors.WrapPrefix(typedNil, "prefix") == nil, test.ShouldBeTrue)
}

func TestWrapSuffixNilReturnsUntypedNil(t *testing.T) {
	test.That(t, errors.WrapSuffix(nil, "suffix") == nil, test.ShouldBeTrue)
}

func TestWrapNonNilStillWraps(t *testing.T) {
	err := errors.Wrap(errors.NewBare("real problem"))
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "real problem")

	err = errors.WrapPrefix(errors.NewBare("real problem"), "context")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "context")
}
