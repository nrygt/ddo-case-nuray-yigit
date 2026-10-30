// loadtest: k6'ya eşdeğer, Go ile yazılmış karma yük testi (make load).
// Gerçekçi bir okuma/yazma karışımı üretir ve uç nokta bazında p50/p95/p99 ile
// hata oranını tablo olarak basar. k6 kurmak istemeyenler için; k6 betiği de
// loadtest/k6.js içinde.
//
//	loadtest -duration 60s -vus 50
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"sync"
	"text/tabwriter"
	"time"
	_ "time/tzdata"

	"pistkoordinat/internal/client"
)

type sample struct {
	name string
	d    time.Duration
	ok   bool
}

func main() {
	dur := flag.Duration("duration", 60*time.Second, "test süresi")
	vus := flag.Int("vus", 50, "eşzamanlı sanal kullanıcı")
	base := flag.String("api", env("API_URL", "http://localhost:8080"), "API adresi")
	md := flag.Bool("markdown", false, "README için markdown tablo")
	flag.Parse()

	ctx := context.Background()
	c := client.New(*base)
	if err := c.WaitHealthy(ctx, 2*time.Minute); err != nil {
		fmt.Println("API hazır değil:", err)
		os.Exit(2)
	}
	pw := env("DEMO_PASSWORD", "Demo.2026!")
	ops, err1 := c.Login(ctx, "ops@pist.test", pw)
	mgmt, err2 := c.Login(ctx, "yonetim@pist.test", pw)
	chief, err3 := c.Login(ctx, "sef@pist.test", pw)
	if err1 != nil || err2 != nil || err3 != nil {
		fmt.Println("giriş başarısız")
		os.Exit(2)
	}
	date := time.Now().In(loc()).Format("2006-01-02")

	// ağırlıklı senaryo: panolar ve listeler baskın, arada yazma ve analitik
	type op struct {
		name   string
		weight int
		fn     func(context.Context, *rand.Rand) (client.Response, error)
	}
	ops_ := []op{
		{"GET /flights", 30, func(ctx context.Context, r *rand.Rand) (client.Response, error) {
			return ops.Get(ctx, "/api/v1/flights?limit=50&date="+date)
		}},
		{"GET /dashboard", 20, func(ctx context.Context, r *rand.Rand) (client.Response, error) {
			return mgmt.Get(ctx, "/api/v1/dashboard?date="+date)
		}},
		{"GET /allocations", 15, func(ctx context.Context, r *rand.Rand) (client.Response, error) {
			return ops.Get(ctx, "/api/v1/allocations?date="+date)
		}},
		{"GET /tasks", 10, func(ctx context.Context, r *rand.Rand) (client.Response, error) {
			return chief.Get(ctx, "/api/v1/tasks?limit=100&date="+date)
		}},
		{"GET /reports/turnaround", 5, func(ctx context.Context, r *rand.Rand) (client.Response, error) {
			g := []string{"type", "airline", "stand"}[r.IntN(3)]
			return mgmt.Get(ctx, "/api/v1/reports/turnaround?group_by="+g)
		}},
		{"POST /replan/preview", 8, func(ctx context.Context, r *rand.Rand) (client.Response, error) {
			return ops.Post(ctx, "/api/v1/replan/preview", map[string]any{"flight_id": 2 + r.IntN(250),
				"new_time": time.Now().Add(time.Duration(r.IntN(240)) * time.Minute)})
		}},
		{"POST /notices", 12, func(ctx context.Context, r *rand.Rand) (client.Response, error) {
			return ops.Post(ctx, "/api/v1/notices", map[string]string{"message": "yük testi", "level": "info"})
		}},
	}
	total := 0
	for _, o := range ops_ {
		total += o.weight
	}

	var mu sync.Mutex
	var samples []sample
	deadline := time.Now().Add(*dur)
	var wg sync.WaitGroup
	for i := 0; i < *vus; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(i), 7))
			var local []sample
			for time.Now().Before(deadline) {
				n := rng.IntN(total)
				var o op
				for _, x := range ops_ {
					if n < x.weight {
						o = x
						break
					}
					n -= x.weight
				}
				t0 := time.Now()
				r, err := o.fn(ctx, rng)
				// 422 önizlemede beklenen bir iş kuralı yanıtı (ör. iptal uçuş); hata sayılmaz
				ok := err == nil && (r.Status < 400 || (o.name == "POST /replan/preview" && r.Status == 422))
				local = append(local, sample{o.name, time.Since(t0), ok})
			}
			mu.Lock()
			samples = append(samples, local...)
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	by := map[string][]sample{"TÜMÜ": samples}
	for _, s := range samples {
		by[s.name] = append(by[s.name], s)
	}
	names := make([]string, 0, len(by))
	for k := range by {
		names = append(names, k)
	}
	sort.Strings(names)

	fmt.Printf("Süre %s, %d VU, %d istek, %.0f istek/sn\n\n", *dur, *vus, len(samples), float64(len(samples))/dur.Seconds())
	if *md {
		fmt.Println("| Uç nokta | İstek | p50 (ms) | p95 (ms) | p99 (ms) | Hata oranı |")
		fmt.Println("|---|---:|---:|---:|---:|---:|")
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	if !*md {
		fmt.Fprintln(tw, "UÇ NOKTA\tİSTEK\tp50 ms\tp95 ms\tp99 ms\tHATA\t")
	}
	for _, n := range names {
		list := by[n]
		ds := make([]time.Duration, len(list))
		errs := 0
		for i, s := range list {
			ds[i] = s.d
			if !s.ok {
				errs++
			}
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		p := func(q float64) float64 { return float64(ds[int(q*float64(len(ds)-1))].Microseconds()) / 1000 }
		rate := float64(errs) / float64(len(list)) * 100
		if *md {
			fmt.Printf("| %s | %d | %.1f | %.1f | %.1f | %%%.2f |\n", n, len(list), p(.5), p(.95), p(.99), rate)
		} else {
			fmt.Fprintf(tw, "%s\t%d\t%.1f\t%.1f\t%.1f\t%.2f%%\t\n", n, len(list), p(.5), p(.95), p(.99), rate)
		}
	}
	tw.Flush()
}

func loc() *time.Location {
	l, err := time.LoadLocation(env("AIRPORT_TZ", "Europe/Istanbul"))
	if err != nil {
		return time.UTC
	}
	return l
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
