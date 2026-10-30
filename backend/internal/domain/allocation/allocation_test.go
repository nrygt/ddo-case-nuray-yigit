package allocation

import (
	"math/rand/v2"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC)

func at(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

func ta(id int64, size byte, on, off int) Turnaround {
	return Turnaround{ID: id, Tail: "TC-" + string(rune('A'+id%26)), Size: size, On: at(on), Off: at(off)}
}

func stand(id int, size byte, bridge bool) Stand {
	return Stand{ID: id, Code: "S" + string(rune('0'+id)), MaxSize: size, HasBridge: bridge, Active: true}
}

func TestOverlappingGoToDifferentStands(t *testing.T) {
	p := Problem{
		Stands:      []Stand{stand(1, 'C', false), stand(2, 'C', false)},
		Turnarounds: []Turnaround{ta(1, 'C', 0, 60), ta(2, 'C', 30, 90)},
	}
	r := Solve(p)
	if len(r.Unallocated) != 0 || r.Assign[1] == r.Assign[2] {
		t.Fatalf("çakışanlar ayrı standa gitmeli: %+v", r)
	}
}

func TestBufferExactly15MinutesIsAllowed(t *testing.T) {
	p := Problem{
		Stands:      []Stand{stand(1, 'C', false)},
		Turnarounds: []Turnaround{ta(1, 'C', 0, 60), ta(2, 'C', 75, 120)},
	}
	r := Solve(p)
	if len(r.Unallocated) != 0 {
		t.Fatalf("15 dk sonra başlayan aynı standa sığmalı: %+v", r.Unallocated)
	}
}

func TestBuffer14MinutesIsRejected(t *testing.T) {
	p := Problem{
		Stands:      []Stand{stand(1, 'C', false)},
		Turnarounds: []Turnaround{ta(1, 'C', 0, 60), ta(2, 'C', 74, 120)},
	}
	r := Solve(p)
	if len(r.Unallocated) != 1 || r.Unallocated[0].TurnaroundID != 2 {
		t.Fatalf("14 dk boşluk reddedilmeliydi: %+v", r)
	}
	if r.Unallocated[0].Reason != ReasonAllOccupied || len(r.Unallocated[0].BlockedBy) != 1 {
		t.Fatalf("neden ve engelleyen yanlış: %+v", r.Unallocated[0])
	}
}

func TestSizeCompatibility(t *testing.T) {
	p := Problem{
		Stands:      []Stand{stand(1, 'C', false), stand(2, 'E', false)},
		Turnarounds: []Turnaround{ta(1, 'E', 0, 90)},
	}
	if r := Solve(p); r.Assign[1] != 2 {
		t.Fatalf("E uçak E standa gitmeli, gitti: %d", r.Assign[1])
	}
}

func TestSmallAircraftPrefersSmallStand(t *testing.T) {
	p := Problem{
		Stands:      []Stand{stand(1, 'E', false), stand(2, 'C', false)},
		Turnarounds: []Turnaround{ta(1, 'C', 0, 60)},
	}
	if r := Solve(p); r.Assign[1] != 2 {
		t.Fatalf("C uçak büyük standı işgal etmemeli")
	}
}

func TestBridgeRequirement(t *testing.T) {
	p := Problem{
		Stands: []Stand{stand(1, 'C', false), stand(2, 'C', true)},
		Turnarounds: []Turnaround{
			{ID: 1, Size: 'C', NeedsBridge: true, On: at(0), Off: at(60)},
			{ID: 2, Size: 'C', On: at(0), Off: at(60)},
		},
	}
	r := Solve(p)
	if r.Assign[1] != 2 || r.Assign[2] != 1 {
		t.Fatalf("köprü isteyen köprülüye, istemeyen köprüsüze: %+v", r.Assign)
	}
}

func TestNoCompatibleStandReason(t *testing.T) {
	p := Problem{
		Stands:      []Stand{stand(1, 'C', false)},
		Turnarounds: []Turnaround{ta(1, 'F', 0, 60)},
	}
	r := Solve(p)
	if len(r.Unallocated) != 1 || r.Unallocated[0].Reason != ReasonNoCompatible {
		t.Fatalf("uyumlu stand yok nedeni bekleniyordu: %+v", r)
	}
}

func TestInvalidTimes(t *testing.T) {
	p := Problem{Stands: []Stand{stand(1, 'C', false)}, Turnarounds: []Turnaround{ta(1, 'C', 60, 60)}}
	r := Solve(p)
	if len(r.Unallocated) != 1 || r.Unallocated[0].Reason != ReasonBadTimes {
		t.Fatalf("geçersiz zaman yakalanmalı: %+v", r)
	}
}

func TestSameAircraftKeepsPreviousStand(t *testing.T) {
	p := Problem{
		Stands: []Stand{stand(1, 'C', false), stand(2, 'C', false), stand(3, 'C', false)},
		Turnarounds: []Turnaround{
			{ID: 1, Tail: "TC-JAA", Size: 'C', On: at(0), Off: at(50)},
			{ID: 2, Tail: "TC-JAA", Size: 'C', On: at(300), Off: at(350), PreferStand: 3},
		},
	}
	if r := Solve(p); r.Assign[2] != 3 {
		t.Fatalf("aynı uçağın dönüşü tercih edilen standda kalmalı, gitti: %d", r.Assign[2])
	}
}

func TestInactiveStandIgnored(t *testing.T) {
	s := stand(1, 'C', false)
	s.Active = false
	p := Problem{Stands: []Stand{s, stand(2, 'C', false)}, Turnarounds: []Turnaround{ta(1, 'C', 0, 30)}}
	if r := Solve(p); r.Assign[1] != 2 {
		t.Fatalf("pasif stand kullanılmamalı")
	}
}

func TestPinnedAllocationIsKept(t *testing.T) {
	p := Problem{
		Stands:      []Stand{stand(1, 'C', false), stand(2, 'C', false)},
		Turnarounds: []Turnaround{ta(1, 'C', 0, 60), ta(2, 'C', 0, 60)},
		Pinned:      map[int64]int{2: 1},
	}
	r := Solve(p)
	if r.Assign[2] != 1 || r.Assign[1] != 2 {
		t.Fatalf("sabitlenen atama korunmalı: %+v", r.Assign)
	}
}

// Açgözlü adım köprüsüz uçağı (A) köprülü C standına koyar (büyük standı
// israf etmemek için). Sonra köprü isteyen B gelir: tek köprülü stand A'da.
// Onarım A'yı D standına kaydırıp B'ye yer açmalı.
func TestRepairSwapsBlockingTurnaround(t *testing.T) {
	p := Problem{
		Stands: []Stand{stand(1, 'D', false), stand(2, 'C', true)},
		Turnarounds: []Turnaround{
			ta(1, 'C', 0, 60),
			{ID: 2, Size: 'C', NeedsBridge: true, On: at(10), Off: at(70)},
		},
	}
	r := Solve(p)
	if len(r.Unallocated) != 0 {
		t.Fatalf("onarım her ikisini yerleştirmeliydi: %+v", r.Unallocated)
	}
	if r.Assign[2] != 2 || r.Assign[1] != 1 {
		t.Fatalf("beklenen takas olmadı: %+v", r.Assign)
	}
	if v := Validate(p, r.Assign); len(v) > 0 {
		t.Fatal(v)
	}
}

func TestRepairModeMinimalChanges(t *testing.T) {
	stands := []Stand{stand(1, 'C', false), stand(2, 'C', false), stand(3, 'C', false)}
	tas := []Turnaround{ta(1, 'C', 0, 60), ta(2, 'C', 80, 140), ta(3, 'C', 0, 60), ta(4, 'C', 200, 260)}
	base := Solve(Problem{Stands: stands, Turnarounds: tas})
	// 1 numara 40 dk gecikti: 2 numarayla aynı standdaysa çakışır
	tas[0].On, tas[0].Off = at(40), at(100)
	r := Solve(Problem{Stands: stands, Turnarounds: tas, Current: base.Assign, Changed: map[int64]bool{1: true}})
	if len(r.Unallocated) != 0 {
		t.Fatalf("hepsi yerleşmeli: %+v", r.Unallocated)
	}
	if r.Changes > 1 {
		t.Fatalf("en fazla bir değişiklik bekleniyordu, %d", r.Changes)
	}
	for _, id := range []int64{2, 3, 4} {
		if r.Assign[id] != base.Assign[id] {
			t.Fatalf("değişmeyen turnaround %d yer değiştirmemeli", id)
		}
	}
}

// Rastgele 300 uçuşluk günlerde: çıkan plan kuralları ihlal etmemeli ve
// yerleşemeyen her turnaround için gerçekten boş uyumlu stand olmamalı.
func TestRandomDaysAreValid(t *testing.T) {
	for seed := uint64(1); seed <= 20; seed++ {
		p := randomDay(seed, 300)
		r := Solve(p)
		if v := Validate(p, r.Assign); len(v) > 0 {
			t.Fatalf("seed %d: %v", seed, v[0])
		}
		if len(r.Assign)+len(r.Unallocated) != len(p.Turnarounds) {
			t.Fatalf("seed %d: kayıp turnaround", seed)
		}
		b := newBoard(DefaultBuffer)
		byID := map[int64]Turnaround{}
		for _, x := range p.Turnarounds {
			byID[x.ID] = x
		}
		for id, sid := range r.Assign {
			b.put(sid, byID[id])
		}
		for _, u := range r.Unallocated {
			for _, s := range p.Stands {
				if Compatible(s, byID[u.TurnaroundID]) && b.free(s.ID, byID[u.TurnaroundID]) {
					t.Fatalf("seed %d: %d yerleşmedi ama %s boş", seed, u.TurnaroundID, s.Code)
				}
			}
		}
	}
}

func TestPerformance300FlightsUnder20s(t *testing.T) {
	p := randomDay(7, 300)
	start := time.Now()
	Solve(p)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("300 turnaround %s sürdü (sınır 20 sn, hedef < 2 sn)", d)
	}
}

