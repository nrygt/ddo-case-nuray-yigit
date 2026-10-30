// Package propagation bir uçuş gecikmesinin zincirleme etkisini hesaplar ve
// etkilenen stand/ekip atamalarını en az değişiklikle yeniden planlar (T-2).
//
// Adımlar:
//  1. Geciken uçuşun turnaround'u kayar (geliş gecikirse kalkış, minimum
//     turnaround süresini koruyacak kadar ileri itilir).
//  2. Aynı uçağın sonraki turnaroundları, dış meydanda RecoveryPerLeg kadar
//     toparlanabileceği varsayımıyla kayar; kayma sıfırlanınca zincir durur.
//  3. Stand planı onarım modunda yeniden çözülür: değişmeyenler yerinde kalır,
//     önce kayanlar yer değiştirir.
//  4. Kayan turnaroundların görevleri aynı ofsetle kaydırılıp yeniden atanır;
//     diğer görevler mevcut ekipleriyle sabit tutulur.
//
// Hepsi saf hesaplamadır; veritabanına yazmak çağıranın işidir. Böylece aynı
// fonksiyon hem "ne olur" önizlemesini hem de uygulamayı besler.
package propagation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"pistkoordinat/internal/domain/allocation"
	"pistkoordinat/internal/domain/crew"
)

const DefaultRecovery = 10 * time.Minute

type Turnaround struct {
	ID          int64
	Tail        string
	ArrFlightID int64
	DepFlightID int64
	On, Off     time.Time
	MinTurn     time.Duration
	Size        byte
	NeedsBridge bool
	// Locked: uçak zaten standda/kalkmış, stand değiştirilemez.
	Locked bool
}

type Timeline struct {
	Turnarounds []Turnaround
	Stands      []allocation.Stand
	StandOf     map[int64]int // turnaround -> mevcut stand
	Tasks       []crew.Task
	Crews       []crew.Crew
	Equipment   []crew.Equipment
	Recovery    time.Duration
}

type Trigger struct {
	FlightID int64     `json:"flight_id"`
	NewTime  time.Time `json:"new_time"`
}

type TurnaroundChange struct {
	TurnaroundID int64     `json:"turnaround_id"`
	Tail         string    `json:"tail_number"`
	OldOn        time.Time `json:"old_on_block"`
	NewOn        time.Time `json:"new_on_block"`
	OldOff       time.Time `json:"old_off_block"`
	NewOff       time.Time `json:"new_off_block"`
}

type FlightChange struct {
	FlightID int64     `json:"flight_id"`
	Old      time.Time `json:"old_est_time"`
	New      time.Time `json:"new_est_time"`
}

type StandChange struct {
	TurnaroundID int64 `json:"turnaround_id"`
	OldStand     int   `json:"old_stand_id"`
	NewStand     int   `json:"new_stand_id"` // 0: yer bulunamadı
}

type TaskChange struct {
	TaskID   int64     `json:"task_id"`
	OldCrew  int       `json:"old_crew_id"`
	NewCrew  int       `json:"new_crew_id"`
	NewStart time.Time `json:"new_start"`
	NewEnd   time.Time `json:"new_end"`
	NewEquip int       `json:"new_equipment_id,omitempty"`
}

type Plan struct {
	Trigger     Trigger                  `json:"trigger"`
	Turnarounds []TurnaroundChange       `json:"turnarounds"`
	Flights     []FlightChange           `json:"flights"`
	Stands      []StandChange            `json:"stands"`
	Tasks       []TaskChange             `json:"tasks"`
	Unallocated []allocation.Unallocated `json:"unallocated"`
	Unassigned  []crew.Unassigned        `json:"unassigned"`
	Risks       []crew.Risk              `json:"risks"`
	// Atama değişikliği sayısı: stand değişen turnaround + ekibi değişen görev.
	AssignmentChanges int    `json:"assignment_changes"`
	Fingerprint       string `json:"fingerprint"`
}

var ErrUnknownFlight = errors.New("uçuş bu zaman çizelgesinde yok")

