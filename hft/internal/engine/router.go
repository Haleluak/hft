package engine

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
	// Using standard library and imported risk package if available.
	// Since risk package is at `exchange/risk` or `exchange_server/risk`, we define an interface here to avoid circular dependency.
)

// RiskEngine is an interface that allows the Engine Router to lock/unlock user balances
// avoiding a heavy hard dependency. It corresponds to methods in `risk/redis.go`.
type RiskEngine interface {
	CheckAndLockBalance(ctx context.Context, userID uint64, asset string, expectedLockQty uint64) error
	UnlockBalance(ctx context.Context, userID uint64, asset string, unlockQty uint64) error
	SettleMatch(ctx context.Context, match Match) error
	SaveOrder(ctx context.Context, order *Order) error
	GetOrderHistory(ctx context.Context, userID uint64) ([]*Order, error)
	CancelOrder(ctx context.Context, userID, orderID uint64)
}

// Router represents a Sharding Router that directs requests to specific Engine actors based on the Pair.
// It also integrates the Risk Management layer.
type Router struct {
	mu           sync.RWMutex
	engines      map[string]*Engine
	sequencer    *Sequencer
	risk         RiskEngine
	snapshotMngr *SnapshotManager // Optional manager for dumping state
	nextCPUCore  int              // Auto-incremented: each new Engine gets its own physical core
}

// NewRouter initializes a load balancer/shard manager for our matching engines.
func NewRouter(risk RiskEngine, snapshotMngr *SnapshotManager) *Router {
	return &Router{
		engines:      make(map[string]*Engine),
		sequencer:    NewSequencer(1),
		risk:         risk,
		snapshotMngr: snapshotMngr,
	}
}

// RegisterEngine adds a new trading pair and boots up its Engine actor.
// It automatically assigns the next available physical CPU core to this Engine.
func (r *Router) RegisterEngine(symbol string) *Engine {
	r.mu.Lock()
	defer r.mu.Unlock()

	if eng, exists := r.engines[symbol]; exists {
		return eng // Already registered
	}

	eng := NewEngine(symbol)
	r.engines[symbol] = eng

	// Auto-assign next CPU core (wraps around if more pairs than cores)
	cpuCore := r.nextCPUCore % runtime.NumCPU()
	r.nextCPUCore++

	// Start the background Actor loop pinned to that core
	eng.Start(cpuCore)

	// Connect matches and cancellations back to the Risk Engine
	go r.handleEngineEvents(eng)

	return eng
}

func (r *Router) GetEngine(symbol string) (*Engine, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if eng, ok := r.engines[symbol]; ok {
		return eng, nil
	}
	return nil, fmt.Errorf("trading pair %s is not supported or engine not started", symbol)
}

// EnsureBalanceLock checks the Base Asset limit for Sells, and Quote Asset limit for Buys.
func (r *Router) ensureBalanceLock(ctx context.Context, order *Order, base, quote string) error {
	if r.risk == nil {
		return nil // Risk disabled
	}

	assetToLock := ""
	qtyToLock := uint64(0)

	if order.Side == Bid { // Buy: lock USDT
		assetToLock = quote
		qtyToLock = order.Qty * order.Price // Value in Quote asset
	} else { // Sell: lock BTC
		assetToLock = base
		qtyToLock = order.Qty // Value in Base asset
	}

	return r.risk.CheckAndLockBalance(ctx, order.UserID, assetToLock, qtyToLock)
}

// SubmitOrder acts as the gateway. It checks balance -> locks balance -> assigns Sequencer ID -> enqueues to Actor.
func (r *Router) SubmitOrder(ctx context.Context, symbol string, order *Order, baseAsset, quoteAsset string) (uint64, error) {
	eng, err := r.GetEngine(symbol)
	if err != nil {
		return 0, err
	}

	// 1. Pre-Trade Risk Check (Blocking & Atomic)
	if err := r.ensureBalanceLock(ctx, order, baseAsset, quoteAsset); err != nil {
		return 0, fmt.Errorf("risk rejection: %w", err)
	}

	// 2. Assign Deterministic Sequential ID (Thread-Safe)
	order.ID = r.sequencer.NextID()

	// 3. PERSIST: Save to History (Redis)
	_ = r.risk.SaveOrder(ctx, order)

	// 4. Enqueue to Lock-free RingBuffer. Once it's here, Execution is guaranteed.
	errChan := make(chan error, 1)
	if ok := eng.SubmitOrder(order, errChan); !ok {
		// EC-3: Engine stopped while we were trying to enqueue — rollback balance.
		_ = r.rollbackBalance(context.Background(), order, baseAsset, quoteAsset)
		return 0, ErrEngineStopped
	}

	// Await the synchronous (but super fast) result if the caller cares.
	// In pure HFT, this errChan is omitted and we respond asynchronously.
	select {
	case engErr := <-errChan:
		if engErr != nil {
			// Rollback risk if Engine rejected the order (e.g., duplicated ID)
			_ = r.rollbackBalance(context.Background(), order, baseAsset, quoteAsset)
			return 0, engErr
		}
		// Success! SnapshotWAL could sink the successful place event here.
		if r.snapshotMngr != nil {
			r.snapshotMngr.CommitLog(PlaceOrder, order)
		}
		return order.ID, nil

	case <-ctx.Done():
		// FIX #5: order state is unknown at this point (may or may not have been
		// enqueued). Return a real error so the caller can decide whether to retry
		// or rollback, instead of silently assuming success.
		return 0, fmt.Errorf("submit order timeout: %w", ctx.Err())
	}

}

