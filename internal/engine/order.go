package engine

type Side int

const (
	Bid Side = iota // Buy
	Ask             // Sell
)

type OrderType string

const (
	LimitOrder  OrderType = "LIMIT"
	MarketOrder OrderType = "MARKET"
)

type TimeInForce string

const (
	GTC TimeInForce = "GTC" // Good Til Cancelled (Default)
	IOC TimeInForce = "IOC" // Immediate Or Cancel
	FOK TimeInForce = "FOK" // Fill Or Kill
)

// Order represents a single order in the orderbook
type Order struct {
	ID          uint64      `json:"id,string"`
	UserID      uint64      `json:"user_id"`
	Symbol      string      `json:"symbol"`
	Price       uint64      `json:"price"` // 0 for MarketOrder
	Qty         uint64      `json:"qty"`   // Remaining Qty
	InitialQty  uint64      `json:"initial_qty"`
	Side        Side        `json:"side"`
	Type        OrderType   `json:"type"`
	TimeInForce TimeInForce `json:"time_in_force"`
	PostOnly    bool        `json:"post_only"`
	Status      string      `json:"status"` // "OPEN", "FILLED", "CANCELED"
	Timestamp   int64       `json:"timestamp"`

	// Intrusive linked list pointers for O(1) removal from PriceLevel
	Prev  *Order `json:"-"`
	Next  *Order `json:"-"`
	Limit *Limit `json:"-"` // back-pointer to the limit price level
}
