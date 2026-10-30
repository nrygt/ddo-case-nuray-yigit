package sim

import (
	"testing"
	"time"

	"pistkoordinat/internal/domain/feed"
)

var ist, _ = time.LoadLocation("Europe/Istanbul")
var day = time.Date(2026, 10, 20, 0, 0, 0, 0, ist)

func TestScheduleIsDeterministic(t *testing.T) {
	a := Schedule(42, day, 300)
	b := Schedule(42, day, 300)
	if Fingerprint(a) != Fingerprint(b) {
		t.Fatal("aynı tohum farklı çizelge üretti")
	}
	if Fingerprint(Schedule(43, day, 300)) == Fingerprint(a) {
		t.Fatal("farklı tohum aynı çizelgeyi üretmemeli")
	}
}

func TestScheduleShape(t *testing.T) {
	fl := Schedule(42, day, 300)
	if len(fl) < 280 || len(fl) > 300 {
		t.Fatalf("~300 uçuş bekleniyordu, %d", len(fl))
	}
	nos := map[string]bool{}
	for _, f := range fl {
		key := f.FlightNo + f.Direction
		if nos[key] {
			t.Fatalf("tekrarlanan uçuş no %s", key)
		}
		nos[key] = true
		local := f.Sched.In(ist)
		if local.Day() != 20 {
			t.Fatalf("uçuş gün dışında: %v", local)
		}
	}
	// her uçağın uçuşları geliş/gidiş sırasıyla tutarlı olmalı: iki ARR art arda gelmez
	last := map[string]string{}
	for _, f := range fl {
		if last[f.Tail] == "ARR" && f.Direction == "ARR" {
			t.Fatalf("%s art arda iki geliş", f.Tail)
		}
		last[f.Tail] = f.Direction
	}
}

func TestFeedScriptHasDuplicatesAndDisorder(t *testing.T) {
	fl := Schedule(42, day, 300)
	batches := FeedScript(42, day, fl)
	seen := map[string]int{}
	disorder := 0
	lastSeq := map[string]int64{}
	for _, b := range batches {
		for _, e := range b {
			seen[e.MessageID]++
			k := e.FlightNo + e.Direction
			if e.Seq < lastSeq[k] {
				disorder++
			}
			if e.Seq > lastSeq[k] {
				lastSeq[k] = e.Seq
			}
			if msg := e.Valid(); msg != "" {
				t.Fatalf("geçersiz olay: %s", msg)
			}
		}
	}
	dups := 0
	for _, n := range seen {
		if n > 1 {
			dups++
		}
	}
	if dups == 0 || disorder == 0 {
		t.Fatalf("tekrar (%d) ve sıra bozukluğu (%d) olmalı", dups, disorder)
	}
	if Fingerprint(FeedScript(42, day, fl)) != Fingerprint(batches) {
		t.Fatal("besleme senaryosu deterministik değil")
	}
}

// Senaryodaki olaylar hangi sırayla uygulanırsa uygulansın son durum aynı olmalı.
func TestFeedScriptConvergesRegardlessOfOrder(t *testing.T) {
	fl := Schedule(1, day, 120)
	batches := FeedScript(1, day, fl)
	inOrder := map[string]feed.FlightState{}
	shuffled := map[string]feed.FlightState{}
	var all []feed.Event
	for _, b := range batches {
		all = append(all, b...)
	}
	for _, e := range all {
		k := e.FlightNo + e.Direction
		inOrder[k], _ = feed.Apply(inOrder[k], e)
	}
	for i := len(all) - 1; i >= 0; i-- {
		e := all[i]
		k := e.FlightNo + e.Direction
		shuffled[k], _ = feed.Apply(shuffled[k], e)
	}
	for k, v := range inOrder {
		if shuffled[k] != v {
			t.Fatalf("%s farklı son durum", k)
		}
	}
}
