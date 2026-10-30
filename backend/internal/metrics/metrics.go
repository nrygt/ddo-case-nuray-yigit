// Package metrics Prometheus metin formatında küçük bir metrik kayıt defteri.
// Tam istemci kütüphanesini çekmek yerine ihtiyaç duyduğumuz üç tipi yazdık
// (bkz. DECISIONS.md, ADR-006).
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

type Registry struct {
	mu      sync.Mutex
	metrics []writer
}

type writer interface {
	write(w io.Writer)
	name() string
}

var Default = &Registry{}

func (r *Registry) register(m writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metrics = append(r.metrics, m)
}

func (r *Registry) WriteTo(w io.Writer) {
	r.mu.Lock()
	ms := append([]writer(nil), r.metrics...)
	r.mu.Unlock()
	sort.Slice(ms, func(i, j int) bool { return ms[i].name() < ms[j].name() })
	for _, m := range ms {
		m.write(w)
	}
}

// ---- Counter (etiketli) ----

type CounterVec struct {
	n, help string
	labels  []string
	mu      sync.Mutex
	vals    map[string]*uint64
}

func NewCounterVec(r *Registry, name, help string, labels ...string) *CounterVec {
	c := &CounterVec{n: name, help: help, labels: labels, vals: map[string]*uint64{}}
	r.register(c)
	return c
}

func (c *CounterVec) Inc(values ...string) { c.Add(1, values...) }

func (c *CounterVec) Add(d uint64, values ...string) {
	key := labelKey(c.labels, values)
	c.mu.Lock()
	p, ok := c.vals[key]
	if !ok {
		p = new(uint64)
		c.vals[key] = p
	}
	c.mu.Unlock()
	atomic.AddUint64(p, d)
}

func (c *CounterVec) Value(values ...string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.vals[labelKey(c.labels, values)]; ok {
		return atomic.LoadUint64(p)
	}
	return 0
}

func (c *CounterVec) name() string { return c.n }

func (c *CounterVec) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.n, c.help, c.n)
	c.mu.Lock()
	keys := make([]string, 0, len(c.vals))
	for k := range c.vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s%s %d\n", c.n, k, atomic.LoadUint64(c.vals[k]))
	}
	c.mu.Unlock()
}

// ---- Gauge ----

type Gauge struct {
	n, help string
	v       int64
}

func NewGauge(r *Registry, name, help string) *Gauge {
	g := &Gauge{n: name, help: help}
	r.register(g)
	return g
}

func (g *Gauge) Add(d int64)    { atomic.AddInt64(&g.v, d) }
func (g *Gauge) Set(v int64)    { atomic.StoreInt64(&g.v, v) }
func (g *Gauge) Get() int64     { return atomic.LoadInt64(&g.v) }
func (g *Gauge) name() string   { return g.n }
func (g *Gauge) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", g.n, g.help, g.n, g.n, g.Get())
}

// ---- Histogram (etiketli) ----

type HistogramVec struct {
	n, help string
	labels  []string
	buckets []float64
	mu      sync.Mutex
	series  map[string]*hist
}

type hist struct {
	counts []uint64
	sum    float64
	count  uint64
}

var DefaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

func NewHistogramVec(r *Registry, name, help string, buckets []float64, labels ...string) *HistogramVec {
	h := &HistogramVec{n: name, help: help, labels: labels, buckets: buckets, series: map[string]*hist{}}
	r.register(h)
	return h
}

func (h *HistogramVec) Observe(v float64, values ...string) {
	key := labelKey(h.labels, values)
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.series[key]
	if !ok {
		s = &hist{counts: make([]uint64, len(h.buckets))}
		h.series[key] = s
	}
	for i, b := range h.buckets {
		if v <= b {
			s.counts[i]++
		}
	}
	s.sum += v
	s.count++
}

func (h *HistogramVec) name() string { return h.n }

func (h *HistogramVec) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", h.n, h.help, h.n)
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := make([]string, 0, len(h.series))
	for k := range h.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := h.series[k]
		inner := strings.TrimSuffix(strings.TrimPrefix(k, "{"), "}")
		sep := ""
		if inner != "" {
			sep = ","
		}
		for i, b := range h.buckets {
			fmt.Fprintf(w, "%s_bucket{%s%sle=\"%s\"} %d\n", h.n, inner, sep, fmtFloat(b), s.counts[i])
		}
		fmt.Fprintf(w, "%s_bucket{%s%sle=\"+Inf\"} %d\n", h.n, inner, sep, s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n%s_count%s %d\n", h.n, k, fmtFloat(s.sum), h.n, k, s.count)
	}
}

func labelKey(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		v := ""
		if i < len(values) {
			v = values[i]
		}
		v = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
		fmt.Fprintf(&b, `%s="%s"`, n, v)
	}
	b.WriteByte('}')
	return b.String()
}

func fmtFloat(f float64) string {
	if math.IsInf(f, 1) {
		return "+Inf"
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", f), "0"), ".")
}

// Uygulama metrikleri
var (
	HTTPRequests  = NewCounterVec(Default, "pk_http_requests_total", "HTTP istek sayısı", "method", "route", "status")
	HTTPDuration  = NewHistogramVec(Default, "pk_http_request_duration_seconds", "HTTP istek süresi", DefaultBuckets, "method", "route")
	SSEClients    = NewGauge(Default, "pk_sse_clients", "Bağlı SSE istemcisi")
	StreamEvents  = NewCounterVec(Default, "pk_stream_events_total", "Panoya yayınlanan olaylar", "type")
	StreamDropped = NewCounterVec(Default, "pk_stream_events_dropped_total", "Yavaş istemci nedeniyle düşen olaylar")
	FeedEvents    = NewCounterVec(Default, "pk_feed_events_total", "Uçuş beslemesi olayları", "outcome")
	SyncEvents    = NewCounterVec(Default, "pk_sync_events_total", "Mobil senkronizasyon olayları", "result")
)
