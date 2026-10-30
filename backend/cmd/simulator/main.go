// simulator: canlı veri üretici. Seed'deki çizelgenin deterministik besleme
// senaryosunu (gecikme, iptal, tekrar, sıra bozukluğu) API'ye partiler halinde
// gönderir; ekip liderlerinin tabletlerini taklit ederek görev olayları üretir.
// Bazı tabletler belirli aralıklarla "çevrimdışı" kalır, olaylarını biriktirip
// geri gelince karışık sırada gönderir.
//
// Ortam değişkenleri: API_URL, SEED, SIM_INTERVAL (parti aralığı, varsayılan 3s),
// SIM_PASSWORD, AIRPORT_TZ, SEED_DATE.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"pistkoordinat/internal/client"
	"pistkoordinat/internal/domain/feed"
	"pistkoordinat/internal/domain/tasksync"
	"pistkoordinat/internal/sim"
)

type device struct {
	id      string
	api     *client.Client
	crewID  int // 0: şef (tüm görevler)
	offline int // kalan çevrimdışı tur sayısı
	queue   []tasksync.Event
	known   []taskView     // son çevrimiçi okumada görülen görevler (çevrimdışıyken bunlarla çalışır)
	sent    map[int64]bool // bu cihazın zaten olay ürettiği görev+durum
}

