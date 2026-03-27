package ws

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/mailru/easygo/netpoll"
)

// clientSendBuf is the depth of each client's outbound channel.
// If a slow client fills this buffer, messages are dropped for that client only —
// never blocking the broadcaster or other clients.
const clientSendBuf = 512

// Broadcaster manages Epoll-based concurrent thousands of WebSocket connections
// and broadcasts trade events ultra-fast to subscribed clients.
type Broadcaster struct {
	poller  netpoll.Poller
	clients sync.Map // *netpoll.Desc → *Client
	count   int64
}

// Client wraps a native net.Conn socket with a per-client send buffer
// and an explicit subscription set to filter symbol events.
type Client struct {
	conn       net.Conn
	desc       *netpoll.Desc
	sendCh     chan []byte // per-client outbound buffer; closed on teardown
	subscribed sync.Map    // map[symbol string] → struct{}
	closeOnce  sync.Once   // guarantees teardown runs exactly once
}

// clientMsg is the JSON control frame a client sends to manage subscriptions.
type clientMsg struct {
	Action string `json:"action"` // "subscribe" | "unsubscribe"
	Symbol string `json:"symbol"` // e.g. "BTC_USDT"
}

// NewBroadcaster initialises the OS-level I/O event poller (epoll on Linux, kqueue on macOS).
func NewBroadcaster() *Broadcaster {
	poller, err := netpoll.New(nil)
	if err != nil {
		log.Fatalf("failed to initialize epoll/kqueue netpoll: %v", err)
	}
	return &Broadcaster{poller: poller}
}

// HandleWS upgrades the HTTP connection to WebSocket and registers it with epoll.
// Clients may pass ?symbol=BTC_USDT as an initial subscription; additional
// subscribe/unsubscribe control frames can be sent at any time.
func (b *Broadcaster) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		log.Printf("WS Upgrade Error: %v", err)
		return
	}

	desc, err := netpoll.HandleRead(conn)
	if err != nil {
		log.Printf("Failed to create netpoll descriptor: %v", err)
		conn.Close()
		return
	}

	client := &Client{
		conn:   conn,
		desc:   desc,
		sendCh: make(chan []byte, clientSendBuf),
	}

	// Allow initial subscription via query parameter (e.g. ws://host/ws?symbol=BTC_USDT)
	if symbol := r.URL.Query().Get("symbol"); symbol != "" {
		client.subscribed.Store(symbol, struct{}{})
	}

	b.clients.Store(desc, client)
	atomic.AddInt64(&b.count, 1)

	// FIX #2: dedicated writer goroutine per client drains sendCh.
	// The broadcaster never writes directly to the socket, so a slow client
	// cannot stall Dispatch or starve other clients (no head-of-line blocking).
	go client.writeLoop(b)

	// FIX #3: epoll callback only detects events; actual I/O is offloaded to
	// a separate goroutine so the netpoll thread is never blocked by a single client.
	if err = b.poller.Start(desc, func(ev netpoll.Event) {
		if ev&(netpoll.EventReadHup|netpoll.EventHup) != 0 {
			b.removeClient(client)
			return
		}
		// Offload read off the netpoll event thread.
		go func() {
			msg, op, readErr := wsutil.ReadClientData(conn)
			if readErr != nil {
				b.removeClient(client)
				return
			}
			if op == ws.OpText {
				client.handleMessage(msg)
			}
		}()
	}); err != nil {
		log.Printf("Failed to start epoll poller for socket: %v", err)
		b.removeClient(client)
	}
}

// handleMessage processes subscribe/unsubscribe JSON control frames from the client.
// Frame format: {"action":"subscribe","symbol":"BTC_USDT"}
func (c *Client) handleMessage(msg []byte) {
	var m clientMsg
	if err := json.Unmarshal(msg, &m); err != nil {
		return // ignore malformed frames
	}
	switch m.Action {
	case "subscribe":
		c.subscribed.Store(m.Symbol, struct{}{})
	case "unsubscribe":
		c.subscribed.Delete(m.Symbol)
	}
}

// writeLoop is the sole goroutine allowed to write to the socket.
// It exits when sendCh is closed (triggered by removeClient).
func (c *Client) writeLoop(b *Broadcaster) {
	defer b.removeClient(c) // ensure cleanup even if write fails mid-stream
	for data := range c.sendCh {
		if err := wsutil.WriteServerMessage(c.conn, ws.OpText, data); err != nil {
			return // connection broken; defer triggers removeClient
		}
	}
}

// Dispatch fans out a serialised payload to every client subscribed to symbol.
// It is completely non-blocking: slow clients whose sendCh is full get the frame
// dropped for them alone — no other client or the calling goroutine is affected.
//
// FIX #2 & #4: non-blocking per-client push + subscription filter.
func (b *Broadcaster) Dispatch(symbol string, payload any) {
	if atomic.LoadInt64(&b.count) == 0 {
		return // nobody watching; avoid serialisation cost entirely
	}

	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Dispatch marshal error: %v", err)
		return
	}

	b.clients.Range(func(_, value any) bool {
		client := value.(*Client)

		// FIX #4: only deliver to clients that subscribed to this symbol.
		if _, ok := client.subscribed.Load(symbol); !ok {
			return true
		}

		// FIX #2: non-blocking send; drop on a lagging client rather than stalling.
		select {
		case client.sendCh <- data:
		default:
			log.Printf("[WS] slow client lagging on %s — frame dropped", symbol)
		}
		return true
	})
}

// removeClient deregisters and tears down a connection exactly once,
// even if called concurrently from writeLoop and the epoll callback.
func (b *Broadcaster) removeClient(c *Client) {
	b.poller.Stop(c.desc)
	if _, loaded := b.clients.LoadAndDelete(c.desc); loaded {
		atomic.AddInt64(&b.count, -1)
		c.closeOnce.Do(func() {
			close(c.sendCh) // signals writeLoop to exit its range loop
			c.conn.Close()
		})
	}
}
