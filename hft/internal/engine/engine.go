package engine

import (
	"fmt"
	"log"
	"runtime"
	"sync/atomic"
)

// Engine manages the orderbook with an actor model (Single Goroutine).
type Engine struct {
	ob         *Orderbook
	queue      *DisruptorQueue // Replaces reqChan with Lock-free RingBuffer
	MatchChan  chan []Match    // Channel to output matches
	CancelChan chan *Order     // Channel to output cancelled orders for Risk refund
	DepthChan  chan *DepthSnapshot
	running    int32
}

// Request types
type RequestType int

const (
	PlaceOrder RequestType = iota
	CancelOrder
	GetOrders
)

type Request struct {
	Type     RequestType
	Order    *Order // Used for PlaceOrder
	OrderID  uint64 // Used for CancelOrder/GetOrders
	UserID   uint64 // Used for GetOrders
	SnapChan chan *Snapshot
	ErrChan  chan error // Synchronous responses
}

func NewEngine(symbol string) *Engine {
	return &Engine{
		ob:         NewOrderbook(symbol),
		queue:      NewDisruptorQueue(131072), // 128K pre-allocated buffer power of 2
		MatchChan:  make(chan []Match, 10000), // 10K match batches — plenty for burst
		CancelChan: make(chan *Order, 10000),  // 10K cancel events
		DepthChan:  make(chan *DepthSnapshot, 500),
		running:    1,
	}
}

// ErrEngineStopped is returned when a caller tries to enqueue into a stopped Engine.
var ErrEngineStopped = fmt.Errorf("engine stopped")

// Start spawns the single worker goroutine that manages all state mutation.
// cpuCore: the physical CPU core index to pin this Actor's OS thread to.
// Pass -1 to skip pinning (useful in tests or when running many pairs on a small machine).
func (e *Engine) Start(cpuCore int) {
	go func() {
		// Step 1: Lock this goroutine to its OS thread PERMANENTLY.
		// Without this, the Go scheduler may migrate the goroutine between OS threads
		// at any point, defeating the purpose of CPU affinity entirely.
		runtime.LockOSThread()

		// Step 2: Pin the OS thread to the specific physical CPU core.
		if cpuCore >= 0 {
			if err := PinToCore(cpuCore); err != nil {
				log.Printf("[ENGINE %s] CPU affinity warning (core %d): %v", e.ob.Symbol, cpuCore, err)
			} else {
				log.Printf("[ENGINE %s] Pinned to CPU core %d", e.ob.Symbol, cpuCore)
			}
		}

		// Step 3: Run the hot-path event loop until Stop() is called.
		e.loop()

		// EC-2 FIX: Close all output channels AFTER the loop exits so that
		// every downstream goroutine (fanout, handleEngineEvents, depth readers)
		// unblocks from its range loop and exits cleanly instead of leaking forever.
		close(e.MatchChan)
		close(e.CancelChan)
		close(e.DepthChan)
		log.Printf("[ENGINE %s] Actor stopped, output channels closed.", e.ob.Symbol)
	}()
}

// loop is the hot-path single thread execution traversing the Ring Buffer.
// Uses adaptive exponential backoff when idle: no busy-spin that would peg the CPU
// at 100% even with zero orders. Backoff resets immediately on new work to preserve
// low-latency when the exchange is actually active.
func (e *Engine) loop() {
	var req Request
	for atomic.LoadInt32(&e.running) == 1 {
		// Dequeue locks nothing. It just checks the memory array pointer.
		if ok := e.queue.Dequeue(&req); ok {
			switch req.Type {
			case PlaceOrder:
				matches, stpCanceled, err := e.ob.AddOrder(req.Order)
				if err != nil && req.ErrChan != nil {
					req.ErrChan <- err
					continue
				}
				if len(matches) > 0 {
					e.MatchChan <- matches
				}
				// Push stpCanceled to refunds
				for _, ord := range stpCanceled {
					e.CancelChan <- ord
				}

				// Always emit depth on any change for real-time UI
				e.DepthChan <- e.ob.GetDepth(30)

				if req.ErrChan != nil {
					req.ErrChan <- nil
				}

			case CancelOrder:
				ord, err := e.ob.CancelOrder(req.OrderID)
				if err == nil {
					e.CancelChan <- ord
					// Depth ONLY on successful cancel
					e.DepthChan <- e.ob.GetDepth(30)
				}

				if req.ErrChan != nil {
					req.ErrChan <- err
				}

			case GetOrders:
				snap := e.ob.Snapshot()
				if req.SnapChan != nil {
					req.SnapChan <- snap
				}
				if req.ErrChan != nil {
					req.ErrChan <- nil
				}
			}
		} else {
			// Instead of manual exponential backoff which introduces unpredictable
			// tail latency (up to 1ms delay), we use standard scheduler yield.
			// At full load, Gosched enables maximum throughput.
			runtime.Gosched()
		}
	}
}

// SubmitOrder sends an order to the engine lock-free via the DisruptorQueue.
// If you want it completely async, set errChan to nil.
func (e *Engine) SubmitOrder(order *Order, errChan chan error) bool {
	req := Request{
		Type:    PlaceOrder,
		Order:   order,
		ErrChan: errChan,
	}
	// EC-3 FIX: Check the running flag inside the spin-wait.
	// Without this, if the Engine is stopped while the queue is full,
	// the goroutine would spin forever because nobody drains the queue anymore.
	for !e.queue.Enqueue(req) {
		if atomic.LoadInt32(&e.running) == 0 {
			if errChan != nil {
				errChan <- ErrEngineStopped
			}
			return false
		}
		runtime.Gosched()
	}
	return true
}

func (e *Engine) Cancel(orderID uint64, errChan chan error) bool {
	req := Request{
		Type:    CancelOrder,
		OrderID: orderID,
		ErrChan: errChan,
	}
	// EC-3 FIX: same guard as SubmitOrder.
	for !e.queue.Enqueue(req) {
		if atomic.LoadInt32(&e.running) == 0 {
			if errChan != nil {
				errChan <- ErrEngineStopped
			}
			return false
		}
		runtime.Gosched()
	}
	return true
}

// Stop gracefully shuts down the engine
func (e *Engine) Stop() {
	atomic.StoreInt32(&e.running, 0)
}
