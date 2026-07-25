package graft

import (
	"io"
	"sync"

	"github.com/edaniels/graft/errors"
)

var errOutputRingReadCanceled = errors.NewBare("output ring read canceled")

// outputRing is a bounded, append-only byte buffer addressed by absolute
// offsets that supports blocking tail reads. It retains the most recent
// capacity bytes; the absolute offset of the first retained byte advances as
// older data is evicted. It is what keeps a command's output continuously
// drained (so the process never blocks on a full pty/pipe) while allowing a
// client to attach later and replay from an offset.
type outputRing struct {
	mu       sync.Mutex
	buf      []byte // retained bytes; buf[0] is at absolute offset start
	start    uint64 // absolute offset of buf[0]
	capacity int
	closed   bool
	notify   chan struct{} // closed and replaced on each write/close

	// lossless switches Write from evict-oldest to TCP-like backpressure:
	// only bytes the consumer confirmed (below released) may be evicted, and
	// a Write that cannot make progress blocks until space is released or the
	// ring closes.
	lossless bool
	released uint64        // absolute offset below which data is confirmed consumed
	space    chan struct{} // closed and replaced on release/close to wake writers
}

func newOutputRing(capacity int) *outputRing {
	return &outputRing{
		capacity: capacity,
		notify:   make(chan struct{}),
		space:    make(chan struct{}),
	}
}

// setLossless enables backpressure mode. Must be called before any writes.
func (r *outputRing) setLossless() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.lossless = true
}

// setReleased marks all output below the absolute offset as confirmed
// consumed, allowing lossless writers to reuse that space.
func (r *outputRing) setReleased(offset uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if offset <= r.released {
		return
	}

	r.released = offset

	if r.closed {
		// Close already released the space channel; late acks (e.g. a client
		// confirming the final bytes after EOF) have nothing to wake.
		return
	}

	close(r.space)
	r.space = make(chan struct{})
}

// Write appends p. In the default mode the oldest bytes beyond capacity are
// evicted; in lossless mode only released bytes may be evicted and the write
// blocks until it can complete without losing unconfirmed data.
func (r *outputRing) Write(p []byte) (int, error) {
	if !r.lossless {
		return r.writeEvicting(p)
	}

	return r.writeLossless(p)
}

func (r *outputRing) writeEvicting(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return 0, errors.New("output ring is closed")
	}

	if len(p) >= r.capacity {
		r.start += uint64(len(r.buf)) + uint64(len(p)-r.capacity) //nolint:gosec // non-negative by the branch condition
		r.buf = append(r.buf[:0], p[len(p)-r.capacity:]...)
	} else {
		r.buf = append(r.buf, p...)
		if over := len(r.buf) - r.capacity; over > 0 {
			r.start += uint64(over)
			r.buf = append(r.buf[:0], r.buf[over:]...)
		}
	}

	close(r.notify)
	r.notify = make(chan struct{})

	return len(p), nil
}

func (r *outputRing) writeLossless(p []byte) (int, error) {
	written := 0

	for written < len(p) {
		r.mu.Lock()

		if r.closed {
			r.mu.Unlock()

			return written, errors.New("output ring is closed")
		}

		// Free space plus whatever confirmed data may be evicted.
		evictable := 0
		if r.released > r.start {
			evictable = min(int(r.released-r.start), len(r.buf)) //nolint:gosec // bounded by len(buf)
		}

		space := r.capacity - len(r.buf) + evictable
		if space <= 0 {
			wait := r.space
			r.mu.Unlock()

			<-wait

			continue
		}

		n := min(space, len(p)-written)

		if over := len(r.buf) + n - r.capacity; over > 0 {
			r.start += uint64(over)
			r.buf = append(r.buf[:0], r.buf[over:]...)
		}

		r.buf = append(r.buf, p[written:written+n]...)
		written += n

		close(r.notify)
		r.notify = make(chan struct{})
		r.mu.Unlock()
	}

	return written, nil
}

// Close marks the ring as complete; blocked and future end-of-data reads
// return io.EOF.
func (r *outputRing) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}

	r.closed = true

	close(r.notify)
	close(r.space)
}

// StartOffset returns the absolute offset of the earliest retained byte.
func (r *outputRing) StartOffset() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.start
}

// EndOffset returns the absolute offset one past the last written byte.
func (r *outputRing) EndOffset() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.start + uint64(len(r.buf))
}

// ReadAt returns a copy of the data available at the given absolute offset. If
// that offset has been evicted, reading begins at the earliest retained byte
// and gotOffset reports where the returned data actually starts. When no data
// is available at the offset, ReadAt blocks until data is written, the ring is
// closed (io.EOF), or cancel is closed (errOutputRingReadCanceled).
func (r *outputRing) ReadAt(offset uint64, cancel <-chan struct{}) ([]byte, uint64, error) {
	for {
		r.mu.Lock()

		if offset < r.start {
			offset = r.start
		}

		if offset < r.start+uint64(len(r.buf)) {
			data := make([]byte, r.start+uint64(len(r.buf))-offset)
			copy(data, r.buf[offset-r.start:])
			r.mu.Unlock()

			return data, offset, nil
		}

		if r.closed {
			r.mu.Unlock()

			return nil, offset, io.EOF
		}

		notify := r.notify
		r.mu.Unlock()

		select {
		case <-notify:
		case <-cancel:
			return nil, offset, errors.Wrap(errOutputRingReadCanceled)
		}
	}
}
