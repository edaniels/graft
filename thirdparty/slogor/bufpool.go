package slogor

import "sync"

const (
	// initialBufferSize is the default capacity for newly allocated log buffers.
	// Most log messages will fit within this size, avoiding repeated reallocations.
	initialBufferSize = 1 << 10

	// maxBufferSize is the maximum buffer capacity allowed in the pool.
	// Buffers larger than this are discarded to avoid holding large, rarely used memory
	// and prevent memory bloat in long-running programs.
	maxBufferSize = 16 << 10
)

// bufPool is a sync.Pool that holds pointers to reusable byte slices.
// Each buffer is used for formatting log messages before writing to the final output.
// Using pointers to slices avoids small allocations when boxing/unboxing interface{} in sync.Pool.
// The pool helps reduce garbage collection overhead in high-frequency logging scenarios.
var bufPool = sync.Pool{
	// New allocates a new byte slice with length 0 and initialBufferSize capacity.
	// This is only called if the pool is empty.
	New: func() any {
		// We create a local variable so we can take its pointer.
		// Taking the address of a composite literal directly is not allowed in Go.
		b := make([]byte, 0, initialBufferSize)
		return &b
	},
}

// allocBuf retrieves a buffer from the pool and returns a pointer to it.
//
// Usage pattern:
//   bufp := allocBuf()
//   buf := *bufp
//   ... use buf ...
//   *bufp = buf
//   freeBuf(bufp)
//
// This function guarantees that the returned slice is always length 0.
// The underlying array may have capacity initialBufferSize or larger if it was previously used.
func allocBuf() *[]byte {
	return bufPool.Get().(*[]byte)
}

// freeBuf truncates the buffer to length 0 and returns it to the pool.
//
// Behavior notes:
// 1. Oversized buffers are discarded (cap(*b) > maxBufferSize). This prevents
//    holding large slices indefinitely, which can cause memory bloat in long-running applications.
// 2. Truncating the slice in-place keeps the underlying array for reuse,
//    minimizing allocations and reducing GC pressure.
// 3. Using a pointer to a slice (*[]byte) avoids interface allocations that
//    would occur if we stored []byte directly in the sync.Pool.
// 4. This function does not shrink the underlying array beyond truncation;
//    extremely large slices are simply discarded.
func freeBuf(b *[]byte) {
	if cap(*b) <= maxBufferSize {
		// Reset the length to zero; capacity is unchanged.
		*b = (*b)[:0]

		// Return the slice pointer to the pool for future reuse.
		bufPool.Put(b)
	}
}
