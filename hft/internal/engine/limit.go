package engine

// Limit represents a price level in the orderbook.
// It is a doubly-linked list of orders at a single price, kept in FIFO
// (price-time priority) order. All mutations happen inside the single Engine
// actor goroutine — no locks needed.
type Limit struct {
	Price       uint64
	TotalVolume uint64
	Head        *Order
	Tail        *Order
}

func NewLimit(price uint64) *Limit {
	return &Limit{Price: price}
}

// AddOrder appends an order to the tail of the queue (FIFO / price-time priority).
func (l *Limit) AddOrder(o *Order) {
	o.Limit = l
	if l.Head == nil {
		l.Head = o
		l.Tail = o
	} else {
		l.Tail.Next = o
		o.Prev = l.Tail
		l.Tail = o
	}
	l.TotalVolume += o.Qty
}

// RemoveOrder removes an order from the doubly-linked list in O(1).
//
// BUG-1 / ARCH-5 FIX: The caller must supply `volumeDelta` — the actual qty
// to subtract from TotalVolume. This is necessary because `o.Qty` may have
// already been mutated by the matching loop before RemoveOrder is called (e.g.
// after a partial fill reduced o.Qty to zero). Passing the pre-mutation matchQty
// directly avoids the TotalVolume drift bug.
func (l *Limit) RemoveOrder(o *Order, volumeDelta uint64) {
	if o.Prev != nil {
		o.Prev.Next = o.Next
	} else {
		l.Head = o.Next // o was the head
	}

	if o.Next != nil {
		o.Next.Prev = o.Prev
	} else {
		l.Tail = o.Prev // o was the tail
	}

	// Clear intrusive pointers so the Order struct can be safely returned to
	// the caller or GC'd without retaining references into the limit chain.
	o.Next = nil
	o.Prev = nil
	o.Limit = nil

	// Guard against underflow (should never happen in correct code, but defensive).
	if volumeDelta > l.TotalVolume {
		l.TotalVolume = 0
	} else {
		l.TotalVolume -= volumeDelta
	}
}
