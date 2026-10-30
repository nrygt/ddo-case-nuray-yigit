// Package allocation stand tahsis çizelgelemesini (T-1) yapar.
//
// Problem: her turnaround'a [OnBlock, OffBlock) aralığında bir stand ver;
// aynı standda iki turnaround arasında en az Buffer kadar boşluk kalsın,
// uçak boyutu stand kapasitesini aşmasın, köprü isteyen köprülü standa gitsin.
//
// Yaklaşım: başlangıç zamanına göre sıralı açgözlü atama + puanlama,
// ardından yerleşemeyenler için tek adımlı "yer değiştirme" onarımı.
// Aralık grafiği renklendirmesinde başlangıca göre açgözlü yöntem özdeş
// makinelerde optimaldir; standlar farklı olduğu için burada sezgiseldir,
// onarım adımı pratikte boşluğu kapatıyor (bkz. testler).
package allocation

import (
	"fmt"
	"sort"
	"time"
)

const DefaultBuffer = 15 * time.Minute

type Stand struct {
	ID        int
	Code      string
	MaxSize   byte // 'B'..'F'
	HasBridge bool
	GateID    int
	Active    bool
}

type Turnaround struct {
	ID          int64
	Tail        string
	Size        byte
	NeedsBridge bool
	On, Off     time.Time
	// Aynı uçağın bir önceki turnaround'unun standı (varsa). Mümkünse korunur.
	PreferStand int
}

type Problem struct {
	Stands      []Stand
	Turnarounds []Turnaround
	Buffer      time.Duration
	// Pinned: değiştirilemez atamalar (ör. uçak zaten standda).
	Pinned map[int64]int
	// Current: mevcut plan. Doluysa onarım modunda çalışılır; değişiklik sayısı en aza indirilir.
	Current map[int64]int
	// Changed: zamanı değişen turnaroundlar (onarım modunda önce bunlar yer değiştirir).
	Changed map[int64]bool
}

type Reason string

const (
	ReasonNoCompatible Reason = "NO_COMPATIBLE_STAND"
	ReasonAllOccupied  Reason = "ALL_COMPATIBLE_STANDS_OCCUPIED"
	ReasonBadTimes     Reason = "INVALID_TIMES"
)

type Unallocated struct {
	TurnaroundID int64   `json:"turnaround_id"`
	Tail         string  `json:"tail_number"`
	Reason       Reason  `json:"reason"`
	Detail       string  `json:"detail"`
	BlockedBy    []int64 `json:"blocked_by,omitempty"`
}

type Result struct {
	Assign      map[int64]int
	Unallocated []Unallocated
	Changes     int // Current'a göre stand'ı değişen turnaround sayısı
}

type interval struct {
	start, end time.Time // end = Off + Buffer
	id         int64
}

type board struct {
	buffer time.Duration
	occ    map[int][]interval // stand -> başlangıca göre sıralı aralıklar
}

func newBoard(buf time.Duration) *board {
	return &board{buffer: buf, occ: map[int][]interval{}}
}

func (b *board) span(t Turnaround) interval {
	return interval{start: t.On, end: t.Off.Add(b.buffer), id: t.ID}
}

// conflicts verilen standda t ile çakışan turnaroundları döner.
func (b *board) conflicts(stand int, t Turnaround) []int64 {
	iv := b.span(t)
	var out []int64
	for _, o := range b.occ[stand] {
		if o.start.Before(iv.end) && iv.start.Before(o.end) {
			out = append(out, o.id)
		}
		if !o.start.Before(iv.end) {
			break
		}
	}
	return out
}

func (b *board) free(stand int, t Turnaround) bool { return len(b.conflicts(stand, t)) == 0 }

func (b *board) put(stand int, t Turnaround) {
	iv := b.span(t)
	list := b.occ[stand]
	i := sort.Search(len(list), func(i int) bool { return !list[i].start.Before(iv.start) })
	list = append(list, interval{})
	copy(list[i+1:], list[i:])
	list[i] = iv
	b.occ[stand] = list
}

func (b *board) remove(stand int, id int64) {
	list := b.occ[stand]
	for i, o := range list {
		if o.id == id {
			b.occ[stand] = append(list[:i], list[i+1:]...)
			return
		}
	}
}

