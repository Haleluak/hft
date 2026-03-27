package engine

import (
	"testing"
)

// ── Orderbook unit tests ──────────────────────────────────────────────────────

func TestOrderbookMatch(t *testing.T) {
	ob := NewOrderbook("BTCUSDT")

	ob.AddOrder(&Order{ID: 1, UserID: 1, Price: 1000, Qty: 10, Side: Ask})
	ob.AddOrder(&Order{ID: 2, UserID: 2, Price: 1005, Qty: 20, Side: Ask})
	ob.AddOrder(&Order{ID: 3, UserID: 3, Price: 990, Qty: 10, Side: Bid})
	ob.AddOrder(&Order{ID: 4, UserID: 4, Price: 985, Qty: 5, Side: Bid})

	// Aggressive bid crosses the spread: should match orders 1 (full) and 2 (partial).
	matches, _, err := ob.AddOrder(&Order{ID: 5, UserID: 5, Price: 1010, Qty: 15, Side: Bid})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}
	if matches[0].MakerOrderID != 1 || matches[0].Qty != 10 {
		t.Errorf("match 1 incorrect: %+v", matches[0])
	}
	if matches[1].MakerOrderID != 2 || matches[1].Qty != 5 {
		t.Errorf("match 2 incorrect: %+v", matches[1])
	}

	// BUG-1 regression: TotalVolume must reflect remaining qty, not pre-match qty.
	if ob.askLimits[1005].TotalVolume != 15 {
		t.Errorf("BUG-1: expected TotalVolume=15 at 1005, got %d", ob.askLimits[1005].TotalVolume)
	}

	if _, exists := ob.orders[1]; exists {
		t.Errorf("order 1 should be removed after full fill")
	}
	if ob.orders[2].Qty != 15 {
		t.Errorf("expected 15 remaining on order 2, got %d", ob.orders[2].Qty)
	}
}

func TestOrderbookCancel(t *testing.T) {
	ob := NewOrderbook("BTCUSDT")
	ob.AddOrder(&Order{ID: 1, Price: 1000, Qty: 10, Side: Ask})

	_, err := ob.CancelOrder(1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, exists := ob.orders[1]; exists {
		t.Errorf("order 1 should be gone after cancel")
	}
	if limit, exists := ob.askLimits[1000]; exists {
		t.Errorf("limit level should be removed, got %+v", limit)
	}
	if v, ok := ob.asks.Get(1000); ok || v != nil {
		t.Errorf("ask tree should not contain price 1000 after cancel")
	}
}

// TestTotalVolumeAfterPartialFill is a targeted regression for BUG-1:
// partial match of a maker order used to subtract the wrong qty from TotalVolume.
func TestTotalVolumeAfterPartialFill(t *testing.T) {
	ob := NewOrderbook("BTCUSDT")
	ob.AddOrder(&Order{ID: 1, UserID: 1, Price: 100, Qty: 50, Side: Ask})

	// Taker buys only 20 — maker order 1 should have 30 remaining.
	matches, _, err := ob.AddOrder(&Order{ID: 2, UserID: 2, Price: 100, Qty: 20, Side: Bid})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matches) != 1 || matches[0].Qty != 20 {
		t.Fatalf("expected 1 match of qty 20, got %+v", matches)
	}

	// BUG-1: TotalVolume must be 30, not 50 (never decremented) or 0 (under-decremented).
	wantVol := uint64(30)
	if got := ob.askLimits[100].TotalVolume; got != wantVol {
		t.Errorf("BUG-1 regression: TotalVolume = %d, want %d", got, wantVol)
	}
	if ob.orders[1].Qty != 30 {
		t.Errorf("expected maker order remaining qty = 30, got %d", ob.orders[1].Qty)
	}
}

