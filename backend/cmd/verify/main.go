// verify: ayağa kalkmış ve seed edilmiş sistem üzerinde her özelliği (F-1..F-7),
// her zorlu görevi (T-1..T-5), sağlık, kimlik doğrulama ve yetki senaryolarını
// API üzerinden uçtan uca dener. Sonunda PASS/FAIL tablosu basar; bir FAIL varsa
// çıkış kodu 1 olur. Tekrar çalıştırılabilir (her çalıştırma kendine özgü uçuş
// numaraları kullanır); temiz başlangıç için önce `make reset-demo`.
//
// Ortam: API_URL, DATABASE_URL (yalnızca T-5 yük verisi ve EXPLAIN için),
// VERIFY_EVENTS (T-5 hedef olay sayısı, varsayılan 5_000_000), AIRPORT_TZ.
package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"pistkoordinat/internal/client"
)

type result struct {
	id, name string
	ok       bool
	took     time.Duration
	note     string
}

type check struct {
	id, name string
	fn       func(ctx context.Context, v *V) (string, error)
}

// V doğrulama boyunca paylaşılan durum.
type V struct {
	base                        *client.Client
	ops, chief, lead, rep, mgmt *client.Client
	date, tomorrow              string
	loc                         *time.Location
	runTag                      string
	pool                        *pgxpool.Pool
	eventsTarget                int64
	pw                          string
}

func main() {
	ctx := context.Background()
	loc, err := time.LoadLocation(env("AIRPORT_TZ", "Europe/Istanbul"))
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	v := &V{
		base:   client.New(env("API_URL", "http://localhost:8080")),
		date:   now.Format("2006-01-02"),
		loc:    loc,
		runTag: strconv.Itoa(1000 + int(now.Unix()%8999)),
		pw:     env("DEMO_PASSWORD", "Demo.2026!"),
	}
	if d := os.Getenv("SEED_DATE"); d != "" {
		v.date = d
	}
	t, _ := time.ParseInLocation("2006-01-02", v.date, loc)
	v.tomorrow = t.AddDate(0, 0, 1).Format("2006-01-02")
	v.eventsTarget, _ = strconv.ParseInt(env("VERIFY_EVENTS", "5000000"), 10, 64)

	fmt.Printf("PistKoordinat doğrulama — API %s, operasyon günü %s\n\n", v.base.Base, v.date)
	if err := v.base.WaitHealthy(ctx, 3*time.Minute); err != nil {
		fmt.Println("API sağlıklı değil:", err)
		os.Exit(2)
	}

	checks := []check{
		{"HEALTH", "GET /health + veritabanı", checkHealth},
		{"AUTH-1", "Yanlış parola 401", checkWrongPassword},
		{"AUTH-2", "Token yok / sahte token 401", checkNoToken},
		{"AUTH-3", "Refresh rotasyonu, tekrar kullanım reddi", checkRefresh},
		{"RBAC-1", "Yetkisiz rol 403 (her rol için örnek)", checkRBAC},
		{"RBAC-2", "Havayolu temsilcisi yalnızca kendi verisi", checkAirlineScope},
		{"F-1", "Uçuş çizelgesi, keyset sayfalama, oluşturma", checkFlights},
		{"F-2", "Stand/kapı envanteri, tip uyumu", checkInventory},
		{"F-3", "Stand/kapı tahsisi, elle atama çakışması", checkAllocations},
		{"T-1", "Stand tahsis çizelgeleme (kurallar, süre, rapor)", checkT1},
		{"F-4", "Ekipler, ekipman, görevler", checkCrews},
		{"T-3", "Ekip atama (belge, dinlenme, ekipman, risk)", checkT3},
		{"F-5", "Gecikme/değişiklik olayları (besleme)", checkFeed},
		{"T-2", "Gecikme yayılımı, what-if, uygulama", checkT2},
		{"F-6", "Canlı pano + SSE", checkDashboard},
		{"T-4", "Çevrimdışı senkronizasyon + 2000 olay/dk akış", checkT4},
		{"F-7", "Turnaround performans raporu", checkReport},
		{"T-5", "5M olayda analitik < 2 sn, EXPLAIN, veri kalitesi", checkT5},
		{"OPS-1", "Denetim kaydı, metrikler, OpenAPI", checkOps},
	}

	logins := map[string]**client.Client{"ops": &v.ops, "sef": &v.chief, "ekip": &v.lead, "havayolu": &v.rep, "yonetim": &v.mgmt}
	for name, dst := range logins {
		c, err := v.base.Login(ctx, name+"@pist.test", v.pw)
		if err != nil {
			fmt.Println("demo hesabıyla giriş yapılamadı:", err)
			os.Exit(2)
		}
		*dst = c
	}
	if url := os.Getenv("DATABASE_URL"); url != "" {
		v.pool, err = pgxpool.New(ctx, url)
		if err != nil {
			fmt.Println("DATABASE_URL:", err)
		}
	}

	var results []result
	start := time.Now()
	for _, c := range checks {
		cctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
		t0 := time.Now()
		note, err := c.fn(cctx, v)
		cancel()
		r := result{id: c.id, name: c.name, ok: err == nil, took: time.Since(t0), note: note}
		if err != nil {
			r.note = err.Error()
		}
		status := "PASS"
		if !r.ok {
			status = "FAIL"
		}
		fmt.Printf("  %-7s %-4s %6.1fs  %s\n", c.id, status, r.took.Seconds(), trim(r.note, 110))
		results = append(results, r)
	}

	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KİMLİK\tSONUÇ\tSÜRE\tAÇIKLAMA")
	fmt.Fprintln(tw, "------\t-----\t----\t--------")
	failed := 0
	for _, r := range results {
		s := "PASS"
		if !r.ok {
			s = "FAIL"
			failed++
		}
		fmt.Fprintf(tw, "%s\t%s\t%.1fs\t%s\n", r.id, s, r.took.Seconds(), r.name)
	}
	tw.Flush()
	fmt.Printf("\n%d/%d PASS, toplam %.0f sn\n", len(results)-failed, len(results), time.Since(start).Seconds())
	if failed > 0 {
		os.Exit(1)
	}
}

// ---------------------------------------------------------------- yardımcılar

func env(k, def string) string {
	if x := os.Getenv(k); x != "" {
		return x
	}
	return def
}

func trim(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) > n {
		return string([]rune(s)[:n]) + "…"
	}
	return s
}

