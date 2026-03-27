package engine

import (
	"fmt"
	"math"

	"github.com/tidwall/btree"
)

// Match represents a trade execution between two orders.
type Match struct {
	MakerOrderID       uint64 `json:"MakerOrderID,string"`
	TakerOrderID       uint64 `json:"TakerOrderID,string"`
	MakerUserID        uint64 `json:"MakerUserID"`
	TakerUserID        uint64 `json:"TakerUserID"`
	Symbol             string `json:"Symbol"`
	Price              uint64
	Qty                uint64
	TakerSide          Side   `json:"TakerSide"`
	TakerOriginalPrice uint64 `json:"TakerOriginalPrice"`
}

type DepthLevel struct {
	Price uint64 `json:"price"`
	Qty   uint64 `json:"qty"`
}

type DepthSnapshot struct {
	Symbol string       `json:"symbol"`
	Asks   []DepthLevel `json:"asks"`
	Bids   []DepthLevel `json:"bids"`
}

// Orderbook is the core matching engine data structure.
//
// THREAD SAFETY: NOT safe for concurrent use. All access must come from the
// single Engine actor goroutine. The DisruptorQueue is the only safe entry point.
//
// Data layout:
//   - bids / asks    : B-Trees keyed by price for O(log N) best-price lookup.
//   - bidLimits / askLimits : separate maps (BUG-3 fix) so bid and ask at the
//     same price cannot collide. Using one flat map caused incorrect limit
//     deletions when a crossed-book edge case arose.
//   - orders         : O(1) cancel lookup by order ID.
//   - limitPool      : reuses Limit objects to avoid GC pressure on hot path.
//   - matchPool      : reuses the Match slice between AddOrder calls.
type Orderbook struct {
	Symbol    string
	bids      *btree.Map[uint64, *Limit] // Buy orders  (iterated Descend)
	asks      *btree.Map[uint64, *Limit] // Sell orders (iterated Ascend)
	orders    map[uint64]*Order
	bidLimits map[uint64]*Limit // BUG-3 FIX: separate bid limit index
	askLimits map[uint64]*Limit // BUG-3 FIX: separate ask limit index

	// Object pools — zero GC allocation on steady-state hot path
	limitPool *LimitPool
	matchPool *MatchPool
}

func NewOrderbook(symbol string) *Orderbook {
	return &Orderbook{
		Symbol:    symbol,
		bids:      new(btree.Map[uint64, *Limit]),
		asks:      new(btree.Map[uint64, *Limit]),
		orders:    make(map[uint64]*Order),
		bidLimits: make(map[uint64]*Limit),
		askLimits: make(map[uint64]*Limit),
		limitPool: NewLimitPool(1024),
		matchPool: NewMatchPool(64), // BUG-5 FIX: wire the pool
	}
}

// AddOrder processes a new order and returns (matches, stpCanceled, error).
//
// Guarantees:
//   - Order IDs are unique; duplicate returns an error (caller must rollback balance).
//   - Remaining qty > 0 after matching is placed on the resting book.
//   - Returned match slice is borrowed from matchPool; caller must NOT retain it
//     across the next AddOrder call (engine copies it to MatchChan immediately).
func (ob *Orderbook) AddOrder(order *Order) ([]Match, []*Order, error) {
	if _, exists := ob.orders[order.ID]; exists {
		return nil, nil, fmt.Errorf("order %d already exists", order.ID)
	}
	if order.Qty == 0 {
		return nil, nil, fmt.Errorf("order %d has zero qty", order.ID)
	}

	// Post Only check
	if order.PostOnly {
		wouldMatch := false
		if order.Side == Bid {
			ob.asks.Ascend(0, func(askPrice uint64, limit *Limit) bool {
				if askPrice <= order.Price {
					wouldMatch = true
				}
				return false // Just need to peek the best ask
			})
		} else {
			ob.bids.Descend(math.MaxUint64, func(bidPrice uint64, limit *Limit) bool {
				if bidPrice >= order.Price {
					wouldMatch = true
				}
				return false // Just need to peek the best bid
			})
		}
		if wouldMatch {
			// Cancels immediately instead of matching or resting.
			return ob.matchPool.GetSlice(), []*Order{order}, nil
		}
	}

	// Fill Or Kill check
	if order.TimeInForce == FOK {
		var availableQty uint64
		if order.Side == Bid {
			ob.asks.Ascend(0, func(price uint64, limit *Limit) bool {
				if order.Type != MarketOrder && price > order.Price {
					return false // stop
				}
				availableQty += limit.TotalVolume
				return availableQty < order.Qty
			})
		} else {
			ob.bids.Descend(math.MaxUint64, func(price uint64, limit *Limit) bool {
				if order.Type != MarketOrder && price < order.Price {
					return false // stop
				}
				availableQty += limit.TotalVolume
				return availableQty < order.Qty
			})
		}
		if availableQty < order.Qty {
			// Cancel entirely, do not match anything
			return ob.matchPool.GetSlice(), []*Order{order}, nil
		}
	}

	matches := ob.matchPool.GetSlice()
	var stpCanceled []*Order
	var qtyRemaining = order.Qty

	if order.Side == Bid {
		matches, stpCanceled, qtyRemaining = ob.matchWithAsks(order, matches, qtyRemaining)
	} else {
		matches, stpCanceled, qtyRemaining = ob.matchWithBids(order, matches, qtyRemaining)
	}

	order.Qty = qtyRemaining

	// Place unfilled remainder on the resting book.
	if order.Qty > 0 {
		// Market Orders and IOC Orders do NOT rest on the book.
		if order.Type == MarketOrder || order.TimeInForce == IOC || order.TimeInForce == FOK {
			stpCanceled = append(stpCanceled, order) // The remainder is canceled and refunded
		} else {
			ob.orders[order.ID] = order
			ob.addOrderToLimit(order)
		}
	}

	return matches, stpCanceled, nil
}