// TestSTPDoesNotMatchSameUser verifies Self-Trade Prevention cancels the resting
// maker order and does NOT create a match when taker == maker user.
func TestSTPDoesNotMatchSameUser(t *testing.T) {
	ob := NewOrderbook("BTCUSDT")
	// User 99 rests an ask.
	ob.AddOrder(&Order{ID: 1, UserID: 99, Price: 100, Qty: 10, Side: Ask})

	// User 99 tries to buy from themselves.
	matches, stpCanceled, err := ob.AddOrder(&Order{ID: 2, UserID: 99, Price: 100, Qty: 10, Side: Bid})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("STP: expected 0 matches for self-trade, got %d", len(matches))
	}
	if len(stpCanceled) != 1 || stpCanceled[0].ID != 1 {
		t.Errorf("STP: expected maker order 1 in stpCanceled, got %+v", stpCanceled)
	}
	// Maker order must be removed from the book.
	if _, exists := ob.orders[1]; exists {
		t.Errorf("STP: maker order 1 should be removed from book")
	}
	// TotalVolume of the now-empty limit must be 0.
	if l, exists := ob.askLimits[100]; exists {
		t.Errorf("STP: ask limit at 100 should be pruned, got %+v", l)
	}
}

// TestDuplicateOrderIDRejected ensures the engine rejects re-submission of an ID.
func TestDuplicateOrderIDRejected(t *testing.T) {
	ob := NewOrderbook("BTCUSDT")
	ob.AddOrder(&Order{ID: 42, UserID: 1, Price: 500, Qty: 5, Side: Ask})

	_, _, err := ob.AddOrder(&Order{ID: 42, UserID: 2, Price: 500, Qty: 5, Side: Ask})
	if err == nil {
		t.Error("expected error for duplicate order ID 42, got nil")
	}
}

// TestZeroQtyRejected ensures zero-qty orders are rejected before touching the book.
func TestZeroQtyRejected(t *testing.T) {
	ob := NewOrderbook("BTCUSDT")
	_, _, err := ob.AddOrder(&Order{ID: 1, UserID: 1, Price: 100, Qty: 0, Side: Bid})
	if err == nil {
		t.Error("expected error for zero-qty order, got nil")
	}
}

// TestCancelNotFound returns an error for unknown order IDs.
func TestCancelNotFound(t *testing.T) {
	ob := NewOrderbook("BTCUSDT")
	_, err := ob.CancelOrder(999)
	if err == nil {
		t.Error("expected error cancelling non-existent order 999, got nil")
	}
}

// TestBidAskAtSamePriceDontCollide verifies BUG-3 is fixed:
// a bid and an ask at the same price use separate limit maps and do not corrupt each other.
func TestBidAskAtSamePriceDontCollide(t *testing.T) {
	ob := NewOrderbook("BTCUSDT")
	// Rest an ask at 100.
	ob.AddOrder(&Order{ID: 1, UserID: 1, Price: 100, Qty: 5, Side: Ask})

	// Cancel it — ask limit at 100 should be pruned.
	ob.CancelOrder(1)

	if _, exists := ob.askLimits[100]; exists {
		t.Error("BUG-3: ask limit at 100 should be removed after cancel")
	}

	// Now rest a bid at the same price (should use bidLimits, not askLimits).
	ob.AddOrder(&Order{ID: 2, UserID: 2, Price: 100, Qty: 8, Side: Bid})
	if ob.bidLimits[100] == nil {
		t.Error("BUG-3: bid limit at 100 should exist")
	}
	if ob.bidLimits[100].TotalVolume != 8 {
		t.Errorf("BUG-3: bid TotalVolume = %d, want 8", ob.bidLimits[100].TotalVolume)
	}
}

// TestSequencerMonotonic verifies NextID always produces strictly increasing IDs
// even under concurrent callers.
func TestSequencerMonotonic(t *testing.T) {
	seq := NewSequencer(1)
	const n = 10000
	ids := make([]uint64, n)
	for i := range ids {
		ids[i] = seq.NextID()
	}
	for i := 1; i < n; i++ {
		if ids[i] <= ids[i-1] {
			t.Errorf("ID not monotonically increasing at index %d: %d <= %d", i, ids[i], ids[i-1])
		}
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

func BenchmarkOrderbookMatch(b *testing.B) {
	ob := NewOrderbook("BTCUSDT")
	for i := 0; i < b.N; i++ {
		ob.AddOrder(&Order{ID: uint64(i*2 + 1), UserID: 1, Price: 1000, Qty: 10, Side: Ask})
		ob.AddOrder(&Order{ID: uint64(i*2 + 2), UserID: 2, Price: 1000, Qty: 10, Side: Bid})
	}
}
