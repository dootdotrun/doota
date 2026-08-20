// Package events fans server-side events out to connected browsers.
//
// Its own package rather than part of internal/web because both the web layer and
// the agent loop need it, and the agent must not import the web layer.
//
// One hub, in process. There is one machine by design, so there is no
// cross-instance pub/sub to arrange and no broker to operate.
package events

import (
	"log/slog"
	"sync"
)

// Frame is one event delivered to a subscriber.
//
// ID is the persisted event row id, or 0 for a frame that was never written down.
// That distinction is the whole reconnection story: a frame with an id is sent to
// the browser as an SSE id and comes back as Last-Event-ID, so the server can
// resume from it. Text deltas carry no id, which leaves the browser's resume point
// pinned to the last structural event — exactly right, because the text itself is
// recoverable from the message row and the animation is not worth replaying.
type Frame struct {
	ID   int64
	Type string
	Data []byte
}

// subscriberBuffer is how many frames a slow client may fall behind by.
//
// Generous, because the cost of a frame is bytes and the cost of blocking the agent
// loop on a stalled phone is the whole run. When it overflows the frame is dropped
// rather than queued forever; the client reconciles from Postgres on the next
// structural event, so a dropped frame delays the UI rather than corrupting it.
const subscriberBuffer = 512

// Hub broadcasts frames to every current subscriber.
type Hub struct {
	log *slog.Logger

	mu      sync.Mutex
	next    int64
	clients map[int64]chan Frame
	dropped int64
}

// NewHub builds an empty hub.
func NewHub(log *slog.Logger) *Hub {
	return &Hub{log: log, clients: map[int64]chan Frame{}}
}

// Subscribe registers a client and returns its id and channel.
//
// The caller must Unsubscribe, or the hub leaks a channel and keeps trying to
// deliver to a browser that has gone.
func (h *Hub) Subscribe() (int64, <-chan Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.next++
	id := h.next
	ch := make(chan Frame, subscriberBuffer)
	h.clients[id] = ch
	return id, ch
}

// Unsubscribe removes a client and closes its channel.
func (h *Hub) Unsubscribe(id int64) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if ch, found := h.clients[id]; found {
		delete(h.clients, id)
		close(ch)
	}
}

// Publish delivers a frame to every subscriber.
//
// Never blocks. The agent loop calls this while streaming, and a phone on a bad
// connection must not be able to slow the work down, let alone stop it.
func (h *Hub) Publish(f Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for id, ch := range h.clients {
		select {
		case ch <- f:
		default:
			h.dropped++
			h.log.Warn("dropped event frame for a slow client",
				"client", id, "type", f.Type, "dropped_total", h.dropped)
		}
	}
}