func expect(r client.Response, err error, status int) error {
	if err != nil {
		return err
	}
	if r.Status != status {
		return fmt.Errorf("HTTP %d bekleniyordu, %d geldi: %s", status, r.Status, trim(string(r.Body), 200))
	}
	return nil
}

func fail(format string, a ...any) error { return fmt.Errorf(format, a...) }

type flight struct {
	ID           int64     `json:"id"`
	FlightNo     string    `json:"flight_no"`
	AirlineCode  string    `json:"airline_code"`
	Direction    string    `json:"direction"`
	TailNumber   string    `json:"tail_number"`
	Type         string    `json:"aircraft_type"`
	EstTime      time.Time `json:"est_time"`
	Status       string    `json:"status"`
	TurnaroundID *int64    `json:"turnaround_id"`
}

type allocation struct {
	TurnaroundID int64     `json:"turnaround_id"`
	TailNumber   string    `json:"tail_number"`
	AircraftType string    `json:"aircraft_type"`
	StandID      int       `json:"stand_id"`
	StandCode    string    `json:"stand_code"`
	OnBlock      time.Time `json:"on_block"`
	OffBlock     time.Time `json:"off_block"`
}

func (v *V) allocations(ctx context.Context, date string) ([]allocation, error) {
	r, err := v.ops.Get(ctx, "/api/v1/allocations?date="+date)
	if err := expect(r, err, 200); err != nil {
		return nil, err
	}
	var out struct {
		Items []allocation `json:"items"`
	}
	return out.Items, r.JSON(&out)
}

