package main

import (
	"context"
	"exchange/internal/app"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	// 1. Context and Shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 2. Build Application with Functional Options
	application := app.New(
		app.WithRedisAddr(getEnv("REDIS_ADDR", "localhost:6379")),
		app.WithQuestDBAddr(getEnv("QUESTDB_ADDR", "localhost:9009")),
		app.WithTradingPairs(getEnvList("TRADING_PAIRS", "BTC_USDT,ETH_USDT,SOL_USDT")),
		app.WithAPIPort(getEnv("API_PORT", "8080")),
		app.WithSnapshotEnabled(getEnv("ENABLE_SNAPSHOT", "false") == "true"),
	)

	// 3. Trap Signals
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sig
		log.Println("Shutting down exchange gracefully...")
		cancel()
	}()

	// 4. Run Application
	if err := application.Run(ctx); err != nil {
		log.Fatalf("Fatal error during execution: %v", err)
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvList(key, fallback string) []string {
	v := getEnv(key, fallback)
	return strings.Split(v, ",")
}
