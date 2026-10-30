package feed

import (
	"math/rand/v2"
	"testing"
	"time"
)

var sched = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

func ev(seq int64, kind Kind, min int) Event {
	return Event{Source: "AODB", MessageID: "m", FlightNo: "PK101", OpDate: "2026-10-20", Direction: "ARR",
		Seq: seq, Kind: kind, NewTime: sched.Add(time.Duration(min) * time.Minute)}
}

func TestHigherSeqWins(t *testing.T) {
	s := FlightState{Scheduled: sched, EstTime: sched}
	s, o := Apply(s, ev(2, Estimate, 30))
	if o != Applied || s.Status != "DELAYED" {
		t.Fatalf("%v %v", o, s)
	}
	s, o = Apply(s, ev(1, Estimate, 0)) // geç gelen eski mesaj
	if o != Stale || !s.EstTime.Equal(sched.Add(30*time.Minute)) {
		t.Fatalf("eski mesaj durumu değiştirmemeli: %v %v", o, s.EstTime)
	}
}

func TestSameSeqIsStale(t *testing.T) {
	s, _ := Apply(FlightState{Scheduled: sched}, ev(3, Estimate, 10))
	if _, o := Apply(s, ev(3, Estimate, 10)); o != Stale {
		t.Fatal("aynı seq ikinci kez uygulanmamalı")
	}
}

func TestCancelAndActual(t *testing.T) {
	s, _ := Apply(FlightState{Scheduled: sched}, ev(1, Cancel, 0))
	if s.Status != "CANCELLED" {
		t.Fatal("iptal")
	}
	s, _ = Apply(FlightState{Scheduled: sched}, ev(1, Actual, 4))
	if s.Status != "LANDED" || !s.Actual.Equal(sched.Add(4*time.Minute)) {
		t.Fatalf("gerçekleşen iniş: %+v", s)
	}
	d := ev(1, Actual, 4)
	d.Direction = "DEP"
	if s, _ = Apply(FlightState{Scheduled: sched}, d); s.Status != "DEPARTED" {
		t.Fatal("kalkış durumu")
	}
}

func TestSmallDelayIsNotDelayed(t *testing.T) {
	if StatusFor(sched, sched.Add(5*time.Minute)) != "SCHEDULED" || StatusFor(sched, sched.Add(6*time.Minute)) != "DELAYED" {
		t.Fatal("5 dk eşiği")
	}
}

// Aynı olay kümesinin her sıralaması (tekrarlar dahil) aynı son durumu vermeli.
func TestOrderIndependence(t *testing.T) {
	events := []Event{ev(1, Estimate, 10), ev(2, Estimate, 25), ev(3, Estimate, 40), ev(4, Estimate, 35), ev(5, Actual, 37)}
	want := FlightState{Scheduled: sched}
	for _, e := range events {
		want, _ = Apply(want, e)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 500; i++ {
		var stream []Event
		for _, e := range events {
			stream = append(stream, e)
			if rng.IntN(3) == 0 {
				stream = append(stream, e) // tekrar
			}
		}
		rng.Shuffle(len(stream), func(a, b int) { stream[a], stream[b] = stream[b], stream[a] })
		got := FlightState{Scheduled: sched}
		for _, e := range stream {
			got, _ = Apply(got, e)
		}
		if got != want {
			t.Fatalf("sıra %d farklı sonuç: %+v != %+v", i, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	good := ev(1, Estimate, 0)
	if msg := good.Valid(); msg != "" {
		t.Fatal(msg)
	}
	cases := map[string]func(*Event){
		"seq":       func(e *Event) { e.Seq = 0 },
		"direction": func(e *Event) { e.Direction = "XX" },
		"kind":      func(e *Event) { e.Kind = "FOO" },
		"time":      func(e *Event) { e.NewTime = time.Time{} },
		"date":      func(e *Event) { e.OpDate = "20-10-2026" },
		"source":    func(e *Event) { e.Source = "" },
	}
	for name, mut := range cases {
		e := good
		mut(&e)
		if e.Valid() == "" {
			t.Errorf("%s hatası yakalanmadı", name)
		}
	}
	c := good
	c.Kind, c.NewTime = Cancel, time.Time{}
	if c.Valid() != "" {
		t.Error("iptal için new_time gerekmez")
	}
}
