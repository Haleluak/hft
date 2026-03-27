package engine

import (
	"bufio"
	"encoding/gob"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// Snapshot represents a full point-in-time state dump of the engine
type Snapshot struct {
	Symbol  string   `json:"symbol"`
	Orders  []*Order `json:"orders"`
	GenTime int64    `json:"gen_time"` // UNIX timestamp of snapshot
}

// WriteAheadLog (WAL) tracks all incoming mutating operations before processing.
type WALEvent struct {
	Timestamp int64       `json:"ts"`
	Type      RequestType `json:"type"`
	Order     *Order      `json:"order,omitempty"`
}

// SnapshotManager coordinates taking periodic state snapshots and appending to WAL.
// Real production systems write to Redis Append-Only File (AOF) or Apache Kafka.
// This is a minimal local disk implementation.
type SnapshotManager struct {
	mu          sync.Mutex
	walFile     *os.File
	walWriter   *bufio.Writer // Batching IO writes
	walEncoder  *gob.Encoder  // Fast binary encoder
	snapTicker  *time.Ticker
	flushTicker *time.Ticker
	engine      *Engine // The engine to snapshot
}

// NewSnapshotManager creates a snapshot/WAL loop
func NewSnapshotManager(symbol string, engine *Engine, duration time.Duration) (*SnapshotManager, error) {
	// Create data directory for tidy storage
	if err := os.MkdirAll("data", 0755); err != nil {
		return nil, err
	}

	// Open append-only file for Write-Ahead Log events (.gob extension instead of .log)
	f, err := os.OpenFile(fmt.Sprintf("data/%s_wal.gob", symbol), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}

	// Create a 64KB buffer for writing. This stops every single log from hitting the hard drive instantly.
	bw := bufio.NewWriterSize(f, 64*1024)

	sm := &SnapshotManager{
		walFile:     f,
		walWriter:   bw,
		walEncoder:  gob.NewEncoder(bw),
		snapTicker:  time.NewTicker(duration),
		flushTicker: time.NewTicker(10 * time.Millisecond), // Flush buffer to disk every 10ms
		engine:      engine,
	}

	go sm.periodicSnapshotLoop(symbol)
	go sm.periodicFlushLoop()

	return sm, nil
}

// CommitLog synchronously writes mutating action.
// In reality, it should be batched to avoid disk IO bottleneck.
func (sm *SnapshotManager) CommitLog(reqType RequestType, order *Order) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	event := WALEvent{
		Timestamp: time.Now().UnixNano(),
		Type:      reqType,
		Order:     order,
	}

	// Instead of json.Marshal (Creates garbage, uses string reflection),
	// Gob encodes struct directly to binary format and writes to RAM buffer.
	if err := sm.walEncoder.Encode(&event); err != nil {
		log.Printf("[SNAPSHOT] WAL Encode error: %v", err)
	}
}

// periodicFlushLoop runs in the background and commits RAM buffer to Disk
func (sm *SnapshotManager) periodicFlushLoop() {
	for range sm.flushTicker.C {
		sm.mu.Lock()
		sm.walWriter.Flush()
		// f.Sync() // Optional: fsync to guarantee flush to physical disk vs OS cache
		sm.mu.Unlock()
	}
}

func (sm *SnapshotManager) periodicSnapshotLoop(symbol string) {
	for range sm.snapTicker.C {
		// Take snapshot of Engine safely
		// We can't lock the queue, so we rely on the specific `Snapshot()` method
		// in `Orderbook` that returns a copy of Map
		snap := sm.engine.ob.Snapshot()

		snap.Symbol = symbol
		snap.GenTime = time.Now().Unix()

		// Write safely to a new snapshot dump file (.gob binary)
		f, _ := os.Create(fmt.Sprintf("data/%s_snapshot_%d.gob", symbol, snap.GenTime))
		bw := bufio.NewWriter(f)
		enc := gob.NewEncoder(bw)
		enc.Encode(snap)
		bw.Flush()
		f.Close()

		fmt.Printf("[SNAPSHOT] Engine state saved for %s at t=%d (Binary GOB)\n", symbol, snap.GenTime)

		// Here you would truncate/rotate WAL since you have a solid snapshot.
	}
}

// RecoverState allows a fresh engine to replay Orders from the snapshot + WAL
func (sm *SnapshotManager) RecoverState(targetEngine *Engine, snapshot *Snapshot) error {
	for _, o := range snapshot.Orders {
		// Just bypass network/queue and reload them directly
		_, _, err := targetEngine.ob.AddOrder(o)
		if err != nil {
			return err
		}
	}
	fmt.Printf("[RECOVERY] Reloaded %d orders from snapshot\n", len(snapshot.Orders))
	return nil
}
