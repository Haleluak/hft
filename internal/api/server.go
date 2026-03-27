package api

import (
	"context"
	"exchange/internal/engine"
	"exchange/internal/risk"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"exchange/internal/ws"

	"github.com/gin-gonic/gin"
)

// Server is the API Gateway. It now delegates order management to the Router
// which handles Sharding, Risk Check, ID Assignment, and WAL logging.
type Server struct {
	Router      *engine.Router
	RiskEngine  *risk.RiskEngine
	Broadcaster *ws.Broadcaster
}

// OrderRequest payload mapped from Client App
type OrderRequest struct {
	UserID      uint64 `json:"user_id" binding:"required"`
	Price       uint64 `json:"price"` // Not required for MARKET
	Qty         uint64 `json:"qty" binding:"required"`
	Side        string `json:"side" binding:"required,oneof=buy sell"`
	Symbol      string `json:"symbol" binding:"required"` // e.g. "BTC_USDT"
	Type        string `json:"type" binding:"omitempty,oneof=LIMIT MARKET"`
	TimeInForce string `json:"time_in_force" binding:"omitempty,oneof=GTC IOC FOK"`
	PostOnly    bool   `json:"post_only"`
}

type CancelRequest struct {
	Symbol  string `json:"symbol" binding:"required"`
	OrderID uint64 `json:"order_id,string" binding:"required"`
}

// Option defines functional options for configuring the Server.
type Option func(*Server)

// WithRouter sets the Engine shared Router.
func WithRouter(r *engine.Router) Option {
	return func(s *Server) {
		s.Router = r
	}
}

// WithRiskEngine sets the pre-trade Risk Engine.
func WithRiskEngine(re *risk.RiskEngine) Option {
	return func(s *Server) {
		s.RiskEngine = re
	}
}

// WithBroadcaster sets the WebSocket broadcasting manager.
func WithBroadcaster(b *ws.Broadcaster) Option {
	return func(s *Server) {
		s.Broadcaster = b
	}
}

// NewServer constructs a new API Gateway configured via functional options.
func NewServer(opts ...Option) *Server {
	s := &Server{}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Start spawns the API Gateway
func (s *Server) Start(port string) error {
	// Gin is configured with release mode for speed in production
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()

	r.Use(gin.Recovery())

	// Public Routes: UI and WebSocket
	r.Static("/ui", "./public")
	r.GET("/ws/market", func(c *gin.Context) {
		s.Broadcaster.HandleWS(c.Writer, c.Request)
	})

	// Protected API Group
	api := r.Group("/")
	api.Use(func(c *gin.Context) {
		// Exempt the root/NoRoute if we want it to serve index.html without auth
		if c.Request.URL.Path == "/" || c.Request.URL.Path == "/ui" {
			c.Next()
			return
		}

		key := c.GetHeader("X-API-KEY")
		if key != "ultra-secret-key" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
			return
		}
		c.Next()
	})

	api.POST("/orders", s.PlaceOrder)
	api.GET("/orders", s.GetUserOrders)
	api.DELETE("/orders", s.CancelOrder)
	api.POST("/deposit", s.MockDeposit)
	api.GET("/depth", s.GetDepth)
	api.GET("/balances", s.GetBalances)
	api.GET("/trades", s.GetTradeHistory)

	r.NoRoute(func(c *gin.Context) {
		c.File("./public/index.html")
	})

	fmt.Printf("API Gateway listening on :%s\n", port)
	return r.Run(":" + port)
}

// PlaceOrder — the hot path: Risk → Sequencer → Disruptor → Engine
func (s *Server) PlaceOrder(c *gin.Context) {
	var req OrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	side := engine.Bid
	if req.Side == "sell" {
		side = engine.Ask
	}

	orderType := engine.LimitOrder
	if req.Type == "MARKET" {
		orderType = engine.MarketOrder
	}

	tif := engine.GTC
	switch req.TimeInForce {
	case "IOC":
		tif = engine.IOC
	case "FOK":
		tif = engine.FOK
	}

	order := &engine.Order{
		UserID:      req.UserID,
		Symbol:      req.Symbol,
		Price:       req.Price,
		Qty:         req.Qty,
		InitialQty:  req.Qty,
		Side:        side,
		Type:        orderType,
		TimeInForce: tif,
		PostOnly:    req.PostOnly,
		Status:      "OPEN",
		Timestamp:   time.Now().UnixNano(),
	}

	// Derive base/quote from symbol "BTC_USDT" → "BTC", "USDT"
	base, quote, err := parseSymbol(req.Symbol)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Context with tight deadline to prevent API stalls from downstream blocking
	ctx, cancel := context.WithTimeout(c.Request.Context(), 50*time.Millisecond)
	defer cancel()

	// Router handles: RiskCheck → SequencerID → WAL → Enqueue to Engine
	orderID, err := s.Router.SubmitOrder(ctx, req.Symbol, order, base, quote)
	if err != nil {
		log.Printf("[API] Order rejected for %s: %v", req.Symbol, err)
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"message":       "Order Placed",
		"order_id":      strconv.FormatUint(orderID, 10),
		"initial_qty":   order.InitialQty,
		"remaining_qty": order.Qty,
	})
}

// CancelOrder routes a cancel request through the Router
func (s *Server) CancelOrder(c *gin.Context) {
	var req CancelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 50*time.Millisecond)
	defer cancel()

	if err := s.Router.CancelOrder(ctx, req.Symbol, req.OrderID); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Order cancelled"})
}

func (s *Server) GetUserOrders(c *gin.Context) {
	userIDStr := c.Query("user_id")
	userID, _ := strconv.ParseUint(userIDStr, 10, 64)

	// Fetch from persistent history in Redis
	orders, err := s.RiskEngine.GetOrderHistory(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, orders)
}

func (s *Server) GetTradeHistory(c *gin.Context) {
	userIDStr := c.Query("user_id")
	userID, _ := strconv.ParseUint(userIDStr, 10, 64)

	trades, err := s.RiskEngine.GetTradeHistory(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, trades)
}

// MockDeposit simulates users depositing money
func (s *Server) MockDeposit(c *gin.Context) {
	userIDStr := c.Query("user_id")
	asset := c.Query("asset")
	amountStr := c.Query("amount")

	userID, _ := strconv.ParseUint(userIDStr, 10, 64)
	amount, _ := strconv.ParseUint(amountStr, 10, 64)

	err := s.RiskEngine.Deposit(c.Request.Context(), userID, asset, amount)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to deposit"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Deposit successful"})
}

func (s *Server) GetBalances(c *gin.Context) {
	userIDStr := c.Query("user_id")
	userID, _ := strconv.ParseUint(userIDStr, 10, 64)

	assets := []string{"BTC", "ETH", "SOL", "USDT"}
	// ARCH-4 FIX: single MGet round trip instead of 4 sequential GETs.
	balances, err := s.RiskEngine.GetBalances(c.Request.Context(), userID, assets)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, balances)
}

func (s *Server) GetDepth(c *gin.Context) {
	symbol := c.Query("symbol")
	if symbol == "" {
		symbol = "BTC_USDT"
	}

	snap, err := s.Router.FetchDepth(symbol)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, snap)
}

func parseSymbol(symbol string) (base, quote string, err error) {
	for i, ch := range symbol {
		if ch == '_' {
			return symbol[:i], symbol[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("invalid symbol format %q: expected BASE_QUOTE (e.g. BTC_USDT)", symbol)
}
