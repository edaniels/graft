package graft

import (
	"io"
	"testing"
	"time"

	"go.viam.com/test"

	"github.com/edaniels/graft/errors"
)

func TestOutputRingReadFromStart(t *testing.T) {
	ring := newOutputRing(64)

	n, err := ring.Write([]byte("hello"))
	test.That(t, err, test.ShouldBeNil)
	test.That(t, n, test.ShouldEqual, 5)

	data, gotOffset, err := ring.ReadAt(0, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, gotOffset, test.ShouldEqual, 0)
	test.That(t, string(data), test.ShouldEqual, "hello")
	test.That(t, ring.EndOffset(), test.ShouldEqual, 5)
}

func TestOutputRingSequentialReads(t *testing.T) {
	ring := newOutputRing(64)

	_, err := ring.Write([]byte("abc"))
	test.That(t, err, test.ShouldBeNil)

	data, gotOffset, err := ring.ReadAt(0, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, gotOffset, test.ShouldEqual, 0)

	next := gotOffset + uint64(len(data))

	_, err = ring.Write([]byte("def"))
	test.That(t, err, test.ShouldBeNil)

	data, gotOffset, err = ring.ReadAt(next, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, gotOffset, test.ShouldEqual, 3)
	test.That(t, string(data), test.ShouldEqual, "def")
}

func TestOutputRingBlocksUntilWrite(t *testing.T) {
	ring := newOutputRing(64)

	type result struct {
		data []byte
		err  error
	}

	resultCh := make(chan result, 1)

	go func() {
		data, _, err := ring.ReadAt(0, nil)
		resultCh <- result{data, err}
	}()

	_, err := ring.Write([]byte("later"))
	test.That(t, err, test.ShouldBeNil)

	res := <-resultCh
	test.That(t, res.err, test.ShouldBeNil)
	test.That(t, string(res.data), test.ShouldEqual, "later")
}

func TestOutputRingEviction(t *testing.T) {
	ring := newOutputRing(8)

	_, err := ring.Write([]byte("0123456789ab")) // 12 bytes into capacity 8
	test.That(t, err, test.ShouldBeNil)

	test.That(t, ring.StartOffset(), test.ShouldEqual, 4)
	test.That(t, ring.EndOffset(), test.ShouldEqual, 12)

	// A read at an evicted offset starts at the earliest retained byte.
	data, gotOffset, err := ring.ReadAt(0, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, gotOffset, test.ShouldEqual, 4)
	test.That(t, string(data), test.ShouldEqual, "456789ab")
}

func TestOutputRingWriteLargerThanCapacity(t *testing.T) {
	ring := newOutputRing(4)

	_, err := ring.Write([]byte("0123456789"))
	test.That(t, err, test.ShouldBeNil)

	data, gotOffset, err := ring.ReadAt(0, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, gotOffset, test.ShouldEqual, 6)
	test.That(t, string(data), test.ShouldEqual, "6789")
}

func TestOutputRingCloseGivesEOF(t *testing.T) {
	ring := newOutputRing(64)

	_, err := ring.Write([]byte("bye"))
	test.That(t, err, test.ShouldBeNil)

	ring.Close()

	data, _, err := ring.ReadAt(0, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, string(data), test.ShouldEqual, "bye")

	_, _, err = ring.ReadAt(3, nil)
	test.That(t, err, test.ShouldEqual, io.EOF)
}

func TestOutputRingCloseWakesBlockedReader(t *testing.T) {
	ring := newOutputRing(64)

	errCh := make(chan error, 1)

	go func() {
		_, _, err := ring.ReadAt(0, nil)
		errCh <- err
	}()

	ring.Close()

	err := <-errCh
	test.That(t, err, test.ShouldEqual, io.EOF)
}

func TestOutputRingCancelUnblocksReader(t *testing.T) {
	ring := newOutputRing(64)

	cancel := make(chan struct{})
	errCh := make(chan error, 1)

	go func() {
		_, _, err := ring.ReadAt(0, cancel)
		errCh <- err
	}()

	close(cancel)

	err := <-errCh
	test.That(t, errors.Is(err, errOutputRingReadCanceled), test.ShouldBeTrue)
}

func TestOutputRingWriteAfterCloseFails(t *testing.T) {
	ring := newOutputRing(64)
	ring.Close()

	_, err := ring.Write([]byte("nope"))
	test.That(t, err, test.ShouldNotBeNil)
}

func TestOutputRingLosslessBlocksInsteadOfEvicting(t *testing.T) {
	ring := newOutputRing(8)
	ring.setLossless()

	_, err := ring.Write([]byte("01234567"))
	test.That(t, err, test.ShouldBeNil)

	// The ring is full of unconfirmed data: another write must block rather
	// than evict.
	wrote := make(chan error, 1)

	go func() {
		_, writeErr := ring.Write([]byte("abcd"))
		wrote <- writeErr
	}()

	select {
	case <-wrote:
		t.Fatal("lossless write completed by evicting unconfirmed data")
	case <-time.After(50 * time.Millisecond):
	}

	// Confirming the first four bytes releases exactly that much space.
	ring.setReleased(4)

	select {
	case writeErr := <-wrote:
		test.That(t, writeErr, test.ShouldBeNil)
	case <-time.After(5 * time.Second):
		t.Fatal("write did not unblock after release")
	}

	// Nothing unconfirmed was lost: offsets 4.. are all present.
	data, gotOffset, err := ring.ReadAt(4, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, gotOffset, test.ShouldEqual, 4)
	test.That(t, string(data), test.ShouldEqual, "4567abcd")
}

func TestOutputRingLosslessWriteLargerThanCapacity(t *testing.T) {
	ring := newOutputRing(4)
	ring.setLossless()

	// A writer larger than capacity completes in pieces as the reader
	// confirms consumption.
	wrote := make(chan error, 1)

	go func() {
		_, writeErr := ring.Write([]byte("0123456789"))
		wrote <- writeErr
	}()

	var got []byte

	offset := uint64(0)

	for len(got) < 10 {
		data, gotOffset, err := ring.ReadAt(offset, nil)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, gotOffset, test.ShouldEqual, offset)

		got = append(got, data...)
		offset += uint64(len(data))
		ring.setReleased(offset)
	}

	select {
	case writeErr := <-wrote:
		test.That(t, writeErr, test.ShouldBeNil)
	case <-time.After(5 * time.Second):
		t.Fatal("large lossless write never completed")
	}

	test.That(t, string(got), test.ShouldEqual, "0123456789")
}

func TestOutputRingLosslessCloseUnblocksWriter(t *testing.T) {
	ring := newOutputRing(4)
	ring.setLossless()

	_, err := ring.Write([]byte("full"))
	test.That(t, err, test.ShouldBeNil)

	wrote := make(chan error, 1)

	go func() {
		_, writeErr := ring.Write([]byte("more"))
		wrote <- writeErr
	}()

	ring.Close()

	select {
	case writeErr := <-wrote:
		test.That(t, writeErr, test.ShouldNotBeNil)
	case <-time.After(5 * time.Second):
		t.Fatal("close did not unblock the blocked writer")
	}
}

func TestOutputRingReleaseAfterCloseIsSafe(t *testing.T) {
	ring := newOutputRing(4)
	ring.setLossless()

	_, err := ring.Write([]byte("data"))
	test.That(t, err, test.ShouldBeNil)

	ring.Close()

	// A client confirming the final bytes after the ring closed (it just read
	// to EOF) must be a no-op, not a panic.
	ring.setReleased(4)
	ring.setReleased(8)
}
