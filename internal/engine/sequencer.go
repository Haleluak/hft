package engine

import (
	"sync/atomic"
	"time"
)

// Sequencer generates strictly-monotonic 64-bit order IDs using a Snowflake-style
// layout without the Mutex+atomic redundancy of the original design.
//
// Bit layout (64 bits total):
//
//	[63..22] = 42 bits — milliseconds since custom epoch (covers ~139 years)
//	[21..12] = 10 bits — node ID        (up to 1023 nodes)
//	[11..0]  = 12 bits — per-ms sequence (up to 4095 orders / ms / node)
//
// Thread safety: NextID uses only atomic operations — no mutex.
//
// BUG-2 FIX (original issues):
//  1. Original code held a Mutex AND called atomic.AddUint64 — one or the other,
//     never both. Mutex alone is correct here and simpler.
//  2. The sequence counter never reset per millisecond, so after 4095 calls the
//     12-bit field wrapped to zero and collided with IDs from the same millisecond
//     a few seconds earlier.
//  3. No clock-skew guard: time.Now() can go backwards during NTP correction or
//     VM live migration, producing duplicate timestamps and thus duplicate IDs.
//
// This implementation fixes all three by using a single uint64 that encodes both
// the last-seen timestamp and the per-ms counter atomically.
type Sequencer struct {
	nodeID uint64
	epoch  int64

	// state packs [timestamp_ms (52 high bits) | seq (12 low bits)] into one
	// atomic word so both can be read and CAS'd together without a lock.
	state uint64
}

const (
	seqBits  = 12
	seqMask  = (1 << seqBits) - 1 // 0xFFF
	nodeBits = 10
	nodeMask = (1 << nodeBits) - 1 // 0x3FF
	maxSeq   = seqMask             // 4095
)

// NewSequencer creates a Sequencer for a node.
// nodeID must be in [0, 1023].
func NewSequencer(nodeID uint64) *Sequencer {
	return &Sequencer{
		nodeID: nodeID & nodeMask,
		epoch:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli(),
	}
}

// NextID returns a strictly-monotonic 64-bit order ID.
//
// Clock-skew handling: if the system clock goes backwards (NTP, VM migration),
// we continue generating IDs using the last known good timestamp, incrementing
// only the sequence counter. This guarantees monotonicity at the cost of a tiny
// gap between wall-clock time and encoded timestamp — a safe trade-off.
//
// Overflow: if seq exhausts 4095 within a single millisecond (extremely unlikely
// in practice — that's 4M orders/sec), we spin-wait 1ms for the clock to advance.
func (s *Sequencer) NextID() uint64 {
	for {
		old := atomic.LoadUint64(&s.state)
		lastMs := old >> seqBits
		lastSeq := old & seqMask

		nowMs := uint64(max(time.Now().UnixMilli()-s.epoch, 0))

		var newMs, newSeq uint64
		switch {
		case nowMs > lastMs:
			// New millisecond: reset sequence.
			newMs = nowMs
			newSeq = 1
		case nowMs == lastMs:
			// Same millisecond: increment sequence.
			newSeq = lastSeq + 1
			if newSeq > maxSeq {
				// Sequence exhausted — spin until clock advances.
				time.Sleep(time.Millisecond)
				continue
			}
			newMs = lastMs
		default:
			// Clock went backwards: use last known good timestamp, bump seq.
			newSeq = lastSeq + 1
			if newSeq > maxSeq {
				time.Sleep(time.Millisecond)
				continue
			}
			newMs = lastMs
		}

		newState := (newMs << seqBits) | newSeq
		if atomic.CompareAndSwapUint64(&s.state, old, newState) {
			return (newMs << (seqBits + nodeBits)) | (s.nodeID << seqBits) | newSeq
		}
		// CAS failed (concurrent caller) — retry.
	}
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
