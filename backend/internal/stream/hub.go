// Package stream canlı operasyon panosu için SSE yayın merkezi.
//
// Her istemcinin kendi tamponlu kanalı vardır. Yavaş bir istemci tamponu
// doldurursa o istemcinin olayı düşürülür (ve sayılır); yayıncı asla bloklanmaz.
// İstemci yeniden bağlanırken Last-Event-ID gönderirse halka tampondaki
// kaçırdığı olaylar tekrar oynatılır.
package stream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"pistkoordinat/internal/metrics"
)

type Event struct {
	ID   uint64    `json:"id"`
	Type string    `json:"type"`
	At   time.Time `json:"at"`
	Data any       `json:"data"`
}

type client struct {
	ch chan Event
}

type Hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
	seq     uint64
	ring    []Event
	ringPos int
	ringLen int
	buf     int
	done    chan struct{}
	once    sync.Once
}

func NewHub(ringSize, clientBuffer int) *Hub {
	return &Hub{clients: map[*client]struct{}{}, ring: make([]Event, ringSize), buf: clientBuffer, done: make(chan struct{})}
}

// Close açık SSE bağlantılarını sonlandırır (düzgün kapanışta Shutdown'u bekletmesinler).
func (h *Hub) Close() { h.once.Do(func() { close(h.done) }) }

func (h *Hub) Publish(typ string, data any) Event {
	h.mu.Lock()
	h.seq++
	ev := Event{ID: h.seq, Type: typ, At: time.Now().UTC(), Data: data}
	h.ring[h.ringPos] = ev
	h.ringPos = (h.ringPos + 1) % len(h.ring)
	if h.ringLen < len(h.ring) {
		h.ringLen++
	}
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()

	metrics.StreamEvents.Inc(typ)
	for _, c := range clients {
		select {
		case c.ch <- ev:
		default:
			metrics.StreamDropped.Inc()
		}
	}
	return ev
}

func (h *Hub) since(id uint64) []Event {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []Event
	start := (h.ringPos - h.ringLen + len(h.ring)) % len(h.ring)
	for i := 0; i < h.ringLen; i++ {
		ev := h.ring[(start+i)%len(h.ring)]
		if ev.ID > id {
			out = append(out, ev)
		}
	}
	return out
}

func (h *Hub) subscribe() *client {
	c := &client{ch: make(chan Event, h.buf)}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	metrics.SSEClients.Add(1)
	return c
}

func (h *Hub) unsubscribe(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	metrics.SSEClients.Add(-1)
}

func (h *Hub) Clients() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// ServeHTTP SSE akışı. Olaylar küçük partiler halinde yazılıp tek flush yapılır;
// dakikada binlerce olayda sistem çağrısı sayısını düşürür.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream desteklenmiyor", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	c := h.subscribe()
	defer h.unsubscribe(c)

	fmt.Fprintf(w, "retry: 2000\n\n")
	if last, err := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64); err == nil {
		for _, ev := range h.since(last) {
			writeEvent(w, ev)
		}
	}
	fl.Flush()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.done:
			return
		case <-ping.C:
			fmt.Fprintf(w, ": ping\n\n")
			fl.Flush()
		case ev := <-c.ch:
			writeEvent(w, ev)
		drain:
			for i := 0; i < 64; i++ {
				select {
				case ev := <-c.ch:
					writeEvent(w, ev)
				default:
					break drain
				}
			}
			fl.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, ev Event) {
	b, _ := json.Marshal(ev)
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, b)
}