func BenchmarkSolve300(b *testing.B) {
	p := randomDay(7, 300)
	for i := 0; i < b.N; i++ {
		Solve(p)
	}
}

func BenchmarkSolve3000(b *testing.B) {
	p := randomDay(7, 3000)
	for i := 0; i < b.N; i++ {
		Solve(p)
	}
}

func randomDay(seed uint64, n int) Problem {
	rng := rand.New(rand.NewPCG(seed, seed*31))
	var stands []Stand
	sizes := []byte{'C', 'C', 'C', 'C', 'D', 'E'}
	for i := 1; i <= 24; i++ {
		stands = append(stands, Stand{ID: i, Code: "S", MaxSize: sizes[i%len(sizes)], HasBridge: i <= 8, Active: true})
	}
	var tas []Turnaround
	acSizes := []byte{'C', 'C', 'C', 'C', 'C', 'D', 'E'}
	for i := 0; i < n; i++ {
		on := rng.IntN(18 * 60)
		dur := 35 + rng.IntN(120)
		tas = append(tas, Turnaround{
			ID: int64(i + 1), Tail: "T", Size: acSizes[rng.IntN(len(acSizes))],
			NeedsBridge: rng.IntN(4) == 0, On: at(on), Off: at(on + dur),
		})
	}
	return Problem{Stands: stands, Turnarounds: tas}
}
