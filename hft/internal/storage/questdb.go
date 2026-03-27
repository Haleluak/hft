package storage

import (
	"context"
	"fmt"
	"log"
	"time"

	"exchange/internal/engine"

	questdb "github.com/questdb/go-questdb-client/v3"
)

// QuestDBStorage manages async flushing of trade events to QuestDB via ILP.
//
// ARCH-1 FIX: The original code shared a single LineSender across multiple
// goroutines (one per symbol). questdb.LineSender is NOT thread-safe — concurrent
// calls to Table/Int64Column/At/Flush on the same sender cause data corruption
// or panics. Each worker now creates its own dedicated sender.
type QuestDBStorage struct {
	addr string // saved so workers can create their own senders
}

// NewQuestDBStorage validates connectivity by opening a probe sender.
// Returns an error if QuestDB is unreachable at startup.
func NewQuestDBStorage(ctx context.Context, address string) (*QuestDBStorage, error) {
	// Probe connection — open and immediately close.
	probe, err := questdb.LineSenderFromConf(ctx, fmt.Sprintf("tcp::addr=%s;", address))
	if err != nil {
		return nil, fmt.Errorf("could not connect to QuestDB at %s: %w", address, err)
	}
	probe.Close(ctx)

	return &QuestDBStorage{addr: address}, nil
}

// StartWorker spawns a background goroutine for one symbol.
// Each worker owns its own LineSender — no sharing, no races.
//
// Flushing strategy: we flush after every batch received from matchChan rather
// than per-row, which amortises TCP overhead across all rows in the batch.
func (q *QuestDBStorage) StartWorker(ctx context.Context, symbol string, matchChan <-chan []engine.Match) {
	go func() {
		// Each goroutine creates its own sender (ARCH-1 fix).
		sender, err := questdb.LineSenderFromConf(ctx, fmt.Sprintf("tcp::addr=%s;", q.addr))
		if err != nil {
			log.Printf("[QUESTDB] worker for %s failed to connect: %v — trades will NOT be persisted", symbol, err)
			// Drain the channel so the fanout goroutine is not blocked.
			for range matchChan {
			}
			return
		}
		defer sender.Close(ctx)

		for {
			select {
			case matches, ok := <-matchChan:
				if !ok {
					// Channel closed (engine stopped) — flush remaining buffer and exit.
					if err := sender.Flush(ctx); err != nil {
						log.Printf("[QUESTDB] final flush error for %s: %v", symbol, err)
					}
					return
				}

				for _, match := range matches {
					err := sender.
						Table("trades").
						Symbol("symbol", symbol).
						Int64Column("maker_order_id", int64(match.MakerOrderID)).
						Int64Column("taker_order_id", int64(match.TakerOrderID)).
						Int64Column("price", int64(match.Price)).
						Int64Column("qty", int64(match.Qty)).
						At(ctx, time.Now().UTC())
					if err != nil {
						log.Printf("[QUESTDB] row error for %s: %v", symbol, err)
					}
				}

				// Flush once per batch — amortises network cost.
				if err := sender.Flush(ctx); err != nil {
					log.Printf("[QUESTDB] flush error for %s: %v", symbol, err)
				}

			case <-ctx.Done():
				if err := sender.Flush(ctx); err != nil {
					log.Printf("[QUESTDB] shutdown flush error for %s: %v", symbol, err)
				}
				return
			}
		}
	}()
}
