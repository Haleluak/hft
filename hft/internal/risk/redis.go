package risk

import (
	"context"
	"encoding/json"
	"exchange/internal/engine"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// AssetBalance holds the in-memory state of a specific asset for a user
type AssetBalance struct {
	mu      sync.Mutex
	Balance uint64
	Loaded  bool
}

// RiskEngine handles balance checks and reservation before orders enter the matching engine.
// HIGH-FREQUENCY TRADING OPTIMIZATION:
// Balances are loaded lazily into RAM (sync.Map) and locked strictly per-user-asset payload.
// Orders get validated against RAM in <100 nanoseconds.
// Redis logic is transitioned to an Asynchronous Write-Behind model.
type RiskEngine struct {
	client      *redis.Client
	cache       sync.Map    // In-memory per-key lock and balance
	persistChan chan func() // Worker channel for async Redis I/O
}

func NewRiskEngine(ctx context.Context, redisAddr string) *RiskEngine {
	client := redis.NewClient(&redis.Options{
		Addr:            redisAddr,
		PoolSize:        100, // Increased to handle high async throughput
		MinIdleConns:    10,
		MaxIdleConns:    100,
		ConnMaxIdleTime: 30 * time.Second,
	})

	re := &RiskEngine{
		client:      client,
		persistChan: make(chan func(), 100000), // Large buffer to absorb bursts
	}

	// Boot 10 Background Persistence Workers (Worker Pool Pattern)
	// Completely offloads Redis I/O from the synchronous matching/settlement path
	for i := 0; i < 10; i++ {
		go re.persistenceWorker()
	}

	return re
}

func (r *RiskEngine) persistenceWorker() {
	for task := range r.persistChan {
		task() // Execute Redis persistence payload
	}
}

// getAssetBalance loads an asset safely with single-flight initialization from Redis
func (r *RiskEngine) getAssetBalance(ctx context.Context, key string) *AssetBalance {
	val, _ := r.cache.LoadOrStore(key, &AssetBalance{})
	ab := val.(*AssetBalance)

	if !ab.Loaded {
		ab.mu.Lock()
		if !ab.Loaded {
			redisVal, err := r.client.Get(ctx, key).Uint64()
			if err == nil {
				ab.Balance = redisVal
			}
			ab.Loaded = true
		}
		ab.mu.Unlock()
	}
	return ab
}

// CheckAndLockBalance checks if the user has enough balance in RAM, and deducts it.
func (r *RiskEngine) CheckAndLockBalance(ctx context.Context, userID uint64, asset string, expectedLockQty uint64) error {
	key := fmt.Sprintf("balance:%d:%s", userID, asset)
	ab := r.getAssetBalance(ctx, key)

	ab.mu.Lock()
	defer ab.mu.Unlock()

	if ab.Balance >= expectedLockQty {
		ab.Balance -= expectedLockQty
		// Async write-behind using the bounded persistence workers
		r.persistChan <- func() {
			r.client.DecrBy(context.Background(), key, int64(expectedLockQty))
		}
		return nil
	}

	return fmt.Errorf("insufficient %s balance", asset)
}

// UnlockBalance is called when an order is cancelled or expires unfilled.
func (r *RiskEngine) UnlockBalance(ctx context.Context, userID uint64, asset string, unlockQty uint64) error {
	key := fmt.Sprintf("balance:%d:%s", userID, asset)
	ab := r.getAssetBalance(ctx, key)

	ab.mu.Lock()
	ab.Balance += unlockQty
	ab.mu.Unlock()

	r.persistChan <- func() {
		r.client.IncrBy(context.Background(), key, int64(unlockQty))
	}
	return nil
}

// Deposit credits user money directly to RAM and Redis synchronously
func (r *RiskEngine) Deposit(ctx context.Context, userID uint64, asset string, amount uint64) error {
	key := fmt.Sprintf("balance:%d:%s", userID, asset)
	ab := r.getAssetBalance(ctx, key)

	ab.mu.Lock()
	ab.Balance += amount
	ab.mu.Unlock()

	return r.client.IncrBy(ctx, key, int64(amount)).Err()
}

// GetBalance retrieves the current RAM balance
func (r *RiskEngine) GetBalance(ctx context.Context, userID uint64, asset string) (uint64, error) {
	key := fmt.Sprintf("balance:%d:%s", userID, asset)
	ab := r.getAssetBalance(ctx, key)

	ab.mu.Lock()
	bal := ab.Balance
	ab.mu.Unlock()
	return bal, nil
}

// GetBalances fetches multiple asset balances entirely from RAM
func (r *RiskEngine) GetBalances(ctx context.Context, userID uint64, assets []string) (map[string]uint64, error) {
	result := make(map[string]uint64, len(assets))
	for _, asset := range assets {
		key := fmt.Sprintf("balance:%d:%s", userID, asset)
		ab := r.getAssetBalance(ctx, key)

		ab.mu.Lock()
		result[asset] = ab.Balance
		ab.mu.Unlock()
	}
	return result, nil
}

func (r *RiskEngine) addBalanceAsync(ctx context.Context, key string, amount uint64, pipe redis.Pipeliner) {
	ab := r.getAssetBalance(ctx, key)
	ab.mu.Lock()
	ab.Balance += amount
	ab.mu.Unlock()
	pipe.IncrBy(ctx, key, int64(amount))
}

// SettleMatch credits the matched assets to Maker and Taker
func (r *RiskEngine) SettleMatch(ctx context.Context, match engine.Match) error {
	pipe := r.client.TxPipeline()

	makerBTCKey := fmt.Sprintf("balance:%d:BTC", match.MakerUserID)
	makerUSDTKey := fmt.Sprintf("balance:%d:USDT", match.MakerUserID)
	takerBTCKey := fmt.Sprintf("balance:%d:BTC", match.TakerUserID)
	takerUSDTKey := fmt.Sprintf("balance:%d:USDT", match.TakerUserID)

	matchUSDTValue := match.Price * match.Qty

	if match.TakerSide == engine.Bid {
		r.addBalanceAsync(ctx, takerBTCKey, match.Qty, pipe)
		r.addBalanceAsync(ctx, makerUSDTKey, matchUSDTValue, pipe)

		if match.TakerOriginalPrice > match.Price {
			refund := (match.TakerOriginalPrice - match.Price) * match.Qty
			r.addBalanceAsync(ctx, takerUSDTKey, refund, pipe)
		}
	} else {
		r.addBalanceAsync(ctx, takerUSDTKey, matchUSDTValue, pipe)
		r.addBalanceAsync(ctx, makerBTCKey, match.Qty, pipe)
	}

	// Persist the match lazily in the background
	r.persistChan <- func() {
		// 1. Commit balances
		if _, err := pipe.Exec(context.Background()); err != nil {
			fmt.Printf("[RISK] Redis SettleMatch fallback failed: %v\n", err)
		}

		// 2. Update Order History ──
		r.updateOrderQty(context.Background(), match.MakerUserID, match.MakerOrderID, match.Qty)
		r.updateOrderQty(context.Background(), match.TakerUserID, match.TakerOrderID, match.Qty)

		// 3. Persist Trade Executions for Trade History ──
		r.saveTrade(context.Background(), match.MakerUserID, match, "MAKER")
		r.saveTrade(context.Background(), match.TakerUserID, match, "TAKER")
	}

	return nil
}

func (r *RiskEngine) updateOrderQty(ctx context.Context, userID, orderID, matchedQty uint64) {
	key := fmt.Sprintf("orders:%d", userID)
	field := fmt.Sprintf("%d", orderID)

	val, err := r.client.HGet(ctx, key, field).Result()
	if err != nil {
		return
	}

	var o engine.Order
	if err := json.Unmarshal([]byte(val), &o); err != nil {
		return
	}

	if o.Qty >= matchedQty {
		o.Qty -= matchedQty
	} else {
		o.Qty = 0
	}

	if o.Qty == 0 {
		o.Status = "FILLED"
	}

	data, _ := json.Marshal(o)
	_ = r.client.HSet(ctx, key, field, data).Err()
}

// TradeExecution is a single fill record stored per user.
type TradeExecution struct {
	Time    int64       `json:"time"` // Unix nano
	Symbol  string      `json:"symbol"`
	Price   uint64      `json:"price"`
	Qty     uint64      `json:"qty"`
	Role    string      `json:"role"` // "MAKER" | "TAKER"
	Side    engine.Side `json:"side"` // buyer/seller perspective
	MatchID string      `json:"match_id"`
}

// saveTrade persists a trade execution record for a user using a Redis List (LPUSH).
// We cap the list at 200 entries per user to avoid unbounded growth.
func (r *RiskEngine) saveTrade(ctx context.Context, userID uint64, match engine.Match, role string) {
	side := match.TakerSide
	if role == "MAKER" {
		// Maker's side is the opposite of Taker's
		if match.TakerSide == engine.Bid {
			side = engine.Ask
		} else {
			side = engine.Bid
		}
	}

	trade := TradeExecution{
		Time:    time.Now().UnixNano(),
		Symbol:  match.Symbol,
		Price:   match.Price,
		Qty:     match.Qty,
		Role:    role,
		Side:    side,
		MatchID: fmt.Sprintf("%d-%d", match.MakerOrderID, match.TakerOrderID),
	}
	data, _ := json.Marshal(trade)

	key := fmt.Sprintf("trades:%d", userID)
	pipe := r.client.Pipeline()
	pipe.LPush(ctx, key, data)   // prepend newest
	pipe.LTrim(ctx, key, 0, 199) // keep last 200 only
	_, _ = pipe.Exec(ctx)
}

// GetTradeHistory returns the most recent trade executions for a user.
func (r *RiskEngine) GetTradeHistory(ctx context.Context, userID uint64) ([]TradeExecution, error) {
	key := fmt.Sprintf("trades:%d", userID)
	data, err := r.client.LRange(ctx, key, 0, 199).Result()
	if err != nil {
		return nil, err
	}

	executions := make([]TradeExecution, 0, len(data))
	for _, v := range data {
		var t TradeExecution
		if err := json.Unmarshal([]byte(v), &t); err == nil {
			executions = append(executions, t)
		}
	}
	return executions, nil
}

// SaveOrder stores an order into user's history list in Redis
func (r *RiskEngine) SaveOrder(ctx context.Context, order *engine.Order) error {
	key := fmt.Sprintf("orders:%d", order.UserID)
	data, _ := json.Marshal(order)

	// Use HSET so we can update the same order if it matches
	return r.client.HSet(ctx, key, fmt.Sprintf("%d", order.ID), data).Err()
}

// CancelOrder updates an order's status to CANCELED in Redis
func (r *RiskEngine) CancelOrder(ctx context.Context, userID, orderID uint64) {
	key := fmt.Sprintf("orders:%d", userID)
	field := fmt.Sprintf("%d", orderID)

	val, err := r.client.HGet(ctx, key, field).Result()
	if err != nil {
		return
	}

	var o engine.Order
	if err := json.Unmarshal([]byte(val), &o); err != nil {
		return
	}

	o.Status = "CANCELED"
	data, _ := json.Marshal(o)
	_ = r.client.HSet(ctx, key, field, data).Err()
}

// GetOrderHistory retrieves all historical orders for a user
func (r *RiskEngine) GetOrderHistory(ctx context.Context, userID uint64) ([]*engine.Order, error) {
	key := fmt.Sprintf("orders:%d", userID)
	data, err := r.client.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, err
	}

	var orders []*engine.Order
	for _, v := range data {
		var o engine.Order
		if err := json.Unmarshal([]byte(v), &o); err == nil {
			orders = append(orders, &o)
		}
	}
	return orders, nil
}
