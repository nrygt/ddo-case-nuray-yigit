package store_test

// Gerçek PostgreSQL üzerinde çalışan testler. TEST_DATABASE_URL yoksa atlanır.
// CI'da postgres:16 servis konteyneriyle çalışır.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pistkoordinat/internal/auth"
	"pistkoordinat/internal/db"
	"pistkoordinat/internal/domain/crew"
	"pistkoordinat/internal/domain/feed"
	"pistkoordinat/internal/domain/propagation"
	"pistkoordinat/internal/domain/tasksync"
	"pistkoordinat/internal/seed"
	"pistkoordinat/internal/store"
	"pistkoordinat/migrations"
)

var (
	pool   *pgxpool.Pool
	st     *store.Store
	ist, _ = time.LoadLocation("Europe/Istanbul")
	day    = time.Date(2026, 10, 20, 0, 0, 0, 0, ist)
	opDate = "2026-10-20"
	ctx    = context.Background()
)

func TestMain(m *testing.M) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		fmt.Println("TEST_DATABASE_URL yok, entegrasyon testleri atlanıyor")
		os.Exit(0)
	}
	var err error
	pool, err = db.Connect(ctx, url)
	if err != nil {
		panic(err)
	}
	if _, err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		panic(err)
	}
	if err := seed.Reset(ctx, pool, 42, day); err != nil {
		panic(err)
	}
	st = store.New(pool)
	code := m.Run()
	pool.Close()
	os.Exit(code)
}