func Replan(tl Timeline, trig Trigger) (Plan, error) {
	if tl.Recovery == 0 {
		tl.Recovery = DefaultRecovery
	}
	plan := Plan{Trigger: trig, Turnarounds: []TurnaroundChange{}, Flights: []FlightChange{},
		Stands: []StandChange{}, Tasks: []TaskChange{}}

	shifted, err := Shift(tl.Turnarounds, trig, tl.Recovery)
	if err != nil {
		return plan, err
	}
	orig := map[int64]Turnaround{}
	for _, t := range tl.Turnarounds {
		orig[t.ID] = t
	}
	changed := map[int64]bool{}
	for _, t := range shifted {
		o := orig[t.ID]
		if t.On.Equal(o.On) && t.Off.Equal(o.Off) {
			continue
		}
		changed[t.ID] = true
		plan.Turnarounds = append(plan.Turnarounds, TurnaroundChange{TurnaroundID: t.ID, Tail: t.Tail,
			OldOn: o.On, NewOn: t.On, OldOff: o.Off, NewOff: t.Off})
		if t.ArrFlightID != 0 && !t.On.Equal(o.On) {
			plan.Flights = append(plan.Flights, FlightChange{FlightID: t.ArrFlightID, Old: o.On, New: t.On})
		}
		if t.DepFlightID != 0 && !t.Off.Equal(o.Off) {
			plan.Flights = append(plan.Flights, FlightChange{FlightID: t.DepFlightID, Old: o.Off, New: t.Off})
		}
	}

	// --- stand onarımı
	prob := allocation.Problem{Stands: tl.Stands, Current: map[int64]int{}, Changed: changed, Pinned: map[int64]int{}}
	for _, t := range shifted {
		at := allocation.Turnaround{ID: t.ID, Tail: t.Tail, Size: t.Size, NeedsBridge: t.NeedsBridge, On: t.On, Off: t.Off}
		prob.Turnarounds = append(prob.Turnarounds, at)
		if sid, ok := tl.StandOf[t.ID]; ok {
			prob.Current[t.ID] = sid
			if t.Locked {
				prob.Pinned[t.ID] = sid
			}
		}
	}
	ares := allocation.Solve(prob)
	for _, t := range shifted {
		old := tl.StandOf[t.ID]
		nw := ares.Assign[t.ID]
		if old != nw {
			plan.Stands = append(plan.Stands, StandChange{TurnaroundID: t.ID, OldStand: old, NewStand: nw})
		}
	}
	plan.Unallocated = ares.Unallocated

	// --- görevleri kaydır ve yeniden ata
	newTA := map[int64]Turnaround{}
	for _, t := range shifted {
		newTA[t.ID] = t
	}
	var tasks []crew.Task
	origTask := map[int64]crew.Task{}
	for _, k := range tl.Tasks {
		origTask[k.ID] = k
		if changed[k.TurnaroundID] && !k.Pinned {
			t := newTA[k.TurnaroundID]
			delta := t.On.Sub(orig[k.TurnaroundID].On)
			k.Start = k.Start.Add(delta)
			k.End = k.End.Add(delta)
			limit := t.Off.Add(-5 * time.Minute)
			if k.End.After(limit) && limit.After(k.Start) {
				k.End = limit
			}
			k.Deadline = t.Off
			k.TargetEnd = k.TargetEnd.Add(delta)
			k.PreferCrewID = k.CrewID
			k.CrewID, k.EquipmentID = 0, 0
		} else {
			k.Pinned = true // değişmeyen görevler mevcut ekibiyle kalır
		}
		tasks = append(tasks, k)
	}
	cres := crew.Assign(crew.Problem{Crews: tl.Crews, Equipment: tl.Equipment, Tasks: tasks})
	for _, k := range tasks {
		if k.Pinned {
			continue
		}
		o := origTask[k.ID]
		a, ok := cres.Assign[k.ID]
		if !ok {
			plan.Tasks = append(plan.Tasks, TaskChange{TaskID: k.ID, OldCrew: o.CrewID, NewCrew: 0, NewStart: k.Start, NewEnd: k.End})
			continue
		}
		plan.Tasks = append(plan.Tasks, TaskChange{TaskID: k.ID, OldCrew: o.CrewID, NewCrew: a.CrewID,
			NewStart: a.Start, NewEnd: a.End, NewEquip: a.EquipmentID})
	}
	plan.Unassigned = cres.Unassigned
	plan.Risks = cres.Risks

	plan.AssignmentChanges = len(plan.Stands)
	for _, c := range plan.Tasks {
		if c.OldCrew != c.NewCrew {
			plan.AssignmentChanges++
		}
	}
	sortPlan(&plan)
	plan.Fingerprint = fingerprint(plan)
	return plan, nil
}

