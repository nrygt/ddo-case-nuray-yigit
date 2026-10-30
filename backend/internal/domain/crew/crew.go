// Package crew yer hizmeti görevlerine ekip ve ekipman atar (T-3).
//
// Kurallar birlikte değerlendirilir:
//   - ekip görev türüne uygun olmalı ve gerekli belgeleri görev günü geçerli olmalı,
//   - görev ekibin bir vardiyasının içinde kalmalı,
//   - ardışık vardiyalar arasında en az 11 saat dinlenme olmalı (ihlal eden vardiya kullanılmaz),
//   - ekip aynı anda iki işte olamaz (görevler arası geçiş süresi dahil),
//   - yakıt görevi sınırlı sayıdaki yakıt aracından birini ister.
//
// Görev planlanan saatte başlatılamıyorsa 5 dk adımlarla ileri kaydırılır; off-block'a
// sığmıyorsa atanmaz, turnaround hedefini aşıyorsa risk uyarısı üretilir.
package crew

import (
	"fmt"
	"sort"
	"time"
)

type TaskType string

const (
	Fuel     TaskType = "FUEL"
	Baggage  TaskType = "BAGGAGE"
	Cleaning TaskType = "CLEANING"
	Bridge   TaskType = "BRIDGE"
)

const (
	MinRest    = 11 * time.Hour
	Travel     = 5 * time.Minute
	SearchStep = 5 * time.Minute
)

// RequiredCerts görev türüne göre gereken belgeler.
var RequiredCerts = map[TaskType][]string{
	Fuel:     {"FUEL_HANDLING", "RAMP_SAFETY"},
	Bridge:   {"BRIDGE_OPERATION"},
	Baggage:  {"RAMP_SAFETY"},
	Cleaning: {},
}

var typeName = map[TaskType]string{Fuel: "Yakıt", Baggage: "Bagaj", Cleaning: "Temizlik", Bridge: "Köprü"}

var reasonText = map[string]string{
	"NO_CREW_OF_TYPE":      "bu türde ekip yok",
	"NO_VALID_CERTIFICATE": "geçerli belgeli ekip yok",
	"NO_CREW_ON_SHIFT":     "vardiyada ekip yok",
	"NO_EQUIPMENT":         "ekipman yok",
	"CAPACITY_EXHAUSTED":   "ekip ya da araç kapasitesi dolu",
}

// RequiredEquipment görev türüne göre gereken ekipman türü.
var RequiredEquipment = map[TaskType]string{
	Fuel: "FUEL_TRUCK",
}

type Window struct{ Start, End time.Time }

func (w Window) Contains(s, e time.Time) bool { return !s.Before(w.Start) && !e.After(w.End) }
func (w Window) Overlaps(s, e time.Time) bool { return s.Before(w.End) && w.Start.Before(e) }

type Crew struct {
	ID     int
	Name   string
	Type   TaskType
	Certs  map[string]time.Time // belge -> geçerlilik son günü (o gün dahil)
	Shifts []Window
}

type Equipment struct {
	ID   int
	Kind string
}

type Task struct {
	ID           int64
	TurnaroundID int64
	Type         TaskType
	Start, End   time.Time
	// Deadline: görevin en geç bitmesi gereken an (off-block).
	Deadline time.Time
	// TargetEnd: turnaround hedef süresinin dolduğu an; aşılırsa risk uyarısı.
	TargetEnd time.Time
	// Pinned görevler (başlamış/bitmiş) yerinden oynatılmaz.
	Pinned      bool
	CrewID      int
	EquipmentID int
	// PreferCrewID: yeniden planlamada mümkünse aynı ekip kalsın (en az değişiklik).
	PreferCrewID int
}

