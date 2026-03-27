package app

import (
	"context"
	"exchange/internal/api"
	"exchange/internal/engine"
	"exchange/internal/risk"
	"exchange/internal/storage"
	"exchange/internal/ws"
	"log"
	"runtime"
	"time"
)

// Config holds all environment-based configuration
type Config struct {
	RedisAddr      string
	QuestDBAddr    string
	TradingPairs   []string
	APIPort        string
	EnableSnapshot bool
}

// Option applies a configuration to the App.
type Option func(*Config)

// WithRedisAddr sets the Redis address.
func WithRedisAddr(addr string) Option {
	return func(c *Config) {
		c.RedisAddr = addr
	}
}

// WithQuestDBAddr sets the QuestDB address.
func WithQuestDBAddr(addr string) Option {
	return func(c *Config) {
		c.QuestDBAddr = addr
	}
}

// WithTradingPairs sets the active trading pairs.
func WithTradingPairs(pairs []string) Option {
	return func(c *Config) {
		c.TradingPairs = pairs
	}
}

// WithAPIPort sets the API Server port.
func WithAPIPort(port string) Option {
	return func(c *Config) {
		c.APIPort = port
	}
}

// WithSnapshotEnabled toggles state WAL snapshotting.
func WithSnapshotEnabled(enabled bool) Option {
	return func(c *Config) {
		c.EnableSnapshot = enabled
	}
}

// App is the high-level container for the exchange services
type App struct {
	cfg    Config
	router *engine.Router
	risk   *risk.RiskEngine
	wsBus  *ws.RedisBus
	broad  *ws.Broadcaster
	qdb    *storage.QuestDBStorage
	server *api.Server
}

// New constructs an App configuring it via provided functional options.
func New(opts ...Option) *App {
	// 1. Default Configuration
	cfg := Config{
		RedisAddr:      "localhost:6379",
		QuestDBAddr:    "localhost:9009",
		TradingPairs:   []string{"BTC_USDT"},
		APIPort:        "8080",
		EnableSnapshot: false,
	}

	// 2. Apply explicit Options over defaults
	for _, opt := range opts {
		opt(&cfg)
	}

	return &App{cfg: cfg}
}

func (a *App) Run(ctx context.Context) error {
	log.Printf("Booting @Antigravity High-Frequency Exchange | GOMAXPROCS=%d", runtime.NumCPU())

	// 1. Initialize Components
	a.risk = risk.NewRiskEngine(ctx, a.cfg.RedisAddr)
	a.router = engine.NewRouter(a.risk, nil)
	a.broad = ws.NewBroadcaster()
	a.wsBus = ws.NewRedisBus(a.cfg.RedisAddr)

	// 2. Wire Engines & Fanouts
	type pairChans struct {
		ws     chan []engine.Match
		db     chan []engine.Match
		settle chan []engine.Match
	}
	fanouts := make(map[string]pairChans, len(a.cfg.TradingPairs))

	for _, symbol := range a.cfg.TradingPairs {
		eng := a.router.RegisterEngine(symbol)

		if a.cfg.EnableSnapshot {
			_, _ = engine.NewSnapshotManager(symbol, eng, 5*time.Minute)
		}

		wsCh := make(chan []engine.Match, 1000)
		dbCh := make(chan []engine.Match, 1000)
		setCh := make(chan []engine.Match, 1000)
		fanouts[symbol] = pairChans{ws: wsCh, db: dbCh, settle: setCh}

		// Parallel fan-out workers
		go a.runFanout(eng, wsCh, dbCh, setCh)
	}

	// 3. Initialize Storage
	var err error
	a.qdb, err = storage.NewQuestDBStorage(ctx, a.cfg.QuestDBAddr)
	if err != nil {
		log.Printf("[WARN] QuestDB unavailable: %v", err)
	} else {
		for symbol, ch := range fanouts {
			a.qdb.StartWorker(ctx, symbol, ch.db)
		}
	}

	// 4. Initialize WebSocket & Settlement
	a.wsBus.StartSubscriber(ctx, a.broad)

	for symbol, ch := range fanouts {
		go a.runWSMatchPublisher(ctx, symbol, ch.ws)

		// Depth updates
		eng, _ := a.router.GetEngine(symbol)
		go a.runWSDepthPublisher(ctx, symbol, eng)

		// Settlement
		go a.runSettlementWorker(ctx, ch.settle)
	}

	// 5. Start API Server
	a.server = api.NewServer(
		api.WithRouter(a.router),
		api.WithRiskEngine(a.risk),
		api.WithBroadcaster(a.broad),
	)

	log.Printf("Exchange live on :%s | %d pairs active", a.cfg.APIPort, len(a.cfg.TradingPairs))
	return a.server.Start(a.cfg.APIPort)
}

func (a *App) runFanout(eng *engine.Engine, wsCh, dbCh, settleCh chan []engine.Match) {
	for batch := range eng.MatchChan {
		select {
		case wsCh <- batch:
		default:
		}
		select {
		case dbCh <- batch:
		default:
		}
		select {
		case settleCh <- batch:
		default:
		}
	}
	close(wsCh)
	close(dbCh)
	close(settleCh)
}

func (a *App) runWSMatchPublisher(ctx context.Context, symbol string, ch <-chan []engine.Match) {
	for batch := range ch {
		for _, match := range batch {
			a.wsBus.Publish(ctx, symbol, "match", match)
		}
	}
}

func (a *App) runWSDepthPublisher(ctx context.Context, symbol string, eng *engine.Engine) {
	for depth := range eng.DepthChan {
		a.wsBus.Publish(ctx, symbol, "depth", depth)
	}
}

func (a *App) runSettlementWorker(ctx context.Context, ch <-chan []engine.Match) {
	for batch := range ch {
		for _, match := range batch {
			_ = a.risk.SettleMatch(ctx, match)
		}
	}
}
