package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Event is the canonical envelope published to every Redis Pub/Sub channel.
// Both publisher (Exchange Engine) and subscriber (WS Gateway) use this struct,
// ensuring the wire format the frontend already expects is preserved end-to-end:
//
//	{ "type": "depth", "symbol": "BTC_USDT", "data": { ... } }
type Event struct {
	Type   string `json:"type"`
	Symbol string `json:"symbol"`
	Data   any    `json:"data"`
}

// RedisBus is the Pub/Sub adapter that decouples the Exchange Engine from
// WebSocket gateway processes.
//
// Scaling model:
//   - ONE Exchange process (per shard) PUBLISHES to Redis channels.
//   - N WS Gateway processes each SUBSCRIBE to all channels and dispatch
//     to their own locally-connected epoll clients.
//
// Channel naming convention:
//
//	"depth:{symbol}"  — orderbook snapshot after any change
//	"match:{symbol}"  — individual trade execution
type RedisBus struct {
	client *redis.Client
}

// NewRedisBus creates a single Redis client shared between publish and subscribe
// operations. In production you may want separate clients (or separate Redis
// nodes) for pub and sub to avoid head-of-line blocking on the connection.
func NewRedisBus(addr string) *RedisBus {
	client := redis.NewClient(&redis.Options{
		Addr:            addr,
		PoolSize:        5, // PubSub only needs a few connections
		MinIdleConns:    1,
		MaxIdleConns:    3,
		ConnMaxIdleTime: 30 * time.Second,
		DialTimeout:     2 * time.Second,
		ReadTimeout:     50 * time.Millisecond,
		WriteTimeout:    50 * time.Millisecond,
	})
	return &RedisBus{client: client}
}

// channelFor returns the Redis Pub/Sub channel name.
// e.g. channelFor("depth", "BTC_USDT") → "depth:BTC_USDT"
func channelFor(eventType, symbol string) string {
	return fmt.Sprintf("%s:%s", eventType, symbol)
}

// Publish serialises the event envelope and pushes it onto the Redis channel.
// This is called by the Exchange Engine fanout goroutine (~0.5ms overhead).
// It is intentionally fire-and-forget: if Redis is unavailable, the event is
// dropped rather than blocking the hot path.
func (b *RedisBus) Publish(ctx context.Context, symbol, eventType string, data any) {
	evt := Event{Type: eventType, Symbol: symbol, Data: data}
	payload, err := json.Marshal(evt)
	if err != nil {
		log.Printf("[REDISBUS] marshal error (%s/%s): %v", eventType, symbol, err)
		return
	}
	ch := channelFor(eventType, symbol)
	if err := b.client.Publish(ctx, ch, payload).Err(); err != nil {
		log.Printf("[REDISBUS] publish error on %s: %v", ch, err)
	}
}

// StartSubscriber spawns a background goroutine that pattern-subscribes to
// ALL depth and match channels ("depth:*", "match:*") and fans each incoming
// event out to the local Broadcaster.
//
// Run this ONCE per WS gateway process. When ctx is cancelled (graceful
// shutdown), the goroutine exits cleanly.
func (b *RedisBus) StartSubscriber(ctx context.Context, broadcaster *Broadcaster) {
	pubsub := b.client.PSubscribe(ctx, "depth:*", "match:*")

	go func() {
		defer pubsub.Close()
		ch := pubsub.Channel()
		log.Printf("[REDISBUS] Subscriber ready — listening on depth:* match:*")

		for {
			select {
			case msg, ok := <-ch:
				if !ok {
					log.Printf("[REDISBUS] subscriber channel closed")
					return
				}

				// Deserialise the Event envelope.
				var evt Event
				if err := json.Unmarshal([]byte(msg.Payload), &evt); err != nil {
					log.Printf("[REDISBUS] unmarshal error on %s: %v", msg.Channel, err)
					continue
				}

				// Extract symbol from channel name: "depth:BTC_USDT" → "BTC_USDT"
				parts := strings.SplitN(msg.Channel, ":", 2)
				if len(parts) != 2 {
					continue
				}
				symbol := parts[1]

				// Dispatch to all locally-connected WS clients subscribed to symbol.
				// broadcaster.Dispatch is non-blocking (per-client channel), so this
				// goroutine is never stalled by slow clients.
				broadcaster.Dispatch(symbol, evt)

			case <-ctx.Done():
				log.Printf("[REDISBUS] subscriber shutting down")
				return
			}
		}
	}()
}