type taskView struct {
	ID           int64     `json:"id"`
	Status       string    `json:"status"`
	CrewID       *int      `json:"crew_id"`
	PlannedStart time.Time `json:"planned_start"`
	PlannedEnd   time.Time `json:"planned_end"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		// simülatörün dış bağımlılığı API; API sağlıklıysa simülatör de iş yapabiliyor
		c := client.New(env("API_URL", "http://api:8080"))
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if r, err := c.Get(ctx, "/health"); err != nil || r.Status != 200 {
			os.Exit(1)
		}
		return
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	seed, _ := strconv.ParseInt(env("SEED", "42"), 10, 64)
	interval, err := time.ParseDuration(env("SIM_INTERVAL", "3s"))
	if err != nil {
		interval = 3 * time.Second
	}
	loc, err := time.LoadLocation(env("AIRPORT_TZ", "Europe/Istanbul"))
	if err != nil {
		loc = time.UTC
	}
	day := today(loc)
	if d := os.Getenv("SEED_DATE"); d != "" {
		if t, err := time.ParseInLocation("2006-01-02", d, loc); err == nil {
			day = t
		}
	}
	pw := env("SIM_PASSWORD", "Demo.2026!")
	base := client.New(env("API_URL", "http://api:8080"))

	if err := base.WaitHealthy(ctx, 5*time.Minute); err != nil {
		slog.Error("api hazır değil", "err", err)
		os.Exit(1)
	}
	ops := mustLogin(ctx, base, "ops@pist.test", pw)
	devices := []*device{{id: "sef-tablet", api: mustLogin(ctx, base, "sef@pist.test", pw)}}
	for _, lead := range []string{"ekip@pist.test", "ekip2@pist.test", "ekip3@pist.test"} {
		c := mustLogin(ctx, base, lead, pw)
		var me struct {
			CrewID int `json:"crew_id"`
		}
		if r, err := c.Get(ctx, "/api/v1/me"); err == nil {
			_ = r.JSON(&me)
		}
		devices = append(devices, &device{id: fmt.Sprintf("tablet-ekip-%d", me.CrewID), api: c, crewID: me.CrewID})
	}

	rng := rand.New(rand.NewPCG(uint64(seed), 99))
	batches := sim.FeedScript(seed, day, sim.Schedule(seed, day, 300))
	var actuals []feed.Event // gerçekleşen saatler, zamanı gelince gönderilir
	slog.Info("simülatör başladı", "op_date", day.Format("2006-01-02"), "seed", seed, "feed_batches", len(batches),
		"fingerprint", sim.Fingerprint(batches))

	tick := time.NewTicker(interval)
	defer tick.Stop()
	round := 0
	for {
		select {
		case <-ctx.Done():
			slog.Info("simülatör durdu")
			return
		case <-tick.C:
		}
		round++

		// 1) besleme partisi
		if len(batches) > 0 {
			var send []feed.Event
			for _, e := range batches[0] {
				if e.Kind == feed.Actual && e.NewTime.After(time.Now()) {
					actuals = append(actuals, e)
					continue
				}
				send = append(send, e)
			}
			batches = batches[1:]
			postFeed(ctx, ops, send)
		}
		// 2) zamanı gelen gerçekleşen saatler
		var due []feed.Event
		rest := actuals[:0]
		for _, e := range actuals {
			if !e.NewTime.After(time.Now()) {
				due = append(due, e)
			} else {
				rest = append(rest, e)
			}
		}
		actuals = rest
		if len(due) > 0 {
			postFeed(ctx, ops, due)
		}

		// 3) tabletler: görev olayları, çevrimdışı/çevrimiçi geçişleri
		for _, d := range devices {
			produceTaskEvents(ctx, d, day, rng)
			if d.offline > 0 {
				d.offline--
				if d.offline == 0 {
					rng.Shuffle(len(d.queue), func(i, j int) { d.queue[i], d.queue[j] = d.queue[j], d.queue[i] })
					flush(ctx, d, "yeniden bağlandı")
				}
				continue
			}
			if d.crewID != 0 && rng.IntN(12) == 0 {
				d.offline = 3 + rng.IntN(6)
				slog.Info("tablet çevrimdışı", "device", d.id, "rounds", d.offline)
				continue
			}
			flush(ctx, d, "")
		}
		if round%20 == 0 {
			_, _ = ops.Post(ctx, "/api/v1/notices", map[string]string{"message": fmt.Sprintf("Simülatör turu %d: %d tablet çevrimdışı", round, countOffline(devices)), "level": "info"})
		}
	}
}

func produceTaskEvents(ctx context.Context, d *device, day time.Time, rng *rand.Rand) {
	if d.sent == nil {
		d.sent = map[int64]bool{}
	}
	if d.offline == 0 {
		path := "/api/v1/tasks?limit=500&date=" + day.Format("2006-01-02")
		if d.crewID != 0 {
			path = "/api/v1/my/tasks?limit=500&date=" + day.Format("2006-01-02")
		}
		r, err := d.api.Get(ctx, path)
		if err != nil || r.Status != 200 {
			return
		}
		var page struct {
			Items []taskView `json:"items"`
		}
		if r.JSON(&page) != nil {
			return
		}
		d.known = page.Items
	}
	now := time.Now()
	n := 0
	for i := range d.known {
		k := &d.known[i]
		if n >= 15 {
			break
		}
		// şef çoğunlukla lideri olmayan ekiplerin görevlerini işler; arada bir liderli
		// ekibin görevine de dokunur, böylece iki cihazdan çakışan güncelleme oluşur.
		if d.crewID == 0 && k.CrewID != nil && *k.CrewID <= 3 && rng.IntN(5) != 0 {
			continue
		}
		jitter := time.Duration(rng.IntN(240)-60) * time.Second
		switch {
		case k.Status == "ASSIGNED" && !k.PlannedStart.Add(jitter).After(now):
			d.queue = append(d.queue, tasksync.Event{ClientEventID: uuid(rng), TaskID: k.ID, Type: tasksync.Started,
				OccurredAt: minTime(k.PlannedStart.Add(jitter), now)})
			k.Status = "STARTED" // yerel görünüm (çevrimdışıyken de ilerler)
			n++
		case k.Status == "STARTED" && !k.PlannedEnd.Add(jitter).After(now):
			d.queue = append(d.queue, tasksync.Event{ClientEventID: uuid(rng), TaskID: k.ID, Type: tasksync.Finished,
				OccurredAt: minTime(k.PlannedEnd.Add(jitter), now)})
			k.Status = "DONE"
			n++
		}
	}
}

func flush(ctx context.Context, d *device, why string) {
	if len(d.queue) == 0 {
		return
	}
	for len(d.queue) > 0 {
		n := min(len(d.queue), 200)
		r, err := d.api.Post(ctx, "/api/v1/sync/task-events", map[string]any{"device_id": d.id, "events": d.queue[:n]})
		if err != nil || r.Status >= 500 {
			slog.Warn("senkronizasyon başarısız, tekrar denenecek", "device", d.id, "err", err)
			return // kuyruk korunur
		}
		var out struct {
			Results []struct{ Result string } `json:"results"`
		}
		_ = r.JSON(&out)
		counts := map[string]int{}
		for _, x := range out.Results {
			counts[x.Result]++
		}
		if why != "" || counts["MERGED"] > 0 || counts["REJECTED"] > 0 {
			slog.Info("senkronizasyon", "device", d.id, "events", n, "results", counts, "note", why)
		}
		d.queue = d.queue[n:]
	}
}

func postFeed(ctx context.Context, ops *client.Client, evs []feed.Event) {
	if len(evs) == 0 {
		return
	}
	r, err := ops.Post(ctx, "/api/v1/feed/events", map[string]any{"events": evs})
	if err != nil {
		slog.Warn("besleme gönderilemedi", "err", err)
		return
	}
	var out struct {
		Summary map[string]int `json:"summary"`
	}
	_ = r.JSON(&out)
	slog.Info("besleme partisi", "events", len(evs), "status", r.Status, "summary", out.Summary)
}

func mustLogin(ctx context.Context, c *client.Client, email, pw string) *client.Client {
	for i := 0; ; i++ {
		lc, err := c.Login(ctx, email, pw)
		if err == nil {
			return lc
		}
		if i > 30 {
			slog.Error("giriş yapılamadı", "email", email, "err", err)
			os.Exit(1)
		}
		time.Sleep(2 * time.Second)
	}
}

func uuid(rng *rand.Rand) string {
	a, b := rng.Uint64(), rng.Uint64()
	return fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x", a>>32, (a>>16)&0xffff, a&0xfff, b>>52&0xfff, b&0xffffffffffff)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func countOffline(ds []*device) int {
	n := 0
	for _, d := range ds {
		if d.offline > 0 {
			n++
		}
	}
	return n
}

func today(loc *time.Location) time.Time {
	n := time.Now().In(loc)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
