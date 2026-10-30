package tasksync

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

func e(id int, typ EventType, min int, dev string) Event {
	return Event{ClientEventID: fmt.Sprintf("00000000-0000-4000-8000-%012d", id), TaskID: 1, Type: typ,
		OccurredAt: t0.Add(time.Duration(min) * time.Minute), DeviceID: dev}
}

func TestStartThenFinish(t *testing.T) {
	s := Fold("ASSIGNED", []Event{e(1, Started, 0, "a"), e(2, Finished, 20, "a")})
	if s.Status != "DONE" || !s.StartedAt.Equal(t0) || len(s.Conflicts) != 0 {
		t.Fatalf("%+v", s)
	}
}

// İki ekip lideri aynı görevi çevrimdışıyken başlatmış: en erken başlangıç kazanır.
func TestTwoDevicesStartSameTask(t *testing.T) {
	s := Fold("ASSIGNED", []Event{e(1, Started, 5, "b"), e(2, Started, 2, "a")})
	if !s.StartedAt.Equal(t0.Add(2*time.Minute)) || s.Status != "STARTED" {
		t.Fatalf("%+v", s)
	}
}

// Biri bitirdi, diğeri (eski bilgiyle) sonradan "başladı" gönderdi: durum geri gitmez.
func TestStatusNeverGoesBackwards(t *testing.T) {
	s := Fold("ASSIGNED", []Event{e(1, Started, 0, "a"), e(2, Finished, 20, "a"), e(3, Started, 25, "b")})
	if s.Status != "DONE" {
		t.Fatalf("DONE kalmalı: %+v", s)
	}
	if !s.StartedAt.Equal(t0) {
		t.Fatal("başlangıç en erken olay olmalı")
	}
}

func TestFinishedWithoutStartIsFlagged(t *testing.T) {
	s := Fold("ASSIGNED", []Event{e(1, Finished, 20, "a")})
	if s.Status != "DONE" || len(s.Conflicts) != 1 || s.Conflicts[0] != "FINISHED_WITHOUT_START" {
		t.Fatalf("%+v", s)
	}
}

func TestFinishedBeforeStartIsFlagged(t *testing.T) {
	s := Fold("ASSIGNED", []Event{e(1, Started, 30, "a"), e(2, Finished, 10, "b")})
	if len(s.Conflicts) != 1 || s.Conflicts[0] != "FINISHED_BEFORE_START" {
		t.Fatalf("%+v", s)
	}
}

func TestFoldIsOrderIndependentAndIdempotent(t *testing.T) {
	evs := []Event{e(1, Started, 3, "a"), e(2, Started, 1, "b"), e(3, Finished, 30, "a"), e(4, Finished, 28, "b")}
	want := Fold("ASSIGNED", evs)
	rng := rand.New(rand.NewPCG(9, 9))
	for i := 0; i < 300; i++ {
		cp := append([]Event(nil), evs...)
		cp = append(cp, cp[rng.IntN(len(cp))]) // tekrar
		rng.Shuffle(len(cp), func(a, b int) { cp[a], cp[b] = cp[b], cp[a] })
		if got := Fold("ASSIGNED", cp); !reflect.DeepEqual(got, want) {
			t.Fatalf("sıra bağımlı sonuç: %+v != %+v", got, want)
		}
	}
}

func TestClassify(t *testing.T) {
	before := Fold("ASSIGNED", []Event{e(1, Started, 0, "a")})
	after := Fold("ASSIGNED", []Event{e(1, Started, 0, "a"), e(2, Started, 5, "b")})
	if Classify(before, after) != Merged {
		t.Fatal("daha geç başlangıç durumu değiştirmez: MERGED")
	}
	after2 := Fold("ASSIGNED", []Event{e(1, Started, 0, "a"), e(2, Finished, 5, "b")})
	if Classify(before, after2) != Applied {
		t.Fatal("bitiş durumu değiştirir: APPLIED")
	}
}

func TestValidateEvent(t *testing.T) {
	now := t0.Add(time.Hour)
	if msg := e(1, Started, 0, "a").Validate(now); msg != "" {
		t.Fatal(msg)
	}
	future := e(1, Started, 120, "a")
	if future.Validate(now) == "" {
		t.Fatal("gelecekteki olay reddedilmeli")
	}
	bad := e(1, "PAUSED", 0, "a")
	if bad.Validate(now) == "" {
		t.Fatal("bilinmeyen tür reddedilmeli")
	}
	short := e(1, Started, 0, "a")
	short.ClientEventID = "abc"
	if short.Validate(now) == "" {
		t.Fatal("uuid olmayan kimlik reddedilmeli")
	}
}

func TestSortCanonical(t *testing.T) {
	evs := []Event{e(2, Started, 5, "a"), e(1, Started, 5, "a"), e(3, Started, 1, "a")}
	SortCanonical(evs)
	if evs[0].OccurredAt != t0.Add(time.Minute) || evs[1].ClientEventID > evs[2].ClientEventID {
		t.Fatalf("%+v", evs)
	}
}
