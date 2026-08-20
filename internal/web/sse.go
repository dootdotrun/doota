package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dootdotrun/doot-ai/internal/store"
)

// heartbeat is how often a comment is sent to keep the connection alive.
//
// Mobile networks and intermediate proxies close idle connections, and an SSE
// stream watching a model think is idle for tens of seconds at a time. A comment
// line costs three bytes and is ignored by EventSource.
const heartbeat = 20 * time.Second

// replayLimit bounds how much history one reconnect will replay.
//
// A client that has been away for a long time is better served by reloading the
// page — which rebuilds from Postgres anyway — than by being sent thousands of
// stale frames it will immediately supersede.
const replayLimit = 500

// handleEvents streams events to one browser.
//
// Reconnection is the browser's job and it does it by itself: EventSource retries
// and sends Last-Event-ID, and everything above that id is replayed before live
// frames start. That is why structural events are persisted with ids and text
// deltas are not — see store.AppendEvent.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	// Defeats proxy buffering, which otherwise holds frames until the response ends
	// and makes streaming look broken in exactly the environments hardest to debug.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Subscribe before replaying, so a frame published during replay is queued
	// rather than lost in the gap between the two.
	clientID, frames := s.events.Subscribe()
	defer s.events.Unsubscribe(clientID)

	// A comment sent immediately, before anything has happened.
	//
	// It flushes the response so the client knows the stream is open rather than
	// merely accepted, and it gives intermediate proxies a first byte — some hold a
	// response with headers but no body, which looks exactly like a hung server.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	p, err := s.projects.Active(r.Context())
	switch {
	case errors.Is(err, store.ErrNotFound):
		// No project yet. The stream still opens: the client should not have to
		// reconnect after creating one, and heartbeats keep it alive until then.
		fmt.Fprint(w, ": no project\n\n")
		flusher.Flush()
	case err != nil:
		s.log.Error("events: load project", "error", err)
		return
	default:
		if n := s.replay(r, w, p.ID, r.Header.Get("Last-Event-ID")); n > 0 {
			s.log.Info("replayed events", "count", n, "client", clientID)
		}
		flusher.Flush()
	}

	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case frame, open := <-frames:
			if !open {
				return
			}
			writeFrame(w, frame.ID, frame.Type, frame.Data)
			flusher.Flush()

		case <-ticker.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		}
	}
}

// replay sends persisted events newer than the client's last seen id.
func (s *Server) replay(r *http.Request, w http.ResponseWriter, projectID, lastEventID string) int {
	after, err := strconv.ParseInt(strings.TrimSpace(lastEventID), 10, 64)
	if err != nil || after < 0 {
		// A first connection, or a header we cannot read. Either way there is
		// nothing to resume from: the page it belongs to was rendered from Postgres.
		return 0
	}

	rows, err := s.store.EventsSince(r.Context(), projectID, after, replayLimit)
	if err != nil {
		s.log.Error("events: replay", "error", err)
		return 0
	}
	for _, ev := range rows {
		writeFrame(w, ev.ID, ev.Type, ev.Payload)
	}
	return len(rows)
}

// writeFrame emits one SSE frame.
//
// An id is written only when the event was persisted. Without one the browser
// leaves Last-Event-ID where it was, which is what keeps an unpersisted delta from
// becoming a resume point that cannot be resumed from.
func writeFrame(w http.ResponseWriter, id int64, eventType string, data []byte) {
	if id > 0 {
		fmt.Fprintf(w, "id: %d\n", id)
	}
	fmt.Fprintf(w, "event: %s\n", eventType)

	// Data must not contain a bare newline; JSON payloads never do, but a multi-line
	// payload would silently truncate the frame, so it is split defensively.
	payload := string(data)
	if payload == "" {
		payload = "{}"
	}
	for _, line := range strings.Split(payload, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
}