// gapBefore standda t'den hemen önceki işgalin bitişiyle t arasındaki boşluk.
func (b *board) gapBefore(stand int, t Turnaround) time.Duration {
	var last time.Time
	for _, o := range b.occ[stand] {
		if !o.end.After(t.On) && o.end.After(last) {
			last = o.end
		}
	}
	if last.IsZero() {
		return 24 * time.Hour
	}
	return t.On.Sub(last)
}

func Compatible(s Stand, t Turnaround) bool {
	return s.Active && t.Size <= s.MaxSize && (!t.NeedsBridge || s.HasBridge)
}

// score küçük olan daha iyi.
func score(b *board, s Stand, t Turnaround, current int) int64 {
	var sc int64
	if current != 0 && s.ID == current {
		sc -= 1_000_000 // onarımda mevcut standda kalmak her şeyden önemli
	}
	if t.PreferStand != 0 && s.ID == t.PreferStand {
		sc -= 100_000 // aynı uçak aynı standda kalsın
	}
	sc += int64(s.MaxSize-t.Size) * 1_000 // büyük standları büyük uçaklara sakla
	if s.HasBridge && !t.NeedsBridge {
		sc += 500 // köprüleri köprü isteyenlere bırak
	}
	// boşluğu en az olan standa koy (sıkı paketleme); dakika cinsinden, en fazla 600
	gap := int64(b.gapBefore(s.ID, t) / time.Minute)
	if gap > 600 {
		gap = 600
	}
	sc += gap
	return sc
}

func Solve(p Problem) Result {
	if p.Buffer == 0 {
		p.Buffer = DefaultBuffer
	}
	res := Result{Assign: map[int64]int{}}
	b := newBoard(p.Buffer)

	stands := make([]Stand, 0, len(p.Stands))
	byID := map[int]Stand{}
	for _, s := range p.Stands {
		if s.Active {
			stands = append(stands, s)
		}
		byID[s.ID] = s
	}
	sort.Slice(stands, func(i, j int) bool { return stands[i].ID < stands[j].ID })

	tas := map[int64]Turnaround{}
	var queue []Turnaround
	for _, t := range p.Turnarounds {
		if !t.Off.After(t.On) {
			res.Unallocated = append(res.Unallocated, Unallocated{TurnaroundID: t.ID, Tail: t.Tail,
				Reason: ReasonBadTimes, Detail: "off-block on-block'tan önce ya da eşit"})
			continue
		}
		tas[t.ID] = t
		if sid, ok := p.Pinned[t.ID]; ok {
			b.put(sid, t)
			res.Assign[t.ID] = sid
			continue
		}
		queue = append(queue, t)
	}

	// Onarım modu: değişmeyenler önce mevcut standlarına oturtulur.
	if p.Current != nil {
		var rest []Turnaround
		sort.Slice(queue, func(i, j int) bool { return less(queue[i], queue[j]) })
		for _, t := range queue {
			cur, ok := p.Current[t.ID]
			if ok && !p.Changed[t.ID] {
				if s, ok := byID[cur]; ok && Compatible(s, t) && b.free(cur, t) {
					b.put(cur, t)
					res.Assign[t.ID] = cur
					continue
				}
			}
			rest = append(rest, t)
		}
		queue = rest
	}

	sort.Slice(queue, func(i, j int) bool { return less(queue[i], queue[j]) })
	var failed []Turnaround
	for _, t := range queue {
		if sid, ok := pick(b, stands, t, p.Current[t.ID]); ok {
			b.put(sid, t)
			res.Assign[t.ID] = sid
		} else {
			failed = append(failed, t)
		}
	}

	// Onarım: yerleşemeyen U için, U'yu engelleyen tek bir X'i başka bir standa kaydırmayı dene.
	for _, u := range failed {
		if repairOne(b, stands, byID, tas, res.Assign, p.Pinned, u) {
			continue
		}
		res.Unallocated = append(res.Unallocated, explain(b, stands, u))
	}

	if p.Current != nil {
		for id, sid := range res.Assign {
			if cur, ok := p.Current[id]; ok && cur != sid {
				res.Changes++
			}
		}
		for _, u := range res.Unallocated {
			if _, ok := p.Current[u.TurnaroundID]; ok {
				res.Changes++
			}
		}
	}
	return res
}