// iki turnaround ve bir stand seç: aynı standa çakışan iki atama.
func freshTurnarounds(t *testing.T, n int) []int64 {
	t.Helper()
	var ids []int64
	for i := 0; i < n; i++ {
		var id int64
		err := pool.QueryRow(ctx, `INSERT INTO turnarounds(tail_number, op_date, on_block, off_block, synthetic)
			VALUES ('TC-KDC', '2026-10-21', '2026-10-21T10:00:00Z', '2026-10-21T11:00:00Z', true) RETURNING id`).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func standID(t *testing.T, code string) int {
	var id int
	if err := pool.QueryRow(ctx, `SELECT id FROM stands WHERE code=$1`, code).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertAlloc(taID int64, stand int, on, off string) error {
	_, err := pool.Exec(ctx, `INSERT INTO stand_allocations(turnaround_id, stand_id, on_block, off_block) VALUES ($1,$2,$3,$4)`,
		taID, stand, on, off)
	return err
}

// Yarış durumu: 20 goroutine aynı stand ve aynı zaman için atama yapar; yalnızca biri kazanmalı.
func TestStandExcludeRace(t *testing.T) {
	ids := freshTurnarounds(t, 20)
	stand := standID(t, "R16")
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, conflicts := 0, 0
	start := make(chan struct{})
	for _, id := range ids {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			<-start
			err := insertAlloc(id, stand, "2026-10-21T10:00:00Z", "2026-10-21T11:00:00Z")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if db.ErrCode(err) == db.CodeExclusion {
				conflicts++
			} else {
				t.Errorf("beklenmeyen hata: %v", err)
			}
		}(id)
	}
	close(start)
	wg.Wait()
	if ok != 1 || conflicts != 19 {
		t.Fatalf("1 başarı 19 çakışma bekleniyordu: ok=%d conflict=%d", ok, conflicts)
	}
}

func TestStandBufferEnforcedByDatabase(t *testing.T) {
	ids := freshTurnarounds(t, 3)
	stand := standID(t, "R15")
	if err := insertAlloc(ids[0], stand, "2026-10-21T12:00:00Z", "2026-10-21T13:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := insertAlloc(ids[1], stand, "2026-10-21T13:14:00Z", "2026-10-21T14:00:00Z"); db.ErrCode(err) != db.CodeExclusion {
		t.Fatalf("14 dk boşluk EXCLUDE ile reddedilmeli: %v", err)
	}
	if err := insertAlloc(ids[2], stand, "2026-10-21T13:15:00Z", "2026-10-21T14:00:00Z"); err != nil {
		t.Fatalf("15 dk boşluk kabul edilmeli: %v", err)
	}
}

func TestSizeAndBridgeGuardTrigger(t *testing.T) {
	var id int64
	// B77W (E) küçük standa
	if err := pool.QueryRow(ctx, `INSERT INTO turnarounds(tail_number, op_date, on_block, off_block, synthetic)
		VALUES ('TC-YKG', '2026-10-22', '2026-10-22T10:00:00Z', '2026-10-22T12:00:00Z', true) RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	err := insertAlloc(id, standID(t, "R1"), "2026-10-22T10:00:00Z", "2026-10-22T12:00:00Z")
	if db.ErrCode(err) != db.CodeCheck || !strings.Contains(err.Error(), "boyut") {
		t.Fatalf("boyut ihlali yakalanmalı: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE turnarounds SET needs_bridge=true WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	err = insertAlloc(id, standID(t, "R9"), "2026-10-22T10:00:00Z", "2026-10-22T12:00:00Z")
	if db.ErrCode(err) != db.CodeCheck || !strings.Contains(err.Error(), "köprü") {
		t.Fatalf("köprü ihlali yakalanmalı: %v", err)
	}
}

// Aynı yakıt aracı aynı anda iki göreve verilemez (eşzamanlı iki transaction).
func TestEquipmentExcludeRace(t *testing.T) {
	ids := freshTurnarounds(t, 2)
	var truck int
	_ = pool.QueryRow(ctx, `SELECT id FROM equipment WHERE code='YT-04'`).Scan(&truck)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id int64) {
			defer wg.Done()
			_, errs[i] = pool.Exec(ctx, `INSERT INTO tasks(turnaround_id, task_type, planned_start, planned_end, equipment_id, status)
				VALUES ($1,'FUEL','2026-10-21T10:10:00Z','2026-10-21T10:40:00Z',$2,'ASSIGNED')`, id, truck)
		}(i, id)
	}
	wg.Wait()
	failed := 0
	for _, e := range errs {
		if db.ErrCode(e) == db.CodeExclusion {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("tam olarak biri reddedilmeli: %v", errs)
	}
}

func TestIdempotencyClaimIsAtomic(t *testing.T) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prev, err := st.ClaimIdempotency(ctx, 1, "race-key-0001", "POST", "/x", "h1")
			if err != nil {
				t.Error(err)
				return
			}
			if prev == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("anahtarı tek bir istek almalı, %d", winners)
	}
	if err := st.CompleteIdempotency(ctx, 1, "race-key-0001", 201, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	prev, _ := st.ClaimIdempotency(ctx, 1, "race-key-0001", "POST", "/x", "h1")
	if prev == nil || *prev.Status != 201 || string(prev.Body) != `{"ok":true}` {
		t.Fatalf("kayıtlı yanıt dönmeli: %+v", prev)
	}
}

// Besleme olayları rastgele sırada, tekrarlı ve eşzamanlı uygulansa da son durum aynı olmalı.
func TestFeedOutOfOrderConcurrentConverges(t *testing.T) {
	var fno string
	var sched time.Time
	if err := pool.QueryRow(ctx, `SELECT flight_no, sched_time FROM flights WHERE op_date=$1 AND direction='ARR' ORDER BY id OFFSET 7 LIMIT 1`, opDate).Scan(&fno, &sched); err != nil {
		t.Fatal(err)
	}
	var evs []feed.Event
	for seq := int64(1); seq <= 8; seq++ {
		evs = append(evs, feed.Event{Source: "TEST", MessageID: fmt.Sprintf("conv-%s-%d", fno, seq), FlightNo: fno, OpDate: opDate,
			Direction: "ARR", Seq: seq, Kind: feed.Estimate, NewTime: sched.Add(time.Duration(seq*7) * time.Minute)})
	}
	stream := append(append([]feed.Event{}, evs...), evs...) // her olay iki kez
	rand.New(rand.NewPCG(3, 3)).Shuffle(len(stream), func(i, j int) { stream[i], stream[j] = stream[j], stream[i] })

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < len(stream); i += 4 {
				if _, err := st.ApplyFeedEvent(ctx, stream[i]); err != nil {
					t.Error(err)
				}
			}
		}(w)
	}
	wg.Wait()
	var est time.Time
	var seq int64
	_ = pool.QueryRow(ctx, `SELECT est_time, feed_seq FROM flights WHERE flight_no=$1 AND op_date=$2 AND direction='ARR'`, fno, opDate).Scan(&est, &seq)
	if seq != 8 || !est.Equal(sched.Add(56*time.Minute)) {
		t.Fatalf("son durum seq=8 ve +56 dk olmalı: seq=%d est=%v", seq, est.Sub(sched))
	}
	var applied, stale int
	_ = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE outcome='APPLIED'), count(*) FILTER (WHERE outcome='STALE')
		FROM feed_events WHERE message_id LIKE $1`, "conv-"+fno+"-%").Scan(&applied, &stale)
	if applied+stale != 8 {
		t.Fatalf("8 benzersiz mesaj kaydedilmeli: applied=%d stale=%d", applied, stale)
	}
}

func assignedTask(t *testing.T) (int64, int) {
	var id int64
	var crewID int
	if err := pool.QueryRow(ctx, `SELECT k.id, k.crew_id FROM tasks k JOIN turnarounds t ON t.id=k.turnaround_id
		WHERE t.op_date=$1 AND k.status='ASSIGNED' ORDER BY k.id DESC LIMIT 1`, opDate).Scan(&id, &crewID); err != nil {
		t.Fatal(err)
	}
	return id, crewID
}

func uuid(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

// İki cihaz aynı görevi çevrimdışıyken güncelledi; eşzamanlı senkronizasyon tutarlı sonuç vermeli.
func TestTwoDevicesSyncSameTask(t *testing.T) {
	taskID, crewID := assignedTask(t)
	now := time.Now().UTC().Truncate(time.Second)
	devA := []tasksync.Event{
		{ClientEventID: uuid(9001), TaskID: taskID, Type: tasksync.Started, OccurredAt: now.Add(-40 * time.Minute)},
		{ClientEventID: uuid(9002), TaskID: taskID, Type: tasksync.Finished, OccurredAt: now.Add(-10 * time.Minute)},
	}
	devB := []tasksync.Event{
		{ClientEventID: uuid(9003), TaskID: taskID, Type: tasksync.Started, OccurredAt: now.Add(-45 * time.Minute)},
	}
	var wg sync.WaitGroup
	for _, batch := range [][]tasksync.Event{devA, devB, devA} { // devA iki kez (yeniden deneme)
		wg.Add(1)
		go func(b []tasksync.Event) {
			defer wg.Done()
			if _, err := st.SyncTaskEvents(ctx, crewID, 0, "dev", b); err != nil {
				t.Error(err)
			}
		}(batch)
	}
	wg.Wait()
	var status string
	var started, finished time.Time
	_ = pool.QueryRow(ctx, `SELECT status, started_at, finished_at FROM tasks WHERE id=$1`, taskID).Scan(&status, &started, &finished)
	if status != "DONE" || !started.Equal(now.Add(-45*time.Minute)) || !finished.Equal(now.Add(-10*time.Minute)) {
		t.Fatalf("DONE, en erken başlangıç ve bitiş bekleniyordu: %s %v %v", status, started, finished)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type IN ('STARTED','FINISHED')`, taskID).Scan(&n)
	if n != 3 {
		t.Fatalf("tekrar eden olay iki kez yazılmamalı: %d", n)
	}
	// başka ekibin lideri bu göreve yazamamalı
	res, _ := st.SyncTaskEvents(ctx, crewID+1, 0, "x", []tasksync.Event{{ClientEventID: uuid(9010), TaskID: taskID, Type: tasksync.Started, OccurredAt: now}})
	if res[0].Result != tasksync.Rejected {
		t.Fatalf("yabancı ekip reddedilmeli: %+v", res[0])
	}
}

func TestAuditLogIsAppendOnly(t *testing.T) {
	if err := store.Audit(ctx, pool, auth.Claims{Sub: 1, Role: auth.OpsController}, "test", "x", "1", nil, map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_log SET action='hack'`); err == nil {
		t.Fatal("güncelleme engellenmeli")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM audit_log`); err == nil {
		t.Fatal("silme engellenmeli")
	}
}

func TestRunAllocationValidAndStable(t *testing.T) {
	first, err := st.RunAllocation(ctx, opDate, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Changed != 0 {
		t.Fatalf("seed sonrası tekrar çalıştırma hiçbir şeyi taşımamalı: %d", first.Changed)
	}
	var overlaps int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM stand_allocations a JOIN stand_allocations b
		ON a.stand_id=b.stand_id AND a.id<b.id AND a.active AND b.active AND a.blocked && b.blocked`).Scan(&overlaps)
	if overlaps != 0 {
		t.Fatalf("çakışan aktif atama var: %d", overlaps)
	}
	var unexplained int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM turnarounds t WHERE t.op_date=$1 AND NOT t.synthetic
		AND NOT EXISTS (SELECT 1 FROM stand_allocations sa WHERE sa.turnaround_id=t.id AND sa.active)`, opDate).Scan(&unexplained)
	if unexplained != len(first.Unallocated) {
		t.Fatalf("yerleşemeyen her turnaround raporda olmalı: db=%d rapor=%d", unexplained, len(first.Unallocated))
	}
}

func TestReplanApplyEndToEnd(t *testing.T) {
	var flightID int64
	var est time.Time
	if err := pool.QueryRow(ctx, `SELECT f.id, f.est_time FROM flights f JOIN turnarounds t ON t.arr_flight_id=f.id
		WHERE f.op_date=$1 AND f.status='SCHEDULED' ORDER BY f.sched_time OFFSET 30 LIMIT 1`, opDate).Scan(&flightID, &est); err != nil {
		t.Fatal(err)
	}
	trig := propagation.Trigger{FlightID: flightID, NewTime: est.Add(95 * time.Minute)}
	err := db.RetrySerializable(ctx, pool, 3, func(tx pgx.Tx) error {
		tl, _, err := store.LoadTimeline(ctx, tx, opDate)
		if err != nil {
			return err
		}
		plan, err := propagation.Replan(tl, trig)
		if err != nil {
			return err
		}
		if len(plan.Turnarounds) == 0 {
			t.Fatal("gecikme en az bir turnaround'u kaydırmalı")
		}
		return store.ApplyPlan(ctx, tx, plan, tl.Stands, 0)
	})
	if err != nil {
		t.Fatalf("plan uygulanamadı: %v", err)
	}
	var on time.Time
	_ = pool.QueryRow(ctx, `SELECT t.on_block FROM turnarounds t WHERE t.arr_flight_id=$1`, flightID).Scan(&on)
	if !on.Equal(trig.NewTime) {
		t.Fatalf("on-block yeni saate kaymalı: %v", on)
	}
	// uygulanan plandan sonra aynı tetikleyici yeni değişiklik üretmemeli (idempotent)
	tl, _, _ := store.LoadTimeline(ctx, pool, opDate)
	again, err := propagation.Replan(tl, trig)
	if err != nil || len(again.Turnarounds) != 0 || len(again.Stands) != 0 {
		t.Fatalf("ikinci plan boş olmalı: %+v %v", again, err)
	}
}

func TestShiftRestRuleOnInsert(t *testing.T) {
	var crewID int
	_ = pool.QueryRow(ctx, `SELECT id FROM crews WHERE name='Temizlik-1'`).Scan(&crewID)
	// erken vardiya bugün 05-13 yerel; aynı gün 20:00'de başlayan vardiya 7 saat dinlenme bırakır
	start := time.Date(2026, 10, 20, 20, 0, 0, 0, ist)
	_, err := st.AddShift(ctx, crewID, crew.Window{Start: start, End: start.Add(6 * time.Hour)})
	if err == nil || !strings.Contains(err.Error(), "dinlenme") {
		t.Fatalf("dinlenme kuralı ihlali reddedilmeli: %v", err)
	}
}

func TestRefreshTokenRotationAndReuse(t *testing.T) {
	tok1, h1 := auth.NewRefreshToken()
	_ = tok1
	if err := st.SaveRefresh(ctx, 1, h1, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, h2 := auth.NewRefreshToken()
	if _, err := st.RotateRefresh(ctx, h1, h2, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, h3 := auth.NewRefreshToken()
	if _, err := st.RotateRefresh(ctx, h1, h3, time.Now().Add(time.Hour)); err != store.ErrTokenReused {
		t.Fatalf("eski token tekrar kullanılamamalı: %v", err)
	}
	var active int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM refresh_tokens WHERE user_id=1 AND revoked_at IS NULL`).Scan(&active)
	if active != 0 {
		t.Fatalf("tekrar kullanımda kullanıcının tüm tokenları iptal olmalı: %d aktif", active)
	}
}

// T-5: pencere-fonksiyonlu rapor, PostgreSQL'in percentile_disc sonucuyla birebir aynı olmalı;
// veri kalitesi görünümü bilerek bozulmuş satırları yakalamalı ve MV onları dışlamalı.
func TestAnalyticsReportAndDataQuality(t *testing.T) {
	if _, err := st.GenerateAnalytics(ctx, 120_000, 0.11); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RefreshAnalytics(ctx); err != nil {
		t.Fatal(err)
	}
	rep, err := st.TurnaroundReport(ctx, "2000-01-01", "2100-01-01", "type")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rows) == 0 {
		t.Fatal("rapor boş")
	}
	rows, _ := pool.Query(ctx, `SELECT type_code,
		round(percentile_disc(0.5) WITHIN GROUP (ORDER BY duration_s) / 60.0, 1)::float8,
		round(percentile_disc(0.9) WITHIN GROUP (ORDER BY duration_s) / 60.0, 1)::float8, count(*)
		FROM mv_turnaround_durations GROUP BY 1`)
	want := map[string][3]float64{}
	for rows.Next() {
		var g string
		var p50, p90 float64
		var n int64
		_ = rows.Scan(&g, &p50, &p90, &n)
		want[g] = [3]float64{p50, p90, float64(n)}
	}
	for _, r := range rep.Rows {
		w := want[r.Group]
		if r.P50 != w[0] || r.P90 != w[1] || float64(r.Count) != w[2] {
			t.Fatalf("%s: rapor %v/%v/%d, percentile_disc %v", r.Group, r.P50, r.P90, r.Count, w)
		}
	}
	q, err := st.DataQuality(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int64{}
	for _, r := range q {
		found[r.Issue] = r.Tasks
	}
	if found["FINISHED_BEFORE_STARTED"] == 0 || found["OCCURRED_IN_FUTURE"] == 0 {
		t.Fatalf("bozuk zaman damgaları yakalanmalı: %+v", found)
	}
	var leaked int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM mv_turnaround_durations m
		WHERE EXISTS (SELECT 1 FROM task_event_quality_issues q WHERE q.turnaround_id = m.turnaround_id)`).Scan(&leaked)
	if leaked != 0 {
		t.Fatalf("kalite sorunlu turnaround MV'ye girmemeli: %d", leaked)
	}
}

// Zaman aralığı filtresi yalnızca ilgili ay partisyonunu taramalı.
func TestPartitionPruning(t *testing.T) {
	from := time.Now().AddDate(0, -2, 0)
	from = time.Date(from.Year(), from.Month(), 3, 0, 0, 0, 0, time.UTC)
	rows, err := pool.Query(ctx, `EXPLAIN SELECT count(*) FROM task_events WHERE occurred_at >= $1 AND occurred_at < $2`, from, from.AddDate(0, 0, 7))
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var l string
		_ = rows.Scan(&l)
		plan = append(plan, l)
	}
	joined := strings.Join(plan, "\n")
	if strings.Count(joined, "task_events_2") != 1 {
		t.Fatalf("tek partisyon taranmalı:\n%s", joined)
	}
}
