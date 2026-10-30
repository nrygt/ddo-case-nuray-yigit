package propagation

import (
	"testing"
	"time"

	"pistkoordinat/internal/domain/allocation"
	"pistkoordinat/internal/domain/crew"
)

var base = time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC)

func m(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }

// İki uçak: TC-AAA iki kez geliyor (sabah ve öğlen), TC-BBB öğlen aynı standda.
func fixture() Timeline {
	stands := []allocation.Stand{
		{ID: 1, Code: "A1", MaxSize: 'C', Active: true},
		{ID: 2, Code: "A2", MaxSize: 'C', Active: true},
	}
	tas := []Turnaround{
		{ID: 10, Tail: "TC-AAA", ArrFlightID: 100, DepFlightID: 101, On: m(0), Off: m(45), MinTurn: 35 * time.Minute, Size: 'C'},
		{ID: 11, Tail: "TC-AAA", ArrFlightID: 102, DepFlightID: 103, On: m(180), Off: m(225), MinTurn: 35 * time.Minute, Size: 'C'},
		{ID: 20, Tail: "TC-BBB", ArrFlightID: 200, DepFlightID: 201, On: m(70), Off: m(120), MinTurn: 35 * time.Minute, Size: 'C'},
		{ID: 30, Tail: "TC-CCC", ArrFlightID: 300, DepFlightID: 301, On: m(240), Off: m(300), MinTurn: 35 * time.Minute, Size: 'C'},
	}
	crews := []crew.Crew{
		{ID: 1, Type: crew.Cleaning, Shifts: []crew.Window{{Start: m(-60), End: m(600)}}},
		{ID: 2, Type: crew.Cleaning, Shifts: []crew.Window{{Start: m(-60), End: m(600)}}},
	}
	tasks := []crew.Task{
		{ID: 1, TurnaroundID: 10, Type: crew.Cleaning, Start: m(10), End: m(30), Deadline: m(45), TargetEnd: m(45), CrewID: 1},
		{ID: 2, TurnaroundID: 20, Type: crew.Cleaning, Start: m(80), End: m(100), Deadline: m(120), TargetEnd: m(120), CrewID: 1},
		{ID: 3, TurnaroundID: 11, Type: crew.Cleaning, Start: m(190), End: m(210), Deadline: m(225), TargetEnd: m(225), CrewID: 2},
	}
	return Timeline{
		Turnarounds: tas, Stands: stands,
		StandOf: map[int64]int{10: 1, 11: 1, 20: 1, 30: 2},
		Tasks:   tasks, Crews: crews,
	}
}

func TestArrivalDelayPushesDepartureByMinTurn(t *testing.T) {
	out, err := Shift(fixture().Turnarounds, Trigger{FlightID: 100, NewTime: m(30)}, DefaultRecovery)
	if err != nil {
		t.Fatal(err)
	}
	if !out[0].On.Equal(m(30)) || !out[0].Off.Equal(m(65)) {
		t.Fatalf("on=30 off=65 bekleniyordu, %v %v", out[0].On, out[0].Off)
	}
}

func TestSmallDelayAbsorbedByGroundTime(t *testing.T) {
	out, _ := Shift(fixture().Turnarounds, Trigger{FlightID: 100, NewTime: m(5)}, DefaultRecovery)
	if !out[0].Off.Equal(m(45)) {
		t.Fatalf("5 dk gecikme yerde emilmeli, off=%v", out[0].Off)
	}
	if !out[1].On.Equal(m(180)) {
		t.Fatal("zincir etkilenmemeli")
	}
}

func TestChainPropagatesToNextRotation(t *testing.T) {
	out, _ := Shift(fixture().Turnarounds, Trigger{FlightID: 100, NewTime: m(60)}, DefaultRecovery)
	// off: 60+35=95 (50 dk geç). dışarıdaki süre 135 dk, 10 dk toparlanma: sonraki on = 95+125 = 220
	if !out[1].On.Equal(m(220)) {
		t.Fatalf("sonraki rotasyon 220'de gelmeli, %v", out[1].On.Sub(base).Minutes())
	}
	if !out[1].Off.Equal(m(255)) {
		t.Fatalf("sonraki off 255 olmalı, %v", out[1].Off.Sub(base).Minutes())
	}
	if !out[2].On.Equal(m(70)) {
		t.Fatal("başka uçak etkilenmemeli")
	}
}