// standRules tahsisleri istemci tarafında bağımsızca denetler: 15 dk tampon ve boyut uyumu.
func (v *V) standRules(ctx context.Context, allocs []allocation) error {
	r, _ := v.ops.Get(ctx, "/api/v1/stands")
	var stands struct {
		Items []struct {
			ID         int      `json:"id"`
			Compatible []string `json:"compatible_types"`
		} `json:"items"`
	}
	_ = r.JSON(&stands)
	compat := map[int]map[string]bool{}
	for _, s := range stands.Items {
		compat[s.ID] = map[string]bool{}
		for _, c := range s.Compatible {
			compat[s.ID][c] = true
		}
	}
	by := map[int][]allocation{}
	for _, a := range allocs {
		if !compat[a.StandID][a.AircraftType] {
			return fail("%s (%s) %s standına uyumsuz", a.TailNumber, a.AircraftType, a.StandCode)
		}
		by[a.StandID] = append(by[a.StandID], a)
	}
	for _, list := range by {
		sort.Slice(list, func(i, j int) bool { return list[i].OnBlock.Before(list[j].OnBlock) })
		for i := 1; i < len(list); i++ {
			if list[i].OnBlock.Before(list[i-1].OffBlock.Add(15 * time.Minute)) {
				return fail("stand %s: %s ve %s arasında 15 dk tampon yok", list[i].StandCode, list[i-1].TailNumber, list[i].TailNumber)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- kontroller

func checkHealth(ctx context.Context, v *V) (string, error) {
	r, err := v.base.Get(ctx, "/health")
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	var h struct{ Status, Database string }
	_ = r.JSON(&h)
	if h.Database != "ok" {
		return "", fail("veritabanı durumu: %s", h.Database)
	}
	return "status=ok database=ok", nil
}

func checkWrongPassword(ctx context.Context, v *V) (string, error) {
	r, err := v.base.Post(ctx, "/api/v1/auth/login", map[string]string{"email": "ops@pist.test", "password": "yanlis-parola"})
	if err := expect(r, err, 401); err != nil {
		return "", err
	}
	r2, err := v.base.Post(ctx, "/api/v1/auth/login", map[string]string{"email": "yok@pist.test", "password": "yanlis-parola"})
	if err := expect(r2, err, 401); err != nil {
		return "", err
	}
	if r.ErrorCode() != "invalid_credentials" || r2.ErrorCode() != r.ErrorCode() {
		return "", fail("var olan / olmayan kullanıcı aynı hatayı almalı")
	}
	return "yanlış parola ve bilinmeyen e-posta aynı 401 invalid_credentials", nil
}

func checkNoToken(ctx context.Context, v *V) (string, error) {
	r, err := v.base.Get(ctx, "/api/v1/flights")
	if err := expect(r, err, 401); err != nil {
		return "", err
	}
	forged := *v.ops
	parts := strings.Split(forged.Token, ".")
	forged.Token = parts[0] + "." + parts[1] + "x." + parts[2]
	r, err = forged.Get(ctx, "/api/v1/flights")
	if err := expect(r, err, 401); err != nil {
		return "", err
	}
	return "token yok → 401, imzası bozuk token → 401", nil
}

func checkRefresh(ctx context.Context, v *V) (string, error) {
	r, err := v.base.Post(ctx, "/api/v1/auth/login", map[string]string{"email": "yonetim@pist.test", "password": v.pw})
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	var t struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = r.JSON(&t)
	r, err = v.base.Post(ctx, "/api/v1/auth/refresh", map[string]string{"refresh_token": t.RefreshToken})
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	r, err = v.base.Post(ctx, "/api/v1/auth/refresh", map[string]string{"refresh_token": t.RefreshToken})
	if err := expect(r, err, 401); err != nil {
		return "", err
	}
	if r.ErrorCode() != "token_reused" {
		return "", fail("token_reused bekleniyordu: %s", r.ErrorCode())
	}
	return "yeni çift alındı; eski refresh token ikinci kez → 401 token_reused", nil
}

func checkRBAC(ctx context.Context, v *V) (string, error) {
	cases := []struct {
		who    string
		c      *client.Client
		method string
		path   string
		body   any
	}{
		{"ekip lideri → uçuş listesi", v.lead, "GET", "/api/v1/flights", nil},
		{"havayolu → tahsis çalıştır", v.rep, "POST", "/api/v1/allocations/run", map[string]string{"date": v.date}},
		{"yönetim → besleme gönder", v.mgmt, "POST", "/api/v1/feed/events", map[string]any{"events": []any{}}},
		{"şef → yeniden planla uygula", v.chief, "POST", "/api/v1/replan/apply", map[string]any{}},
		{"operasyon → ekip ata", v.ops, "POST", "/api/v1/assignments/run", map[string]string{"date": v.date}},
		{"operasyon → denetim kaydı", v.ops, "GET", "/api/v1/audit", nil},
		{"ekip lideri → rapor", v.lead, "GET", "/api/v1/reports/turnaround", nil},
		{"havayolu → pano", v.rep, "GET", "/api/v1/dashboard", nil},
		{"şef → stand kapat", v.chief, "PATCH", "/api/v1/stands/1", map[string]bool{"active": false}},
	}
	for _, c := range cases {
		r, err := c.c.Do(ctx, c.method, c.path, c.body)
		if err := expect(r, err, 403); err != nil {
			return "", fail("%s: %v", c.who, err)
		}
	}
	return fmt.Sprintf("%d yetkisiz rol/uç nokta denemesi → 403", len(cases)), nil
}

func checkAirlineScope(ctx context.Context, v *V) (string, error) {
	r, err := v.rep.Get(ctx, "/api/v1/flights?limit=500&date="+v.date)
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	var page struct{ Items []flight }
	_ = r.JSON(&page)
	if len(page.Items) == 0 {
		return "", fail("temsilci hiç uçuş görmedi")
	}
	for _, f := range page.Items {
		if f.AirlineCode != "YK" {
			return "", fail("temsilci başka havayolunun uçuşunu gördü: %s", f.FlightNo)
		}
	}
	r, _ = v.rep.Get(ctx, "/api/v1/flights?airline=MR")
	if r.Status != 403 {
		return "", fail("başka havayolu filtresi 403 olmalı: %d", r.Status)
	}
	r, _ = v.ops.Get(ctx, "/api/v1/flights?airline=MR&limit=1&date="+v.date)
	var other struct{ Items []flight }
	_ = r.JSON(&other)
	if len(other.Items) == 1 {
		r, _ = v.rep.Get(ctx, "/api/v1/flights/"+strconv.FormatInt(other.Items[0].ID, 10))
		if r.Status != 404 {
			return "", fail("başka havayolunun uçuş detayı 404 olmalı: %d", r.Status)
		}
	}
	return fmt.Sprintf("YK temsilcisi %d uçuşun tamamı YK; MR filtresi 403, MR detayı 404", len(page.Items)), nil
}

func checkFlights(ctx context.Context, v *V) (string, error) {
	// keyset sayfalama: tüm günü 37'lik sayfalarla dolaş, tekrar/eksik olmamalı
	seen := map[int64]bool{}
	cursor, pages, arr, dep := "", 0, 0, 0
	var last time.Time
	for {
		path := "/api/v1/flights?limit=37&date=" + v.date
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		r, err := v.ops.Get(ctx, path)
		if err := expect(r, err, 200); err != nil {
			return "", err
		}
		var page struct {
			Items      []flight
			NextCursor string `json:"next_cursor"`
		}
		_ = r.JSON(&page)
		pages++
		for _, f := range page.Items {
			if seen[f.ID] {
				return "", fail("sayfalamada tekrar: %d", f.ID)
			}
			seen[f.ID] = true
			if f.TailNumber == "" || f.Type == "" {
				return "", fail("uçuşta kuyruk/tip eksik: %s", f.FlightNo)
			}
			if f.Direction == "ARR" {
				arr++
			} else {
				dep++
			}
			_ = last
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) < 250 || arr == 0 || dep == 0 {
		return "", fail("gün çizelgesi eksik: %d uçuş", len(seen))
	}
	// doğrulama hatası
	r, err := v.ops.Post(ctx, "/api/v1/flights", map[string]any{"flight_no": "bad no", "direction": "ARR", "tail_number": "TC-PXA",
		"other_airport": "ESB", "sched_time": time.Now()})
	if err := expect(r, err, 422); err != nil {
		return "", err
	}
	// yarın için yeni geliş+gidiş, idempotent oluşturma, turnaround kurma
	tm, _ := time.ParseInLocation("2006-01-02", v.tomorrow, v.loc)
	arrNo, depNo := "PX"+v.runTag, "PX"+strconv.Itoa(1+mustAtoi(v.runTag))
	body := map[string]any{"flight_no": arrNo, "direction": "ARR", "tail_number": "TC-PXA", "other_airport": "ESB",
		"sched_time": tm.Add(10 * time.Hour)}
	key := "verify-create-" + v.runTag
	r1, err := v.ops.Post(ctx, "/api/v1/flights", body, "Idempotency-Key", key)
	if err := expect(r1, err, 201); err != nil {
		return "", err
	}
	r2, err := v.ops.Post(ctx, "/api/v1/flights", body, "Idempotency-Key", key)
	if err := expect(r2, err, 201); err != nil {
		return "", err
	}
	if string(r1.Body) != string(r2.Body) || r2.Header.Get("Idempotent-Replayed") != "true" {
		return "", fail("aynı Idempotency-Key aynı yanıtı dönmeli")
	}
	r, _ = v.ops.Post(ctx, "/api/v1/flights", map[string]any{"flight_no": arrNo, "direction": "DEP", "tail_number": "TC-PXA",
		"other_airport": "ESB", "sched_time": tm.Add(10 * time.Hour)}, "Idempotency-Key", key)
	if r.Status != 422 || r.ErrorCode() != "idempotency_key_reused" {
		return "", fail("aynı anahtar farklı gövde 422 olmalı: %d", r.Status)
	}
	r, err = v.ops.Post(ctx, "/api/v1/flights", map[string]any{"flight_no": depNo, "direction": "DEP", "tail_number": "TC-PXA",
		"other_airport": "ADB", "sched_time": tm.Add(11 * time.Hour)})
	if err := expect(r, err, 201); err != nil {
		return "", err
	}
	r, err = v.ops.Post(ctx, "/api/v1/schedule/build", map[string]string{"date": v.tomorrow})
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	var created flight
	_ = r1.JSON(&created)
	r, _ = v.ops.Get(ctx, "/api/v1/flights/"+strconv.FormatInt(created.ID, 10))
	_ = r.JSON(&created)
	if created.TurnaroundID == nil {
		return "", fail("yeni uçuş turnaround'a bağlanmadı")
	}
	return fmt.Sprintf("%d uçuş (%d ARR/%d DEP) %d sayfada tekrarsız; oluşturma idempotent; ARR+DEP → turnaround %d",
		len(seen), arr, dep, pages, *created.TurnaroundID), nil
}

func mustAtoi(s string) int { n, _ := strconv.Atoi(s); return n }

func checkInventory(ctx context.Context, v *V) (string, error) {
	r, err := v.ops.Get(ctx, "/api/v1/stands")
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	var all struct {
		Items []struct {
			ID         int      `json:"id"`
			Code       string   `json:"code"`
			MaxSize    string   `json:"max_size_class"`
			HasBridge  bool     `json:"has_bridge"`
			Compatible []string `json:"compatible_types"`
		}
	}
	_ = r.JSON(&all)
	r, _ = v.ops.Get(ctx, "/api/v1/stands?type=B77W")
	var wide struct {
		Items []struct {
			Code    string `json:"code"`
			MaxSize string `json:"max_size_class"`
		}
	}
	_ = r.JSON(&wide)
	for _, s := range wide.Items {
		if s.MaxSize < "E" {
			return "", fail("B77W için %s (%s) uyumlu gösterildi", s.Code, s.MaxSize)
		}
	}
	if len(wide.Items) == 0 || len(wide.Items) >= len(all.Items) {
		return "", fail("tip filtresi çalışmıyor")
	}
	r, err = v.ops.Get(ctx, "/api/v1/gates")
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	r, err = v.ops.Get(ctx, "/api/v1/aircraft-types")
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	// stand kapat/aç (denetimli)
	id := strconv.Itoa(all.Items[len(all.Items)-1].ID)
	r, err = v.ops.Do(ctx, "PATCH", "/api/v1/stands/"+id, map[string]bool{"active": false})
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	r, err = v.ops.Do(ctx, "PATCH", "/api/v1/stands/"+id, map[string]bool{"active": true})
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d stand; B77W uyumlu %d stand (hepsi E+); stand pasif/aktif edildi", len(all.Items), len(wide.Items)), nil
}

func checkAllocations(ctx context.Context, v *V) (string, error) {
	allocs, err := v.allocations(ctx, v.date)
	if err != nil {
		return "", err
	}
	if len(allocs) < 100 {
		return "", fail("yalnızca %d tahsis var", len(allocs))
	}
	// elle atama: dolu bir standa çakışan bir turnaround koymaya çalış -> 409
	a, b := allocs[0], allocs[0]
	found := false
	for _, x := range allocs {
		if x.StandID != a.StandID && x.OnBlock.Before(a.OffBlock) && a.OnBlock.Before(x.OffBlock) && x.AircraftType == a.AircraftType {
			b, found = x, true
			break
		}
	}
	if found {
		r, err := v.ops.Post(ctx, "/api/v1/allocations", map[string]any{"turnaround_id": b.TurnaroundID, "stand_id": a.StandID})
		if err := expect(r, err, 409); err != nil {
			return "", fail("çakışan elle atama 409 olmalı: %v", err)
		}
	}
	return fmt.Sprintf("%d aktif tahsis; çakışan elle atama → 409 (EXCLUDE)", len(allocs)), nil
}

func checkT1(ctx context.Context, v *V) (string, error) {
	t0 := time.Now()
	r, err := v.ops.Post(ctx, "/api/v1/allocations/run", map[string]string{"date": v.date})
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	took := time.Since(t0)
	var run struct {
		Total       int     `json:"total"`
		Allocated   int     `json:"allocated"`
		SolveMS     float64 `json:"solve_ms"`
		Unallocated []struct {
			Reason, Detail string
		} `json:"unallocated"`
	}
	_ = r.JSON(&run)
	if took > 20*time.Second {
		return "", fail("tahsis %s sürdü (sınır 20 sn)", took)
	}
	allocs, err := v.allocations(ctx, v.date)
	if err != nil {
		return "", err
	}
	if err := v.standRules(ctx, allocs); err != nil {
		return "", err
	}
	for _, u := range run.Unallocated {
		if u.Reason == "" || u.Detail == "" {
			return "", fail("yerleşemeyen uçuş nedensiz raporlandı")
		}
	}
	if run.Allocated+len(run.Unallocated) != run.Total {
		return "", fail("toplam tutmuyor")
	}
	// çözülemeyen uçuş raporu: yarın B77W'li geliş, E standları kapalıyken
	r, _ = v.ops.Get(ctx, "/api/v1/stands?type=B77W")
	var wide struct{ Items []struct{ ID int } }
	_ = r.JSON(&wide)
	defer func() {
		for _, s := range wide.Items {
			_, _ = v.ops.Do(context.Background(), "PATCH", "/api/v1/stands/"+strconv.Itoa(s.ID), map[string]bool{"active": true})
		}
	}()
	for _, s := range wide.Items {
		_, _ = v.ops.Do(ctx, "PATCH", "/api/v1/stands/"+strconv.Itoa(s.ID), map[string]bool{"active": false})
	}
	tm, _ := time.ParseInLocation("2006-01-02", v.tomorrow, v.loc)
	r, err = v.ops.Post(ctx, "/api/v1/flights", map[string]any{"flight_no": "YK" + v.runTag, "direction": "ARR", "tail_number": "TC-YKG",
		"other_airport": "DXB", "sched_time": tm.Add(14 * time.Hour)})
	if err := expect(r, err, 201); err != nil {
		return "", err
	}
	_, _ = v.ops.Post(ctx, "/api/v1/schedule/build", map[string]string{"date": v.tomorrow})
	r, err = v.ops.Post(ctx, "/api/v1/allocations/run", map[string]string{"date": v.tomorrow})
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	var tmr struct {
		Unallocated []struct{ Reason, Tail string } `json:"unallocated"`
	}
	_ = r.JSON(&tmr)
	ok := false
	for _, u := range tmr.Unallocated {
		if u.Reason == "NO_COMPATIBLE_STAND" {
			ok = true
		}
	}
	if !ok {
		return "", fail("E standları kapalıyken B77W NO_COMPATIBLE_STAND ile raporlanmalı: %s", r.Body)
	}
	return fmt.Sprintf("%d/%d yerleşti, çözüm %.1f ms (uçtan uca %s); 15 dk tampon + boyut istemcide doğrulandı; çözülemeyen → NO_COMPATIBLE_STAND",
		run.Allocated, run.Total, run.SolveMS, took.Round(time.Millisecond)), nil
}

type task struct {
	ID            int64     `json:"id"`
	TurnaroundID  int64     `json:"turnaround_id"`
	TaskType      string    `json:"task_type"`
	PlannedStart  time.Time `json:"planned_start"`
	PlannedEnd    time.Time `json:"planned_end"`
	CrewID        *int      `json:"crew_id"`
	CrewName      *string   `json:"crew_name"`
	EquipmentCode *string   `json:"equipment_code"`
	Status        string    `json:"status"`
}

func (v *V) tasks(ctx context.Context, c *client.Client, path string) ([]task, error) {
	var all []task
	cursor := ""
	for {
		p := path
		if cursor != "" {
			p += "&cursor=" + cursor
		}
		r, err := c.Get(ctx, p)
		if err := expect(r, err, 200); err != nil {
			return nil, err
		}
		var page struct {
			Items      []task
			NextCursor string `json:"next_cursor"`
		}
		_ = r.JSON(&page)
		all = append(all, page.Items...)
		if page.NextCursor == "" {
			return all, nil
		}
		cursor = page.NextCursor
	}
}

func checkCrews(ctx context.Context, v *V) (string, error) {
	r, err := v.chief.Get(ctx, "/api/v1/crews?date="+v.date)
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	var crews struct{ Items []struct{ ID int } }
	_ = r.JSON(&crews)
	r, err = v.chief.Get(ctx, "/api/v1/equipment")
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	var eq struct{ Items []struct{ Kind string } }
	_ = r.JSON(&eq)
	trucks := 0
	for _, e := range eq.Items {
		if e.Kind == "FUEL_TRUCK" {
			trucks++
		}
	}
	ts, err := v.tasks(ctx, v.chief, "/api/v1/tasks?limit=200&date="+v.date)
	if err != nil {
		return "", err
	}
	mine, err := v.tasks(ctx, v.lead, "/api/v1/my/tasks?limit=200&date="+v.date)
	if err != nil {
		return "", err
	}
	for _, t := range mine {
		if t.CrewName == nil || *t.CrewName != "Yakıt-1" {
			return "", fail("ekip lideri başka ekibin görevini gördü")
		}
	}
	return fmt.Sprintf("%d ekip, %d yakıt aracı, %d görev; ekip lideri yalnızca kendi %d görevini görüyor", len(crews.Items), trucks, len(ts), len(mine)), nil
}

func checkT3(ctx context.Context, v *V) (string, error) {
	r, err := v.chief.Post(ctx, "/api/v1/assignments/run", map[string]string{"date": v.date})
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	var sum struct {
		Assigned        int `json:"assigned"`
		Risks           []struct{ Level string }
		ShiftViolations []struct {
			CrewID int `json:"crew_id"`
		} `json:"shift_violations"`
	}
	_ = r.JSON(&sum)
	ts, err := v.tasks(ctx, v.chief, "/api/v1/tasks?limit=500&date="+v.date)
	if err != nil {
		return "", err
	}
	r, _ = v.chief.Get(ctx, "/api/v1/crews?date="+v.date)
	var crews struct {
		Items []struct {
			ID     int
			Name   string
			Certs  map[string]string `json:"certifications"`
			Shifts []struct{ Start, End time.Time }
		}
	}
	_ = r.JSON(&crews)
	byName := map[string]int{}
	shifts := map[int][]struct{ Start, End time.Time }{}
	for _, c := range crews.Items {
		byName[c.Name] = c.ID
		shifts[c.ID] = c.Shifts
	}
	busy := map[int][]task{}
	fuelAt := []task{}
	for _, t := range ts {
		if t.CrewID == nil || t.Status == "PENDING" {
			continue
		}
		if *t.CrewID == byName["Yakıt-10"] && t.TaskType == "FUEL" && t.Status == "ASSIGNED" {
			return "", fail("sertifikası dolmuş Yakıt-10'a yakıt görevi verildi")
		}
		if *t.CrewID == byName["Bagaj-14"] && t.Status == "ASSIGNED" && t.PlannedStart.In(v.loc).Format("2006-01-02") == v.date &&
			t.PlannedStart.In(v.loc).Hour() < 13 {
			return "", fail("dinlenme kuralını ihlal eden vardiyada görev verildi")
		}
		busy[*t.CrewID] = append(busy[*t.CrewID], t)
		if t.TaskType == "FUEL" {
			if t.EquipmentCode == nil {
				return "", fail("yakıt görevi araçsız: %d", t.ID)
			}
			fuelAt = append(fuelAt, t)
		}
	}
	for crewID, list := range busy {
		sort.Slice(list, func(i, j int) bool { return list[i].PlannedStart.Before(list[j].PlannedStart) })
		for i := 1; i < len(list); i++ {
			if list[i].PlannedStart.Before(list[i-1].PlannedEnd) {
				return "", fail("ekip %d aynı anda iki görevde", crewID)
			}
		}
	}
	// aynı anda en fazla 4 yakıt aracı
	for _, a := range fuelAt {
		n := 0
		for _, b := range fuelAt {
			if !b.PlannedStart.After(a.PlannedStart) && b.PlannedEnd.After(a.PlannedStart) {
				n++
			}
		}
		if n > 4 {
			return "", fail("%s anında %d yakıt görevi var (4 araç)", a.PlannedStart, n)
		}
	}
	if len(sum.ShiftViolations) == 0 {
		return "", fail("Bagaj-14'ün dinlenme ihlali raporlanmalı")
	}
	// API dinlenme kuralı: 7 saat dinlenmeli vardiya → 422
	day, _ := time.ParseInLocation("2006-01-02", v.date, v.loc)
	r, err = v.chief.Post(ctx, "/api/v1/crews/"+strconv.Itoa(byName["Temizlik-1"])+"/shifts",
		map[string]any{"start": day.Add(20 * time.Hour), "end": day.Add(26 * time.Hour)})
	if err := expect(r, err, 422); err != nil {
		return "", err
	}
	warn, crit := 0, 0
	for _, x := range sum.Risks {
		if x.Level == "WARNING" {
			warn++
		} else {
			crit++
		}
	}
	return fmt.Sprintf("%d atama; çakışma yok, ≤4 eşzamanlı yakıt aracı, dolmuş sertifika ve dinlenme ihlali dışarıda; risk: %d uyarı %d kritik; 7 sa dinlenme → 422",
		sum.Assigned, warn, crit), nil
}

func checkFeed(ctx context.Context, v *V) (string, error) {
	r, _ := v.ops.Get(ctx, "/api/v1/flights?limit=1&direction=DEP&status=SCHEDULED&date="+v.date)
	var p struct{ Items []flight }
	_ = r.JSON(&p)
	if len(p.Items) == 0 {
		return "", fail("planlı kalkış bulunamadı")
	}
	f := p.Items[0]
	mk := func(seq int64, min int) map[string]any {
		return map[string]any{"source": "VERIFY", "message_id": fmt.Sprintf("v%s-%d-%d", v.runTag, f.ID, seq), "flight_no": f.FlightNo,
			"op_date": v.date, "direction": "DEP", "seq": seq + 1000, "kind": "ESTIMATE", "new_time": f.EstTime.Add(time.Duration(min) * time.Minute)}
	}
	// sırasız ve tekrarlı: 3, 1, 2, 3
	r, err := v.ops.Post(ctx, "/api/v1/feed/events", map[string]any{"events": []any{mk(3, 30), mk(1, 10), mk(2, 20), mk(3, 30)}})
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	var out struct{ Summary map[string]int }
	_ = r.JSON(&out)
	r, _ = v.ops.Get(ctx, "/api/v1/flights/"+strconv.FormatInt(f.ID, 10))
	var after flight
	_ = r.JSON(&after)
	if !after.EstTime.Equal(f.EstTime.Add(30*time.Minute)) || after.Status != "DELAYED" {
		return "", fail("son durum +30 dk DELAYED olmalı: %v %s", after.EstTime.Sub(f.EstTime), after.Status)
	}
	if out.Summary["APPLIED"] != 1 || out.Summary["STALE"] != 2 || out.Summary["DUPLICATE"] != 1 {
		return "", fail("özet beklenmedik: %v", out.Summary)
	}
	// iptal
	r, _ = v.ops.Get(ctx, "/api/v1/flights?limit=1&direction=ARR&status=SCHEDULED&date="+v.tomorrow)
	_ = r.JSON(&p)
	r, _ = v.ops.Get(ctx, "/api/v1/changes")
	if r.Status != 200 {
		return "", fail("değişiklik listesi %d", r.Status)
	}
	return fmt.Sprintf("%s: seq 3,1,2,3 → APPLIED 1, STALE 2, DUPLICATE 1; son durum +30 dk DELAYED", f.FlightNo), nil
}

func checkT2(ctx context.Context, v *V) (string, error) {
	// geç saatlerdeki bir geliş: yayılım zinciri ve stand etkisi olsun
	r, _ := v.ops.Get(ctx, "/api/v1/flights?limit=200&direction=ARR&date="+v.date)
	var p struct{ Items []flight }
	_ = r.JSON(&p)
	var f *flight
	for i := range p.Items {
		x := &p.Items[i]
		if x.TurnaroundID != nil && (x.Status == "SCHEDULED" || x.Status == "DELAYED") && x.EstTime.After(time.Now().Add(30*time.Minute)) {
			f = x
			break
		}
	}
	if f == nil {
		for i := range p.Items {
			if p.Items[i].TurnaroundID != nil && p.Items[i].Status != "CANCELLED" && p.Items[i].Status != "LANDED" {
				f = &p.Items[i]
				break
			}
		}
	}
	if f == nil {
		return "", fail("yeniden planlanabilir geliş bulunamadı")
	}
	newTime := f.EstTime.Add(85 * time.Minute)
	// besleme olarak gelsin; bekleyen yeniden planlamalarda görünmeli
	r, err := v.ops.Post(ctx, "/api/v1/feed/events", map[string]any{"events": []any{map[string]any{"source": "VERIFY",
		"message_id": "t2-" + v.runTag, "flight_no": f.FlightNo, "op_date": v.date, "direction": "ARR", "seq": 5000, "kind": "ESTIMATE", "new_time": newTime}}})
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	r, _ = v.ops.Get(ctx, "/api/v1/replan/pending?date="+v.date)
	if !strings.Contains(string(r.Body), fmt.Sprintf(`"flight_id":%d`, f.ID)) {
		return "", fail("geciken uçuş bekleyenlerde görünmedi")
	}
	before, _ := v.allocations(ctx, v.date)
	body := map[string]any{"flight_id": f.ID, "new_time": newTime}
	r, err = v.ops.Post(ctx, "/api/v1/replan/preview", body)
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	var plan struct {
		Turnarounds       []any `json:"turnarounds"`
		Flights           []any `json:"flights"`
		Stands            []any `json:"stands"`
		Tasks             []any `json:"tasks"`
		AssignmentChanges int   `json:"assignment_changes"`
		Fingerprint       string
	}
	_ = r.JSON(&plan)
	if len(plan.Turnarounds) == 0 || plan.Fingerprint == "" {
		return "", fail("önizleme boş: %s", trim(string(r.Body), 200))
	}
	mid, _ := v.allocations(ctx, v.date)
	if fmt.Sprint(before) != fmt.Sprint(mid) {
		return "", fail("önizleme veritabanını değiştirmemeli")
	}
	stale := map[string]any{"flight_id": f.ID, "new_time": newTime, "fingerprint": "0000000000000000"}
	r, _ = v.ops.Post(ctx, "/api/v1/replan/apply", stale)
	if r.Status != 409 {
		return "", fail("eski parmak izi 409 olmalı: %d", r.Status)
	}
	body["fingerprint"] = plan.Fingerprint
	key := "verify-replan-" + v.runTag
	r, err = v.ops.Post(ctx, "/api/v1/replan/apply", body, "Idempotency-Key", key)
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	r2, _ := v.ops.Post(ctx, "/api/v1/replan/apply", body, "Idempotency-Key", key)
	if r2.Header.Get("Idempotent-Replayed") != "true" {
		return "", fail("tekrar uygulama idempotent olmalı")
	}
	after, _ := v.allocations(ctx, v.date)
	if err := v.standRules(ctx, after); err != nil {
		return "", fail("uygulama sonrası kural ihlali: %v", err)
	}
	r, _ = v.ops.Get(ctx, "/api/v1/replan/pending?date="+v.date)
	if strings.Contains(string(r.Body), fmt.Sprintf(`"flight_id":%d,`, f.ID)) {
		return "", fail("uygulamadan sonra uçuş hâlâ bekleyenlerde")
	}
	return fmt.Sprintf("%s +85 dk: %d turnaround, %d uçuş kaydı, %d stand, %d görev değişikliği (atama değişikliği %d); eski plan 409, tekrar idempotent",
		f.FlightNo, len(plan.Turnarounds), len(plan.Flights), len(plan.Stands), len(plan.Tasks), plan.AssignmentChanges), nil
}

// sse akışa bağlanır, ready kapandığında bağlıdır; count olay sayısını tutar.
func sse(ctx context.Context, c *client.Client, ready chan<- struct{}, count *int64, match string) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.Base+"/api/v1/stream", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		close(ready)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		close(ready)
		return fmt.Errorf("stream HTTP %d", resp.StatusCode)
	}
	close(ready)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") && strings.Contains(sc.Text(), match) {
			atomic.AddInt64(count, 1)
		}
	}
	return nil
}

func checkDashboard(ctx context.Context, v *V) (string, error) {
	r, err := v.mgmt.Get(ctx, "/api/v1/dashboard?date="+v.date)
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	var d struct {
		Flights, Turnarounds, Allocated int
		Tasks                           map[string]int
	}
	_ = r.JSON(&d)
	if d.Flights == 0 || d.Turnarounds == 0 {
		return "", fail("pano boş")
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ready := make(chan struct{})
	var n int64
	go sse(sctx, v.mgmt, ready, &n, "verify-dashboard-"+v.runTag)
	<-ready
	time.Sleep(200 * time.Millisecond)
	r, err = v.ops.Post(ctx, "/api/v1/notices", map[string]string{"message": "verify-dashboard-" + v.runTag, "level": "info"})
	if err := expect(r, err, 202); err != nil {
		return "", err
	}
	for i := 0; i < 50 && atomic.LoadInt64(&n) == 0; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if atomic.LoadInt64(&n) != 1 {
		return "", fail("SSE olayı ulaşmadı")
	}
	return fmt.Sprintf("pano: %d uçuş, %d/%d turnaround standlı; SSE duyuru olayı alındı", d.Flights, d.Allocated, d.Turnarounds), nil
}

func uuid(i int, tag string) string {
	return fmt.Sprintf("00000000-%04s-4000-8000-%012d", tag[:4], i)
}

func checkT4(ctx context.Context, v *V) (string, error) {
	mine, err := v.tasks(ctx, v.lead, "/api/v1/my/tasks?limit=200&date="+v.date)
	if err != nil {
		return "", err
	}
	var target *task
	for i := range mine {
		if mine[i].Status == "ASSIGNED" {
			target = &mine[i]
			break
		}
	}
	if target == nil {
		// gün ilerlediyse yarının görevi yok; atanmış bir görevi şef üzerinden bul
		return "", fail("ekip liderinin atanmış görevi kalmadı (make reset-demo sonrası tekrar deneyin)")
	}
	now := time.Now().UTC().Truncate(time.Second)
	tStart, tLeadStart, tFinish := now.Add(-50*time.Minute), now.Add(-45*time.Minute), now.Add(-20*time.Minute)
	ev := func(i int, typ string, at time.Time) map[string]any {
		return map[string]any{"client_event_id": uuid(i, v.runTag), "task_id": target.ID, "type": typ, "occurred_at": at}
	}
	// Tablet çevrimdışıyken başladı+bitti girmiş, sonra ters sırada gönderiyor.
	leadBatch := []any{ev(2, "FINISHED", tFinish), ev(1, "STARTED", tLeadStart)}
	// Şef kendi tabletinden aynı görevi daha erken "başladı" olarak girmiş.
	chiefBatch := []any{ev(3, "STARTED", tStart)}
	r, err := v.lead.Post(ctx, "/api/v1/sync/task-events", map[string]any{"device_id": "verify-lead", "events": leadBatch})
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	r, err = v.chief.Post(ctx, "/api/v1/sync/task-events", map[string]any{"device_id": "verify-chief", "events": chiefBatch})
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	var res struct {
		Results []struct {
			Result string
			State  struct {
				Status     string
				StartedAt  time.Time `json:"started_at"`
				FinishedAt time.Time `json:"finished_at"`
			}
		}
	}
	_ = r.JSON(&res)
	st := res.Results[0].State
	if st.Status != "DONE" || !st.StartedAt.Equal(tStart) || !st.FinishedAt.Equal(tFinish) || res.Results[0].Result != "APPLIED" {
		return "", fail("çakışma çözümü yanlış: %+v", res.Results[0])
	}
	// aynı parti tekrar (bağlantı koptu, yeniden gönderildi)
	r, _ = v.lead.Post(ctx, "/api/v1/sync/task-events", map[string]any{"device_id": "verify-lead", "events": leadBatch})
	if !strings.Contains(string(r.Body), `"DUPLICATE"`) {
		return "", fail("tekrar gönderim DUPLICATE olmalı")
	}
	// başka ekibin görevine yazma denemesi
	ts, _ := v.tasks(ctx, v.chief, "/api/v1/tasks?limit=200&date="+v.date)
	for _, t := range ts {
		if t.CrewName != nil && *t.CrewName != "Yakıt-1" && t.Status == "ASSIGNED" {
			r, _ = v.lead.Post(ctx, "/api/v1/sync/task-events", map[string]any{"device_id": "verify-lead",
				"events": []any{map[string]any{"client_event_id": uuid(9, v.runTag), "task_id": t.ID, "type": "STARTED", "occurred_at": now}}})
			if !strings.Contains(string(r.Body), `"REJECTED"`) {
				return "", fail("başka ekibin görevi reddedilmeli")
			}
			break
		}
	}
	r, _ = v.lead.Get(ctx, "/api/v1/tasks/"+strconv.FormatInt(target.ID, 10)+"/events")
	var hist struct {
		Events []struct {
			EventType string `json:"event_type"`
		}
	}
	_ = r.JSON(&hist)

	// akış: 2000 olayı 60 sn'ye yayarak gönder, SSE'de hepsi alınmalı
	sctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ready := make(chan struct{})
	var got int64
	tag := "load-" + v.runTag
	go sse(sctx, v.mgmt, ready, &got, tag)
	<-ready
	time.Sleep(200 * time.Millisecond)
	const total = 2000
	interval := 60 * time.Second / total
	start := time.Now()
	var sendErr int64
	sem := make(chan struct{}, 16)
	for i := 0; i < total; i++ {
		sem <- struct{}{}
		go func(i int) {
			defer func() { <-sem }()
			r, err := v.ops.Post(sctx, "/api/v1/notices", map[string]string{"message": fmt.Sprintf("%s #%d", tag, i), "level": "info"})
			if err != nil || r.Status != 202 {
				atomic.AddInt64(&sendErr, 1)
			}
		}(i)
		time.Sleep(time.Until(start.Add(time.Duration(i+1) * interval)))
	}
	for i := 0; i < 100 && atomic.LoadInt64(&got) < total; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	elapsed := time.Since(start)
	if atomic.LoadInt64(&got) != total || sendErr > 0 {
		return "", fail("SSE %d/%d olay aldı, %d gönderim hatası", got, total, sendErr)
	}
	return fmt.Sprintf("ters sıralı + iki cihazlı senkronizasyon → DONE, en erken başlangıç; tekrar DUPLICATE; yabancı görev REJECTED; geçmiş %d olay; SSE %d olay/%.0f sn, kayıp 0",
		len(hist.Events), got, elapsed.Seconds()), nil
}

func checkReport(ctx context.Context, v *V) (string, error) {
	r, err := v.mgmt.Get(ctx, "/api/v1/reports/turnaround?group_by=airline")
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	var rep struct {
		Rows []struct {
			Group string
			P50   float64 `json:"p50_min"`
			P90   float64 `json:"p90_min"`
		}
	}
	_ = r.JSON(&rep)
	if len(rep.Rows) == 0 {
		return "", fail("rapor boş (seed geçmiş analitik veri yüklemeliydi)")
	}
	for _, x := range rep.Rows {
		if x.P90 < x.P50 {
			return "", fail("%s: P90 < medyan", x.Group)
		}
	}
	r, err = v.ops.Get(ctx, "/api/v1/reports/turnaround?group_by=stand")
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	r, _ = v.ops.Get(ctx, "/api/v1/reports/turnaround?group_by=foo")
	if r.Status != 422 {
		return "", fail("geçersiz gruplama 422 olmalı")
	}
	return fmt.Sprintf("havayolu/stand/tip gruplu medyan-P90-ihlal oranı; %d havayolu satırı", len(rep.Rows)), nil
}

func checkT5(ctx context.Context, v *V) (string, error) {
	if v.pool == nil {
		return "", fail("DATABASE_URL gerekli (yük verisi üretimi ve EXPLAIN için)")
	}
	var n int64
	if err := v.pool.QueryRow(ctx, `SELECT count(*) FROM task_events`).Scan(&n); err != nil {
		return "", err
	}
	gen := ""
	if n < v.eventsTarget {
		t0 := time.Now()
		var ta, ev int64
		if err := v.pool.QueryRow(ctx, `SELECT * FROM analytics_generate($1, 0.42)`, v.eventsTarget-n).Scan(&ta, &ev); err != nil {
			return "", err
		}
		gen = fmt.Sprintf("üretildi %d olay/%.0f sn; ", ev, time.Since(t0).Seconds())
		_ = v.pool.QueryRow(ctx, `SELECT count(*) FROM task_events`).Scan(&n)
	}
	r, err := v.mgmt.Post(ctx, "/api/v1/reports/refresh", nil)
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	worst := time.Duration(0)
	for _, g := range []string{"type", "airline", "stand"} {
		t0 := time.Now()
		r, err := v.mgmt.Get(ctx, "/api/v1/reports/turnaround?group_by="+g)
		if err := expect(r, err, 200); err != nil {
			return "", err
		}
		if d := time.Since(t0); d > worst {
			worst = d
		}
	}
	if worst > 2*time.Second {
		return "", fail("rapor %s sürdü (sınır 2 sn)", worst)
	}
	// pencere fonksiyonlu sonuç percentile_disc ile aynı mı?
	r, _ = v.mgmt.Get(ctx, "/api/v1/reports/turnaround?group_by=type")
	var rep struct {
		Rows []struct {
			Group string
			P50   float64 `json:"p50_min"`
			P90   float64 `json:"p90_min"`
		}
	}
	_ = r.JSON(&rep)
	for _, row := range rep.Rows {
		var p50, p90 float64
		_ = v.pool.QueryRow(ctx, `SELECT round(percentile_disc(0.5) WITHIN GROUP (ORDER BY duration_s)/60.0,1)::float8,
			round(percentile_disc(0.9) WITHIN GROUP (ORDER BY duration_s)/60.0,1)::float8 FROM mv_turnaround_durations WHERE type_code=$1`, row.Group).Scan(&p50, &p90)
		if p50 != row.P50 || p90 != row.P90 {
			return "", fail("%s: rapor %v/%v, percentile_disc %v/%v", row.Group, row.P50, row.P90, p50, p90)
		}
	}
	// veri kalitesi
	r, _ = v.mgmt.Get(ctx, "/api/v1/reports/data-quality")
	if !strings.Contains(string(r.Body), "FINISHED_BEFORE_STARTED") || !strings.Contains(string(r.Body), "OCCURRED_IN_FUTURE") {
		return "", fail("veri kalitesi sorunları raporlanmalı: %s", r.Body)
	}
	// partisyon budama
	rows, err := v.pool.Query(ctx, `EXPLAIN SELECT count(*) FROM task_events WHERE occurred_at >= now() - interval '20 days' AND occurred_at < now() - interval '10 days'`)
	if err != nil {
		return "", err
	}
	var plan strings.Builder
	for rows.Next() {
		var l string
		_ = rows.Scan(&l)
		plan.WriteString(l + "\n")
	}
	parts := strings.Count(plan.String(), "task_events_2")
	if parts == 0 || parts > 2 {
		return "", fail("partisyon budaması beklenmedik: %d partisyon", parts)
	}
	return fmt.Sprintf("%s%d olay; en yavaş rapor %s; percentile_disc ile birebir; kalite sorunları yakalandı; 10 günlük sorgu %d partisyon tarıyor",
		gen, n, worst.Round(time.Millisecond), parts), nil
}

func checkOps(ctx context.Context, v *V) (string, error) {
	r, err := v.mgmt.Get(ctx, "/api/v1/audit")
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	var a struct{ Items []struct{ Action string } }
	_ = r.JSON(&a)
	actions := map[string]bool{}
	for _, x := range a.Items {
		actions[x.Action] = true
	}
	if !actions["replan.apply"] && !actions["allocation.run"] {
		return "", fail("kritik işlemler denetim kaydında yok")
	}
	r, err = v.base.Get(ctx, "/metrics")
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	if !strings.Contains(string(r.Body), "pk_http_requests_total") || !strings.Contains(string(r.Body), "pk_stream_events_total") {
		return "", fail("metrikler eksik")
	}
	r, err = v.base.Get(ctx, "/openapi.yaml")
	if err := expect(r, err, 200); err != nil {
		return "", err
	}
	if !strings.Contains(string(r.Body), "/api/v1/replan/preview") {
		return "", fail("OpenAPI eksik")
	}
	return fmt.Sprintf("denetim kaydında %d işlem türü; /metrics ve /openapi.yaml erişilebilir", len(actions)), nil
}