func (r *Router) rollbackBalance(ctx context.Context, order *Order, base, quote string) error {
	if r.risk == nil {
		return nil
	}
	if order.Side == Bid {
		return r.risk.UnlockBalance(ctx, order.UserID, quote, order.Qty*order.Price)
	} else {
		return r.risk.UnlockBalance(ctx, order.UserID, base, order.Qty)
	}
}

func (r *Router) FetchDepth(symbol string) (*Snapshot, error) {
	eng, err := r.GetEngine(symbol)
	if err != nil {
		return nil, err
	}

	errChan := make(chan error, 1)
	snapChan := make(chan *Snapshot, 1)
	req := Request{
		Type:     GetOrders,
		SnapChan: snapChan,
		ErrChan:  errChan,
	}
	for !eng.queue.Enqueue(req) {
		runtime.Gosched()
	}

	// FIX #1: Engine writes SnapChan first, then ErrChan.
	// Reading ErrChan first caused a deadlock when snapChan was full.
	snap := <-snapChan
	err = <-errChan
	return snap, err
}

func (r *Router) GetUserOrders(userID uint64) ([]*Order, error) {
	r.mu.RLock()
	engines := make([]*Engine, 0, len(r.engines))
	for _, eng := range r.engines {
		engines = append(engines, eng)
	}
	r.mu.RUnlock()

	var wg sync.WaitGroup
	var mu sync.Mutex // protects allUserOrders slice
	var allUserOrders []*Order

	// Scatter: Query all engines concurrently
	for _, eng := range engines {
		wg.Add(1)
		go func(e *Engine) {
			defer wg.Done()

			errChan := make(chan error, 1)
			snapChan := make(chan *Snapshot, 1)
			req := Request{
				Type:     GetOrders,
				SnapChan: snapChan,
				ErrChan:  errChan,
			}
			for !e.queue.Enqueue(req) {
				runtime.Gosched()
			}

			snap := <-snapChan
			<-errChan

			// Gather: Collect orders specific to this user
			var userOrders []*Order
			for _, o := range snap.Orders {
				if o.UserID == userID {
					userOrders = append(userOrders, o)
				}
			}

			if len(userOrders) > 0 {
				mu.Lock()
				allUserOrders = append(allUserOrders, userOrders...)
				mu.Unlock()
			}
		}(eng)
	}

	wg.Wait()
	return allUserOrders, nil
}

// CancelOrder routes cancel request to appropriate Engine.
func (r *Router) CancelOrder(ctx context.Context, symbol string, orderID uint64) error {
	eng, err := r.GetEngine(symbol)
	if err != nil {
		return err
	}

	errChan := make(chan error, 1)
	if ok := eng.Cancel(orderID, errChan); !ok {
		// EC-3: Engine already stopped.
		return ErrEngineStopped
	}

	if engErr := <-errChan; engErr != nil {
		return engErr
	}

	if r.snapshotMngr != nil {
		r.snapshotMngr.CommitLog(CancelOrder, &Order{ID: orderID})
	}

	return nil
}

// handleEngineEvents runs in background per Engine, reacting to output cancellations.
// ARCH-3 FIX: each Redis call uses a short context with timeout so the goroutine
// cannot block indefinitely if Redis becomes unavailable during shutdown.
func (r *Router) handleEngineEvents(eng *Engine) {
	for canceledOrder := range eng.CancelChan {
		if r.risk == nil || canceledOrder == nil {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)

		r.risk.CancelOrder(ctx, canceledOrder.UserID, canceledOrder.ID)

		parts := strings.SplitN(eng.ob.Symbol, "_", 2)
		if len(parts) == 2 {
			_ = r.rollbackBalance(ctx, canceledOrder, parts[0], parts[1])
		}

		cancel()
	}
}