// Shift gecikmeyi aynı uçağın turnaround zincirine yayar.
func Shift(tas []Turnaround, trig Trigger, recovery time.Duration) ([]Turnaround, error) {
	out := append([]Turnaround(nil), tas...)
	idx := -1
	for i, t := range out {
		if t.ArrFlightID == trig.FlightID || t.DepFlightID == trig.FlightID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, ErrUnknownFlight
	}
	start := out[idx]
	if start.Locked {
		return nil, fmt.Errorf("turnaround %d kilitli (uçak standda/kalkmış)", start.ID)
	}

	cur := start
	if start.ArrFlightID == trig.FlightID {
		cur.On = trig.NewTime
		if minOff := cur.On.Add(cur.MinTurn); cur.Off.Before(minOff) {
			cur.Off = minOff
		}
	} else {
		cur.Off = trig.NewTime
		if cur.Off.Before(cur.On.Add(cur.MinTurn)) {
			return nil, fmt.Errorf("yeni kalkış saati minimum turnaround süresinin altında")
		}
	}
	out[idx] = cur

	// aynı kuyruk numarasının sonraki turnaroundları
	var chain []int
	for i, t := range out {
		if t.Tail == start.Tail && t.On.After(start.On) && i != idx {
			chain = append(chain, i)
		}
	}
	sort.Slice(chain, func(a, b int) bool { return out[chain[a]].On.Before(out[chain[b]].On) })

	prevOrig, prevNew := start, cur
	for _, i := range chain {
		next := out[i]
		gap := next.On.Sub(prevOrig.Off) // dışarıdaki uçuş + dış meydan süresi
		earliest := prevNew.Off.Add(gap - recovery)
		origNext := next
		if earliest.After(next.On) {
			next.On = earliest
			if minOff := next.On.Add(next.MinTurn); next.Off.Before(minOff) {
				next.Off = minOff
			}
		}
		out[i] = next
		if next.On.Equal(origNext.On) && next.Off.Equal(origNext.Off) {
			break // gecikme emildi
		}
		prevOrig, prevNew = origNext, next
	}
	return out, nil
}

func sortPlan(p *Plan) {
	sort.Slice(p.Turnarounds, func(i, j int) bool { return p.Turnarounds[i].TurnaroundID < p.Turnarounds[j].TurnaroundID })
	sort.Slice(p.Flights, func(i, j int) bool { return p.Flights[i].FlightID < p.Flights[j].FlightID })
	sort.Slice(p.Stands, func(i, j int) bool { return p.Stands[i].TurnaroundID < p.Stands[j].TurnaroundID })
	sort.Slice(p.Tasks, func(i, j int) bool { return p.Tasks[i].TaskID < p.Tasks[j].TaskID })
}

// fingerprint planın içeriğine bağlı bir özet: önizleme ile uygulama arasında
// durum değiştiyse uygulama reddedilir.
func fingerprint(p Plan) string {
	cp := p
	cp.Fingerprint = ""
	b, _ := json.Marshal(struct {
		T []TurnaroundChange
		S []StandChange
		K []TaskChange
	}{cp.Turnarounds, cp.Stands, cp.Tasks})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
