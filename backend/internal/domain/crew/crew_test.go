package crew

import (
	"strings"
	"testing"
	"time"
)

var day = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

func h(hh, mm int) time.Time { return day.Add(time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute) }

func fuelCrew(id int, certUntil time.Time) Crew {
	return Crew{ID: id, Type: Fuel,
		Certs:  map[string]time.Time{"FUEL_HANDLING": certUntil, "RAMP_SAFETY": certUntil},
		Shifts: []Window{{h(6, 0), h(14, 0)}}}
}

func task(id int64, tt TaskType, s, e time.Time) Task {
	return Task{ID: id, TurnaroundID: id, Type: tt, Start: s, End: e, Deadline: e.Add(60 * time.Minute), TargetEnd: e.Add(10 * time.Minute)}
}

func trucks(n int) []Equipment {
	var out []Equipment
	for i := 1; i <= n; i++ {
		out = append(out, Equipment{ID: i, Kind: "FUEL_TRUCK"})
	}
	return out
}

func TestExpiredCertificateIsNotUsed(t *testing.T) {
	p := Problem{
		Crews:     []Crew{fuelCrew(1, day.AddDate(0, 0, -1)), fuelCrew(2, day.AddDate(1, 0, 0))},
		Equipment: trucks(1),
		Tasks:     []Task{task(1, Fuel, h(8, 0), h(8, 20))},
	}
	r := Assign(p)
	if r.Assign[1].CrewID != 2 {
		t.Fatalf("süresi dolmuş sertifikalı ekip seçilmemeli: %+v", r.Assign)
	}
}

func TestCertificateValidOnLastDay(t *testing.T) {
	c := fuelCrew(1, day) // bugün sona eriyor, bugün geçerli
	if !CertValid(c, Fuel, h(13, 0)) {
		t.Fatal("son gün geçerli sayılmalı")
	}
}

func TestOnlyCertExpiredGivesReason(t *testing.T) {
	p := Problem{Crews: []Crew{fuelCrew(1, day.AddDate(0, 0, -3))}, Equipment: trucks(1),
		Tasks: []Task{task(1, Fuel, h(8, 0), h(8, 20))}}
	r := Assign(p)
	if len(r.Unassigned) != 1 || r.Unassigned[0].Reason != "NO_VALID_CERTIFICATE" {
		t.Fatalf("sertifika nedeni bekleniyordu: %+v", r.Unassigned)
	}
	if len(r.Risks) != 1 || r.Risks[0].Level != "CRITICAL" {
		t.Fatalf("kritik risk bekleniyordu: %+v", r.Risks)
	}
}

func TestTaskOutsideShiftIsShiftedOrRejected(t *testing.T) {
	p := Problem{Crews: []Crew{fuelCrew(1, day.AddDate(1, 0, 0))}, Equipment: trucks(1),
		Tasks: []Task{task(1, Fuel, h(15, 0), h(15, 20))}}
	r := Assign(p)
	if len(r.Unassigned) != 1 || r.Unassigned[0].Reason != "NO_CREW_ON_SHIFT" {
		t.Fatalf("vardiya dışı görev atanmamalı: %+v", r)
	}
}

func TestRestRuleDropsShortRestShift(t *testing.T) {
	c := fuelCrew(1, day.AddDate(1, 0, 0))
	c.Shifts = []Window{{h(6, 0), h(14, 0)}, {h(22, 0), h(30, 0)}} // 8 saat dinlenme
	u, v := UsableShifts(c)
	if len(u) != 1 || len(v) != 1 || v[0].RestHrs != 8 {
		t.Fatalf("11 saatten kısa dinlenmeli vardiya düşmeli: usable=%v viol=%v", u, v)
	}
	p := Problem{Crews: []Crew{c}, Equipment: trucks(1), Tasks: []Task{task(1, Fuel, h(23, 0), h(23, 20))}}
	if r := Assign(p); len(r.Unassigned) != 1 {
		t.Fatalf("dinlenme ihlalli vardiyada görev atanmamalı: %+v", r.Assign)
	}
}

func TestValidateRest(t *testing.T) {
	existing := []Window{{h(6, 0), h(14, 0)}}
	if err := ValidateRest(existing, Window{h(25, 0), h(33, 0)}); err != nil {
		t.Fatalf("11 saat dinlenme yeterli: %v", err)
	}
	if err := ValidateRest(existing, Window{h(24, 0), h(32, 0)}); err == nil || !strings.Contains(err.Error(), "10.0") {
		t.Fatalf("10 saat dinlenme reddedilmeli: %v", err)
	}
	if err := ValidateRest(existing, Window{h(13, 0), h(20, 0)}); err == nil {
		t.Fatal("çakışan vardiya reddedilmeli")
	}
	if err := ValidateRest(existing, Window{h(-10, 0), h(-4, 0)}); err == nil {
		t.Fatal("önceden gelen ve 10 saat dinlenme bırakan vardiya reddedilmeli")
	}
}

