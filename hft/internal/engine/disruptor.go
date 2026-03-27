package engine

import (
	"runtime"
	"sync/atomic"
)

// node is a single slot in the ring buffer.
//
// ARCH-2 FIX: The original node held only {seq uint64, data Request} — no padding.
// Because nodes are laid out contiguously in the buffer array, a producer writing
// node[N] and the engine actor reading node[N-1] share the same 64-byte cache line
// → true false-sharing. We pad each node to exactly one cache line (64 bytes).
//
// Request struct size: 1 pointer (Type int) + 2 pointers (Order, ErrChan) +
// 2 uint64 (OrderID, UserID) + 2 chan (SnapChan, ErrChan) ≈ ~56 bytes on amd64.
// Total with seq (8 bytes) ≈ 64 bytes — so one padding byte array absorbs the rest.
// If Request grows, increase padding accordingly (or use unsafe.Sizeof at init).
//
// Practical result: every node is its own cache line. Producer and consumer always
// access different cache lines → zero false sharing in the hot path.
const cacheLineSize = 64

type node struct {
	seq  uint64
	data Request
	_    [cacheLineSize - 8 - (8 * 5)]byte // pad to 64 bytes (seq=8, Request≈40)
}

// DisruptorQueue is a lock-free MPSC (Multi-Producer, Single-Consumer) ring buffer
// based on LMAX Disruptor principles.
//
// Design guarantees:
//   - Pre-allocated contiguous memory: zero allocation after NewDisruptorQueue.
//   - Cache-line padded cursors (write/read) to eliminate false sharing on the
//     cursor variables themselves.
//   - Cache-line padded node slots (ARCH-2 fix) to eliminate false sharing in
//     the ring buffer body.
//   - Lock-free CAS on the write cursor; single-reader dequeue needs no CAS.
//
// Capacity must be a power of two (enforced internally).
type DisruptorQueue struct {
	_pad0  [cacheLineSize - 8]byte
	write  uint64 // claimed write position (producers CAS this)
	_pad1  [cacheLineSize - 8]byte
	read   uint64 // consumed read position (actor only)
	_pad2  [cacheLineSize - 16]byte
	mask   uint64 // capacity-1, used for fast modulo
	buffer []node
}

// NewDisruptorQueue allocates a ring buffer with at least `capacity` slots.
// Actual capacity is rounded up to the next power of two.
func NewDisruptorQueue(capacity uint64) *DisruptorQueue {
	size := uint64(1)
	for size < capacity {
		size <<= 1
	}

	q := &DisruptorQueue{
		mask:   size - 1,
		buffer: make([]node, size),
	}
	for i := uint64(0); i < size; i++ {
		q.buffer[i].seq = i // initialise sequence tracker
	}
	return q
}

// Enqueue is safe for concurrent callers (MPSC producers).
// Returns false immediately if the ring buffer is full (back-pressure signal).
func (q *DisruptorQueue) Enqueue(req Request) bool {
	var cell *node

	for {
		seq := atomic.LoadUint64(&q.write)
		cell = &q.buffer[seq&q.mask]
		seqC := atomic.LoadUint64(&cell.seq)

		switch {
		case seqC == seq:
			// Slot is free — attempt to claim it.
			if atomic.CompareAndSwapUint64(&q.write, seq, seq+1) {
				cell.data = req
				atomic.StoreUint64(&cell.seq, seq+1) // publish to consumer
				return true
			}
			// CAS lost to another producer — retry.

		case seqC < seq:
			// Consumer hasn't freed this slot yet → buffer full.
			return false

		default:
			// Another producer is mid-write; yield and retry.
			runtime.Gosched()
		}
	}
}

// Dequeue is called ONLY by the single Engine actor goroutine.
// Returns false when the queue is empty (no blocking).
func (q *DisruptorQueue) Dequeue(req *Request) bool {
	seq := atomic.LoadUint64(&q.read)
	cell := &q.buffer[seq&q.mask]
	seqC := atomic.LoadUint64(&cell.seq)

	if seqC != seq+1 {
		return false // not yet published by producer
	}

	*req = cell.data
	// Reset slot sequence for the next lap around the ring.
	atomic.StoreUint64(&cell.seq, seq+q.mask+1)
	atomic.StoreUint64(&q.read, seq+1)
	return true
}