// matchWithAsks matches an incoming Bid against resting Asks (ascending price).
//
// BUG-1 / ARCH-5 FIX: We track the actual matched qty for each maker order and
// pass it explicitly to removeOrderFromLimit so TotalVolume is decremented by the
// matched amount, NOT by the post-mutation o.Qty.
func (ob *Orderbook) matchWithAsks(taker *Order, matches []Match, qtyToMatch uint64) ([]Match, []*Order, uint64) {
	var stpCanceled []*Order
	var emptyPrices []uint64

	ob.asks.Ascend(0, func(price uint64, limit *Limit) bool {
		if qtyToMatch == 0 {
			return false
		}
		if taker.Type != MarketOrder && price > taker.Price {
			return false // ask price exceeds taker limit — stop
		}

		curr := limit.Head
		for curr != nil && qtyToMatch > 0 {
			// Self-Trade Prevention: cancel the maker (resting) order.
			if curr.UserID == taker.UserID {
				stpCanceled = append(stpCanceled, curr)
				next := curr.Next
				// BUG-1 FIX: pass full remaining qty (pre-mutation) for correct TotalVolume.
				ob.removeOrderFromLimitQty(curr, limit, curr.Qty)
				curr = next
				continue
			}

			matchQty := min(qtyToMatch, curr.Qty)
			matches = append(matches, Match{
				MakerOrderID:       curr.ID,
				TakerOrderID:       taker.ID,
				MakerUserID:        curr.UserID,
				TakerUserID:        taker.UserID,
				Symbol:             ob.Symbol,
				Price:              price, // maker price wins
				Qty:                matchQty,
				TakerSide:          taker.Side,
				TakerOriginalPrice: taker.Price,
			})

			curr.Qty -= matchQty
			qtyToMatch -= matchQty

			if curr.Qty == 0 {
				next := curr.Next
				// BUG-1 FIX: TotalVolume is decremented by matchQty, not the (now-zero) curr.Qty.
				ob.removeOrderFromLimitQty(curr, limit, matchQty)
				curr = next
			} else {
				// Partial fill on maker: update TotalVolume directly.
				limit.TotalVolume -= matchQty
				curr = curr.Next
			}
		}

		if limit.Head == nil {
			emptyPrices = append(emptyPrices, price)
		}
		return true
	})

	ob.pruneEmptyAskLimits(emptyPrices)
	return matches, stpCanceled, qtyToMatch
}

// matchWithBids mirrors matchWithAsks for incoming Ask orders.
func (ob *Orderbook) matchWithBids(taker *Order, matches []Match, qtyToMatch uint64) ([]Match, []*Order, uint64) {
	var stpCanceled []*Order
	var emptyPrices []uint64

	ob.bids.Descend(math.MaxUint64, func(price uint64, limit *Limit) bool {
		if qtyToMatch == 0 {
			return false
		}
		if taker.Type != MarketOrder && price < taker.Price {
			return false // bid price below taker ask — stop
		}

		curr := limit.Head
		for curr != nil && qtyToMatch > 0 {
			if curr.UserID == taker.UserID {
				stpCanceled = append(stpCanceled, curr)
				next := curr.Next
				ob.removeOrderFromLimitQty(curr, limit, curr.Qty)
				curr = next
				continue
			}

			matchQty := min(qtyToMatch, curr.Qty)
			matches = append(matches, Match{
				MakerOrderID:       curr.ID,
				TakerOrderID:       taker.ID,
				MakerUserID:        curr.UserID,
				TakerUserID:        taker.UserID,
				Symbol:             ob.Symbol,
				Price:              price,
				Qty:                matchQty,
				TakerSide:          taker.Side,
				TakerOriginalPrice: taker.Price,
			})

			curr.Qty -= matchQty
			qtyToMatch -= matchQty

			if curr.Qty == 0 {
				next := curr.Next
				ob.removeOrderFromLimitQty(curr, limit, matchQty)
				curr = next
			} else {
				limit.TotalVolume -= matchQty
				curr = curr.Next
			}
		}

		if limit.Head == nil {
			emptyPrices = append(emptyPrices, price)
		}
		return true
	})

	ob.pruneEmptyBidLimits(emptyPrices)
	return matches, stpCanceled, qtyToMatch
}

