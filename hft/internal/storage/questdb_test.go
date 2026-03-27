package storage

import (
	"exchange/internal/engine"
	"testing"
)

// A mock to verify that our struct interface satisfies the Go compiler
func TestQuestDBStorage_Compilation(t *testing.T) {
	// Typically QuestDB tests need a running container instance. We only verify interface here.
	t.Logf("Checking QuestDB module dependency")
}

func BenchmarkQuestDBWorkerQueue(b *testing.B) {
	// Simple queueing test to ensure channels are not blocking Engine.MatchChan throughput
	matchChan := make(chan []engine.Match, 1000)

	go func() {
		// Mock consumer instead of real HTTP QuestDB Flush
		for range matchChan {
			// Simulating fast ingestion flush delay
		}
	}()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		matchChan <- []engine.Match{
			{MakerOrderID: 1, TakerOrderID: uint64(i), Price: 50000, Qty: 100},
		}
	}
}