func TestLimitedFuelTrucks(t *testing.T) {
	var crews []Crew
	for i := 1; i <= 6; i++ {
		crews = append(crews, fuelCrew(i, day.AddDate(1, 0, 0)))
	}
	var tasks []Task
	for i := int64(1); i <= 6; i++ {
		tasks = append(tasks, task(i, Fuel, h(8, 0), h(8, 20)))
	}
	r := Assign(Problem{Crews: crews, Equipment: trucks(4), Tasks: tasks})
	sameTime := 0
	used := map[int]bool{}
	for _, a := range r.Assign {
		if a.Start.Equal(h(8, 0)) {
			sameTime++
			if used[a.EquipmentID] {
				t.Fatalf("aynı araç iki görevde")
			}
			used[a.EquipmentID] = true
		}
	}
	if sameTime != 4 {
		t.Fatalf("aynı anda en fazla 4 yakıt görevi başlamalı, %d", sameTime)
	}
	if len(r.Risks) != 2 {
		t.Fatalf("araç bekleyen 2 görev için risk uyarısı bekleniyordu: %+v", r.Risks)
	}
}

func TestDelayedStartProducesWarning(t *testing.T) {
	c := fuelCrew(1, day.AddDate(1, 0, 0))
	tasks := []Task{task(1, Fuel, h(8, 0), h(8, 30)), task(2, Fuel, h(8, 10), h(8, 30))}
	r := Assign(Problem{Crews: []Crew{c}, Equipment: trucks(4), Tasks: tasks})
	a := r.Assign[2]
	if a.DelayMin != 25 { // 8:30 bitiş + 5 dk geçiş = 8:35
		t.Fatalf("ikinci görev 25 dk kaymalıydı: %+v", a)
	}
	if len(r.Risks) != 1 || r.Risks[0].Level != "WARNING" {
		t.Fatalf("hedef aşımı uyarısı bekleniyordu: %+v", r.Risks)
	}
}

func TestNoCrewOverlapAndTravelTime(t *testing.T) {
	c := Crew{ID: 1, Type: Cleaning, Shifts: []Window{{h(6, 0), h(14, 0)}}}
	tasks := []Task{task(1, Cleaning, h(9, 0), h(9, 30)), task(2, Cleaning, h(9, 32), h(10, 0))}
	r := Assign(Problem{Crews: []Crew{c}, Tasks: tasks})
	if r.Assign[2].Start.Before(h(9, 35)) {
		t.Fatalf("geçiş süresi gözetilmeli: %+v", r.Assign[2])
	}
}

func TestPinnedTasksBlockCapacity(t *testing.T) {
	c := Crew{ID: 1, Type: Baggage, Certs: map[string]time.Time{"RAMP_SAFETY": day.AddDate(1, 0, 0)}, Shifts: []Window{{h(6, 0), h(14, 0)}}}
	pinned := task(1, Baggage, h(9, 0), h(9, 40))
	pinned.Pinned, pinned.CrewID = true, 1
	r := Assign(Problem{Crews: []Crew{c}, Tasks: []Task{task(2, Baggage, h(9, 10), h(9, 30)), pinned}})
	if r.Assign[2].Start.Before(h(9, 45)) {
		t.Fatalf("sabit görevin üstüne atanmamalı: %+v", r.Assign[2])
	}
}

func TestLoadBalancing(t *testing.T) {
	cs := []Crew{{ID: 1, Type: Cleaning, Shifts: []Window{{h(6, 0), h(14, 0)}}}, {ID: 2, Type: Cleaning, Shifts: []Window{{h(6, 0), h(14, 0)}}}}
	tasks := []Task{task(1, Cleaning, h(8, 0), h(8, 20)), task(2, Cleaning, h(10, 0), h(10, 20))}
	r := Assign(Problem{Crews: cs, Tasks: tasks})
	if r.Assign[1].CrewID == r.Assign[2].CrewID {
		t.Fatalf("iş yükü dengelenmeli: %+v", r.Assign)
	}
}

func TestPreferredCrewKept(t *testing.T) {
	cs := []Crew{{ID: 1, Type: Cleaning, Shifts: []Window{{h(6, 0), h(14, 0)}}}, {ID: 2, Type: Cleaning, Shifts: []Window{{h(6, 0), h(14, 0)}}}}
	k := task(1, Cleaning, h(8, 0), h(8, 20))
	k.PreferCrewID = 2
	if r := Assign(Problem{Crews: cs, Tasks: []Task{k}}); r.Assign[1].CrewID != 2 {
		t.Fatalf("tercih edilen ekip korunmalı")
	}
}

func TestTemplates(t *testing.T) {
	specs := Templates(h(10, 0), h(10, 45), 'C', true)
	if len(specs) != 4 || specs[0].Type != Bridge {
		t.Fatalf("köprülü turnaround 4 görev üretmeli: %+v", specs)
	}
	for _, s := range specs {
		if s.End.After(h(10, 40)) {
			t.Fatalf("%s off-block-5dk sonrasına taşmamalı: %v", s.Type, s.End)
		}
		if !s.End.After(s.Start) {
			t.Fatalf("%s boş pencere", s.Type)
		}
	}
	if got := Templates(h(10, 0), h(12, 0), 'E', false); len(got) != 3 || got[0].End.Sub(got[0].Start) != 40*time.Minute {
		t.Fatalf("büyük uçak bagaj süresi 40 dk olmalı: %+v", got)
	}
}