type Assignment struct {
	TaskID      int64     `json:"task_id"`
	CrewID      int       `json:"crew_id"`
	EquipmentID int       `json:"equipment_id,omitempty"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	DelayMin    int       `json:"delay_min"`
}

type Unassigned struct {
	TaskID int64  `json:"task_id"`
	Reason string `json:"reason"`
}

type Risk struct {
	TurnaroundID int64  `json:"turnaround_id"`
	TaskID       int64  `json:"task_id"`
	Level        string `json:"level"` // WARNING: hedef aşılıyor, CRITICAL: atanamadı
	Message      string `json:"message"`
}

type ShiftViolation struct {
	CrewID  int     `json:"crew_id"`
	Shift   Window  `json:"shift"`
	RestHrs float64 `json:"rest_hours"`
}

type Problem struct {
	Crews     []Crew
	Equipment []Equipment
	Tasks     []Task
}

type Result struct {
	Assign          map[int64]Assignment
	Unassigned      []Unassigned
	Risks           []Risk
	ShiftViolations []ShiftViolation
}

// UsableShifts dinlenme kuralını uygular: önceki kullanılabilir vardiyanın bitişinden
// 11 saat geçmeden başlayan vardiya atlanır.
func UsableShifts(c Crew) (usable []Window, violations []ShiftViolation) {
	shifts := append([]Window(nil), c.Shifts...)
	sort.Slice(shifts, func(i, j int) bool { return shifts[i].Start.Before(shifts[j].Start) })
	var last *Window
	for i := range shifts {
		s := shifts[i]
		if last != nil {
			rest := s.Start.Sub(last.End)
			if rest < MinRest {
				violations = append(violations, ShiftViolation{CrewID: c.ID, Shift: s, RestHrs: rest.Hours()})
				continue
			}
		}
		usable = append(usable, s)
		last = &usable[len(usable)-1]
	}
	return usable, violations
}

// ValidateRest yeni bir vardiyanın mevcutlara eklenmesinin dinlenme kuralını bozup bozmadığını söyler.
func ValidateRest(existing []Window, next Window) error {
	for _, w := range existing {
		if w.Overlaps(next.Start, next.End) {
			return fmt.Errorf("vardiya mevcut bir vardiyayla çakışıyor")
		}
		if !next.Start.Before(w.End) && next.Start.Sub(w.End) < MinRest {
			return fmt.Errorf("önceki vardiyadan sonra %.1f saat dinlenme var, en az 11 saat gerekli", next.Start.Sub(w.End).Hours())
		}
		if !w.Start.Before(next.End) && w.Start.Sub(next.End) < MinRest {
			return fmt.Errorf("sonraki vardiyaya %.1f saat kalıyor, en az 11 saat gerekli", w.Start.Sub(next.End).Hours())
		}
	}
	return nil
}

func CertValid(c Crew, t TaskType, on time.Time) bool {
	day := time.Date(on.Year(), on.Month(), on.Day(), 0, 0, 0, 0, on.Location())
	for _, cert := range RequiredCerts[t] {
		until, ok := c.Certs[cert]
		if !ok || until.Before(day) {
			return false
		}
	}
	return true
}

type schedule struct {
	busy map[int][]Window // kaynak -> dolu aralıklar
}

func (s *schedule) free(id int, start, end time.Time, gap time.Duration) bool {
	for _, w := range s.busy[id] {
		if start.Before(w.End.Add(gap)) && w.Start.Before(end.Add(gap)) {
			return false
		}
	}
	return true
}

func (s *schedule) add(id int, start, end time.Time) {
	s.busy[id] = append(s.busy[id], Window{start, end})
}

func Assign(p Problem) Result {
	res := Result{Assign: map[int64]Assignment{}}
	crews := map[TaskType][]Crew{}
	usable := map[int][]Window{}
	load := map[int]time.Duration{}
	for _, c := range p.Crews {
		u, v := UsableShifts(c)
		usable[c.ID] = u
		res.ShiftViolations = append(res.ShiftViolations, v...)
		crews[c.Type] = append(crews[c.Type], c)
	}
	for t := range crews {
		sort.Slice(crews[t], func(i, j int) bool { return crews[t][i].ID < crews[t][j].ID })
	}
	eqByKind := map[string][]int{}
	for _, e := range p.Equipment {
		eqByKind[e.Kind] = append(eqByKind[e.Kind], e.ID)
	}
	for k := range eqByKind {
		sort.Ints(eqByKind[k])
	}

	crewSch := &schedule{busy: map[int][]Window{}}
	eqSch := &schedule{busy: map[int][]Window{}}

	tasks := append([]Task(nil), p.Tasks...)
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Pinned != tasks[j].Pinned {
			return tasks[i].Pinned // önce sabitler yerleşsin
		}
		if !tasks[i].Start.Equal(tasks[j].Start) {
			return tasks[i].Start.Before(tasks[j].Start)
		}
		return tasks[i].ID < tasks[j].ID
	})

	for _, t := range tasks {
		if t.Pinned {
			if t.CrewID != 0 {
				crewSch.add(t.CrewID, t.Start, t.End)
				load[t.CrewID] += t.End.Sub(t.Start)
			}
			if t.EquipmentID != 0 {
				eqSch.add(t.EquipmentID, t.Start, t.End)
			}
			res.Assign[t.ID] = Assignment{TaskID: t.ID, CrewID: t.CrewID, EquipmentID: t.EquipmentID, Start: t.Start, End: t.End}
			continue
		}
		a, reason := place(t, crews[t.Type], usable, load, crewSch, eqSch, eqByKind)
		if reason != "" {
			res.Unassigned = append(res.Unassigned, Unassigned{TaskID: t.ID, Reason: reason})
			res.Risks = append(res.Risks, Risk{TurnaroundID: t.TurnaroundID, TaskID: t.ID, Level: "CRITICAL",
				Message: fmt.Sprintf("%s görevi kalkıştan önce yapılamıyor (%s)", typeName[t.Type], reasonText[reason])})
			continue
		}
		crewSch.add(a.CrewID, a.Start, a.End)
		load[a.CrewID] += a.End.Sub(a.Start)
		if a.EquipmentID != 0 {
			eqSch.add(a.EquipmentID, a.Start, a.End)
		}
		res.Assign[t.ID] = a
		if !t.TargetEnd.IsZero() && a.End.After(t.TargetEnd) {
			res.Risks = append(res.Risks, Risk{TurnaroundID: t.TurnaroundID, TaskID: t.ID, Level: "WARNING",
				Message: fmt.Sprintf("%s görevi %d dk gecikmeyle başlıyor, hedef %d dk aşılıyor",
					typeName[t.Type], a.DelayMin, int(a.End.Sub(t.TargetEnd).Minutes()))})
		}
	}
	return res
}

func place(t Task, candidates []Crew, usable map[int][]Window, load map[int]time.Duration,
	crewSch, eqSch *schedule, eqByKind map[string][]int) (Assignment, string) {

	dur := t.End.Sub(t.Start)
	if len(candidates) == 0 {
		return Assignment{}, "NO_CREW_OF_TYPE"
	}
	var certified []Crew
	for _, c := range candidates {
		if CertValid(c, t.Type, t.Start) {
			certified = append(certified, c)
		}
	}
	if len(certified) == 0 {
		return Assignment{}, "NO_VALID_CERTIFICATE"
	}
	latest := t.Deadline.Add(-dur)
	if t.Deadline.IsZero() {
		latest = t.Start
	}
	eqKind := RequiredEquipment[t.Type]
	sawShift := false

	for start := t.Start; !start.After(latest); start = start.Add(SearchStep) {
		end := start.Add(dur)
		var best *Crew
		for i := range certified {
			c := &certified[i]
			inShift := false
			for _, w := range usable[c.ID] {
				if w.Contains(start, end) {
					inShift = true
					break
				}
			}
			if !inShift {
				continue
			}
			sawShift = true
			if !crewSch.free(c.ID, start, end, Travel) {
				continue
			}
			if c.ID == t.PreferCrewID {
				best = c
				break
			}
			if best == nil || load[c.ID] < load[best.ID] {
				best = c
			}
		}
		if best == nil {
			continue
		}
		eqID := 0
		if eqKind != "" {
			for _, id := range eqByKind[eqKind] {
				if eqSch.free(id, start, end, 0) {
					eqID = id
					break
				}
			}
			if eqID == 0 {
				continue
			}
		}
		return Assignment{TaskID: t.ID, CrewID: best.ID, EquipmentID: eqID, Start: start, End: end,
			DelayMin: int(start.Sub(t.Start).Minutes())}, ""
	}
	switch {
	case !sawShift:
		return Assignment{}, "NO_CREW_ON_SHIFT"
	case eqKind != "" && len(eqByKind[eqKind]) == 0:
		return Assignment{}, "NO_EQUIPMENT"
	default:
		return Assignment{}, "CAPACITY_EXHAUSTED"
	}
}

// TaskSpec bir turnaround için üretilecek görev şablonu.
type TaskSpec struct {
	Type       TaskType
	Start, End time.Time
}

// Templates turnaround süresine ve uçak boyutuna göre görev pencerelerini üretir.
// Kısa turnaroundlarda pencereler off-block'tan 5 dk önceye kırpılır.
func Templates(on, off time.Time, size byte, needsBridge bool) []TaskSpec {
	big := size >= 'D'
	dur := func(small, large int) time.Duration {
		if big {
			return time.Duration(large) * time.Minute
		}
		return time.Duration(small) * time.Minute
	}
	limit := off.Add(-5 * time.Minute)
	mk := func(t TaskType, offset time.Duration, d time.Duration) TaskSpec {
		s := on.Add(offset)
		e := s.Add(d)
		if e.After(limit) {
			e = limit
		}
		if !e.After(s) {
			s = on
			e = on.Add(time.Minute)
		}
		return TaskSpec{Type: t, Start: s, End: e}
	}
	var out []TaskSpec
	if needsBridge {
		out = append(out, mk(Bridge, 0, 10*time.Minute))
	}
	out = append(out,
		mk(Baggage, 5*time.Minute, dur(25, 40)),
		mk(Cleaning, 10*time.Minute, dur(20, 35)),
		mk(Fuel, 15*time.Minute, dur(15, 30)),
	)
	return out
}
