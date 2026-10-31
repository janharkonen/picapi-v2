package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
)

// Asset change events, streamed to the frontend so its views update live. Events are
// in-process only, which is enough for a single-instance deployment.

type assetEvent struct {
	Type string `json:"type"` // "created" or "deleted"
	ID   string `json:"id"`
}

type eventHub struct {
	mu   sync.Mutex
	subs map[chan assetEvent]struct{}
}

func (h *eventHub) subscribe() chan assetEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs == nil {
		h.subs = map[chan assetEvent]struct{}{}
	}
	ch := make(chan assetEvent, 16)
	h.subs[ch] = struct{}{}
	return ch
}

func (h *eventHub) unsubscribe(ch chan assetEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, ch)
}

// publish never blocks: a subscriber that has fallen 16 events behind misses events,
// which is harmless because every event only tells it to reload.
func (h *eventHub) publish(e assetEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// streamEvents serves asset events as Server-Sent Events, with a heartbeat comment so
// proxies keep the connection open.
func (s *Server) streamEvents(c *echo.Context) error {
	w := c.Response()
	rc := http.NewResponseController(w)

	ch := s.events.subscribe()
	defer s.events.unsubscribe(ch)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	if err := rc.Flush(); err != nil {
		return nil
	}

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-c.Request().Context().Done():
			return nil
		case <-heartbeat.C:
			fmt.Fprint(w, ": heartbeat\n\n")
		case e := <-ch:
			data, _ := json.Marshal(e)
			fmt.Fprintf(w, "event: asset\ndata: %s\n\n", data)
		}
		if err := rc.Flush(); err != nil {
			return nil
		}
	}
}
