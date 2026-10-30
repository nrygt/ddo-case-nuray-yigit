package metrics

import (
	"strings"
	"testing"
)

func TestExpositionFormat(t *testing.T) {
	r := &Registry{}
	c := NewCounterVec(r, "t_requests_total", "istekler", "route", "status")
	c.Inc("GET /x", "200")
	c.Add(2, "GET /x", "200")
	g := NewGauge(r, "t_clients", "istemci")
	g.Set(3)
	h := NewHistogramVec(r, "t_dur_seconds", "süre", []float64{0.1, 1}, "route")
	h.Observe(0.05, "a")
	h.Observe(0.5, "a")

	var b strings.Builder
	r.Write(&b)
	out := b.String()
	for _, want := range []string{
		`t_requests_total{route="GET /x",status="200"} 3`,
		"# TYPE t_clients gauge\nt_clients 3",
		`t_dur_seconds_bucket{route="a",le="0.1"} 1`,
		`t_dur_seconds_bucket{route="a",le="1"} 2`,
		`t_dur_seconds_bucket{route="a",le="+Inf"} 2`,
		`t_dur_seconds_count{route="a"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("çıktıda yok: %q\n%s", want, out)
		}
	}
	if c.Value("GET /x", "200") != 3 {
		t.Fatal("sayaç değeri")
	}
}

func TestLabelEscaping(t *testing.T) {
	if got := labelKey([]string{"a"}, []string{`x"y`}); got != `{a="x\"y"}` {
		t.Fatalf("%s", got)
	}
}