// removeOrderFromLimitQty removes an order from its limit and decrements
// TotalVolume by the provided qty (which must reflect actual matched volume,
// NOT o.Qty after mutation). Keeps ob.orders consistent.
func (ob *Orderbook) removeOrderFromLimitQty(order *Order, limit *Limit, qty uint64) {
	limit.RemoveOrder(order, qty) // qty-aware removal (BUG-1 fix)
	delete(ob.orders, order.ID)
}

// pruneEmptyAskLimits removes fully-drained ask price levels from the tree and
// recycles their Limit objects. Must be called AFTER the Ascend callback completes
// (modifying the tree during iteration is unsafe with tidwall/btree).
func (ob *Orderbook) pruneEmptyAskLimits(prices []uint64) {
	for _, p := range prices {
		ob.asks.Delete(p)
		if l, ok := ob.askLimits[p]; ok { // BUG-3 FIX: use askLimits, not shared map
			delete(ob.askLimits, p)
			ob.limitPool.Put(l)
		}
	}
}

func (ob *Orderbook) pruneEmptyBidLimits(prices []uint64) {
	for _, p := range prices {
		ob.bids.Delete(p)
		if l, ok := ob.bidLimits[p]; ok { // BUG-3 FIX: use bidLimits
			delete(ob.bidLimits, p)
			ob.limitPool.Put(l)
		}
	}
}

// addOrderToLimit inserts an order into the correct price level, creating the
// level if it does not exist yet.
func (ob *Orderbook) addOrderToLimit(order *Order) {
	if order.Side == Bid {
		limit, exists := ob.bidLimits[order.Price]
		if !exists {
			limit = ob.limitPool.Get(order.Price)
			ob.bidLimits[order.Price] = limit
			ob.bids.Set(order.Price, limit)
		}
		limit.AddOrder(order)
	} else {
		limit, exists := ob.askLimits[order.Price]
		if !exists {
			limit = ob.limitPool.Get(order.Price)
			ob.askLimits[order.Price] = limit
			ob.asks.Set(order.Price, limit)
		}
		limit.AddOrder(order)
	}
}

// CancelOrder removes an order from the book in O(1) time.
// Returns the cancelled order (caller uses it for balance refund).
func (ob *Orderbook) CancelOrder(orderID uint64) (*Order, error) {
	order, exists := ob.orders[orderID]
	if !exists {
		return nil, fmt.Errorf("order %d not found", orderID)
	}

	limit := order.Limit
	// On cancel, the full remaining Qty is being removed from the book.
	ob.removeOrderFromLimitQty(order, limit, order.Qty)

	// Clean up empty limit level.
	if limit.Head == nil {
		if order.Side == Bid {
			ob.bids.Delete(limit.Price)
			delete(ob.bidLimits, limit.Price)
		} else {
			ob.asks.Delete(limit.Price)
			delete(ob.askLimits, limit.Price)
		}
		ob.limitPool.Put(limit)
	}

	return order, nil
}

// ReturnMatchSlice recycles the matches slice back to the pool.
// The Engine actor calls this after copying matches to MatchChan.
func (ob *Orderbook) ReturnMatchSlice(s []Match) {
	ob.matchPool.PutSlice(s)
}

// Snapshot creates a point-in-time copy of all open orders.
// Price-time priority order is preserved for deterministic WAL recovery.
func (ob *Orderbook) Snapshot() *Snapshot {
	var orders []*Order

	ob.bids.Descend(math.MaxUint64, func(_ uint64, limit *Limit) bool {
		curr := limit.Head
		for curr != nil {
			cpy := *curr
			cpy.Prev = nil
			cpy.Next = nil
			cpy.Limit = nil
			orders = append(orders, &cpy)
			curr = curr.Next
		}
		return true
	})

	ob.asks.Ascend(0, func(_ uint64, limit *Limit) bool {
		curr := limit.Head
		for curr != nil {
			cpy := *curr
			cpy.Prev = nil
			cpy.Next = nil
			cpy.Limit = nil
			orders = append(orders, &cpy)
			curr = curr.Next
		}
		return true
	})

	return &Snapshot{Symbol: ob.Symbol, Orders: orders}
}

// GetDepth returns up to `levels` price levels on each side.
func (ob *Orderbook) GetDepth(levels int) *DepthSnapshot {
	bids := make([]DepthLevel, 0, levels)
	asks := make([]DepthLevel, 0, levels)

	ob.bids.Descend(math.MaxUint64, func(price uint64, limit *Limit) bool {
		bids = append(bids, DepthLevel{Price: price, Qty: limit.TotalVolume})
		return len(bids) < levels
	})

	ob.asks.Ascend(0, func(price uint64, limit *Limit) bool {
		asks = append(asks, DepthLevel{Price: price, Qty: limit.TotalVolume})
		return len(asks) < levels
	})

	return &DepthSnapshot{Symbol: ob.Symbol, Asks: asks, Bids: bids}
}

func min(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