func TestReplanMovesOnlyWhatIsNeeded(t *testing.T) {
	tl := fixture()
	plan, err := Replan(tl, Trigger{FlightID: 100, NewTime: m(60)})
	if err != nil {
		t.Fatal(err)
	}
	// 10 numara artık [60,95)+15 → 20 numara [70,120) ile A1'de çakışır.
	if len(plan.Stands) == 0 {
		t.Fatal("stand çakışması çözülmeliydi")
	}
	for _, s := range plan.Stands {
		if s.TurnaroundID != 10 && s.TurnaroundID != 11 {
			t.Fatalf("yalnızca gecikenler taşınmalı, taşınan: %d", s.TurnaroundID)
		}
		if s.NewStand == 0 {
			t.Fatalf("yer bulunmalıydı: %+v", plan.Unallocated)
		}
	}
	if len(plan.Unallocated) != 0 {
		t.Fatalf("yerleşemeyen olmamalı: %+v", plan.Unallocated)
	}
	if plan.AssignmentChanges > 3 {
		t.Fatalf("değişiklik sayısı makul olmalı: %d", plan.AssignmentChanges)
	}
	if len(plan.Flights) != 4 {
		t.Fatalf("iki turnaroundun 4 uçuşu kaymalı: %+v", plan.Flights)
	}
}

func TestReplanShiftsTasksAndKeepsCrewWhenPossible(t *testing.T) {
	plan, _ := Replan(fixture(), Trigger{FlightID: 100, NewTime: m(20)})
	var got *TaskChange
	for i := range plan.Tasks {
		if plan.Tasks[i].TaskID == 1 {
			got = &plan.Tasks[i]
		}
	}
	if got == nil || !got.NewStart.Equal(m(30)) {
		t.Fatalf("görev 20 dk kaymalı: %+v", plan.Tasks)
	}
	if got.NewCrew != 1 {
		t.Fatalf("aynı ekip korunmalı (ekip 1, 30-50 boş), verilen %d", got.NewCrew)
	}
}

func TestReplanIsDeterministic(t *testing.T) {
	a, _ := Replan(fixture(), Trigger{FlightID: 100, NewTime: m(60)})
	b, _ := Replan(fixture(), Trigger{FlightID: 100, NewTime: m(60)})
	if a.Fingerprint != b.Fingerprint || a.Fingerprint == "" {
		t.Fatal("aynı girdi aynı planı üretmeli")
	}
	c, _ := Replan(fixture(), Trigger{FlightID: 100, NewTime: m(61)})
	if c.Fingerprint == a.Fingerprint {
		t.Fatal("farklı girdi farklı parmak izi vermeli")
	}
}

func TestLockedTurnaroundCannotBeReplanned(t *testing.T) {
	tl := fixture()
	tl.Turnarounds[0].Locked = true
	if _, err := Replan(tl, Trigger{FlightID: 100, NewTime: m(30)}); err == nil {
		t.Fatal("kilitli turnaround reddedilmeli")
	}
}

func TestUnknownFlight(t *testing.T) {
	if _, err := Replan(fixture(), Trigger{FlightID: 999, NewTime: m(30)}); err != ErrUnknownFlight {
		t.Fatalf("ErrUnknownFlight bekleniyordu: %v", err)
	}
}

func TestDepartureDelayBelowMinTurnRejected(t *testing.T) {
	if _, err := Shift(fixture().Turnarounds, Trigger{FlightID: 101, NewTime: m(20)}, DefaultRecovery); err == nil {
		t.Fatal("minimum turnaround altı kalkış reddedilmeli")
	}
}