// Önce erken başlayan, eşitlikte büyük uçak (daha az seçeneği var), sonra id.
func less(a, b Turnaround) bool {
	if !a.On.Equal(b.On) {
		return a.On.Before(b.On)
	}
	if a.Size != b.Size {
		return a.Size > b.Size
	}
	return a.ID < b.ID
}

func pick(b *board, stands []Stand, t Turnaround, current int) (int, bool) {
	best, bestScore := 0, int64(0)
	for _, s := range stands {
		if !Compatible(s, t) || !b.free(s.ID, t) {
			continue
		}
		sc := score(b, s, t, current)
		if best == 0 || sc < bestScore {
			best, bestScore = s.ID, sc
		}
	}
	return best, best != 0
}

func repairOne(b *board, stands []Stand, byID map[int]Stand, tas map[int64]Turnaround,
	assign map[int64]int, pinned map[int64]int, u Turnaround) bool {
	for _, s := range stands {
		if !Compatible(s, u) {
			continue
		}
		blockers := b.conflicts(s.ID, u)
		if len(blockers) != 1 {
			continue
		}
		x := tas[blockers[0]]
		if _, isPinned := pinned[x.ID]; isPinned {
			continue
		}
		b.remove(s.ID, x.ID)
		b.put(s.ID, u)
		alt, ok := pick(b, stands, x, 0)
		if ok && alt != s.ID {
			b.put(alt, x)
			assign[x.ID] = alt
			assign[u.ID] = s.ID
			return true
		}
		// geri al
		b.remove(s.ID, u.ID)
		b.put(s.ID, x)
	}
	return false
}

func explain(b *board, stands []Stand, u Turnaround) Unallocated {
	out := Unallocated{TurnaroundID: u.ID, Tail: u.Tail}
	var compatible []Stand
	for _, s := range stands {
		if Compatible(s, u) {
			compatible = append(compatible, s)
		}
	}
	if len(compatible) == 0 {
		out.Reason = ReasonNoCompatible
		need := fmt.Sprintf("boyut %c", u.Size)
		if u.NeedsBridge {
			need += " + köprü"
		}
		out.Detail = need + " için aktif stand yok"
		return out
	}
	out.Reason = ReasonAllOccupied
	seen := map[int64]bool{}
	for _, s := range compatible {
		for _, id := range b.conflicts(s.ID, u) {
			if !seen[id] {
				seen[id] = true
				out.BlockedBy = append(out.BlockedBy, id)
			}
		}
	}
	sort.Slice(out.BlockedBy, func(i, j int) bool { return out.BlockedBy[i] < out.BlockedBy[j] })
	out.Detail = fmt.Sprintf("%d uyumlu standın tamamı %s–%s UTC arasında dolu (15 dk tampon dahil)",
		len(compatible), u.On.UTC().Format("15:04"), u.Off.UTC().Format("15:04"))
	return out
}

// Validate bir atamanın kurallara uyduğunu bağımsız olarak kontrol eder (testler ve doğrulama için).
func Validate(p Problem, assign map[int64]int) []string {
	if p.Buffer == 0 {
		p.Buffer = DefaultBuffer
	}
	byID := map[int]Stand{}
	for _, s := range p.Stands {
		byID[s.ID] = s
	}
	tas := map[int64]Turnaround{}
	for _, t := range p.Turnarounds {
		tas[t.ID] = t
	}
	var problems []string
	perStand := map[int][]Turnaround{}
	for id, sid := range assign {
		t, s := tas[id], byID[sid]
		if !Compatible(s, t) {
			problems = append(problems, fmt.Sprintf("turnaround %d stand %s ile uyumsuz", id, s.Code))
		}
		perStand[sid] = append(perStand[sid], t)
	}
	for sid, list := range perStand {
		sort.Slice(list, func(i, j int) bool { return list[i].On.Before(list[j].On) })
		for i := 1; i < len(list); i++ {
			if list[i].On.Before(list[i-1].Off.Add(p.Buffer)) {
				problems = append(problems, fmt.Sprintf("stand %s: %d ve %d arasında tampon ihlali",
					byID[sid].Code, list[i-1].ID, list[i].ID))
			}
		}
	}
	return problems
}
