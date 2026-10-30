// Package tasksync çevrimdışı mobil cihazlardan gelen görev olaylarını birleştirir (T-4).
//
// Görev durumu, olay listesinin katlanmasıyla (fold) hesaplanır; saklanan
// "durum" yalnızca bu katlamanın önbelleğidir. Katlama kuralları sıra
// bağımsızdır (değişmeli + birleşmeli + idempotent), yani iki ekip lideri aynı
// görevi farklı zamanlarda çevrimdışı güncelleyip farklı sırayla senkronize
// etse bile sunucu aynı sonuca varır:
//
//   - durum yalnızca ileri gider: ASSIGNED < STARTED < DONE
//   - started_at = en erken STARTED zamanı
//   - finished_at = en erken FINISHED zamanı
//   - FINISHED başlangıçtan önceyse veya başlangıç hiç yoksa çatışma işaretlenir
package tasksync

import (
	"sort"
	"time"
)

type EventType string

const (
	Started  EventType = "STARTED"
	Finished EventType = "FINISHED"
)

type Event struct {
	ClientEventID string    `json:"client_event_id"`
	TaskID        int64     `json:"task_id"`
	Type          EventType `json:"type"`
	OccurredAt    time.Time `json:"occurred_at"`
	DeviceID      string    `json:"device_id"`
}

type State struct {
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Conflicts  []string   `json:"conflicts,omitempty"`
}

var rank = map[string]int{"PENDING": 0, "ASSIGNED": 1, "STARTED": 2, "DONE": 3}

// Fold başlangıç durumundan (ör. ASSIGNED) olayları katlar.
func Fold(base string, events []Event) State {
	s := State{Status: base}
	for _, e := range events {
		t := e.OccurredAt
		switch e.Type {
		case Started:
			if s.StartedAt == nil || t.Before(*s.StartedAt) {
				s.StartedAt = &t
			}
			if rank[s.Status] < rank["STARTED"] {
				s.Status = "STARTED"
			}
		case Finished:
			if s.FinishedAt == nil || t.Before(*s.FinishedAt) {
				s.FinishedAt = &t
			}
			s.Status = "DONE"
		}
	}
	if s.FinishedAt != nil {
		if s.StartedAt == nil {
			s.Conflicts = append(s.Conflicts, "FINISHED_WITHOUT_START")
		} else if s.FinishedAt.Before(*s.StartedAt) {
			s.Conflicts = append(s.Conflicts, "FINISHED_BEFORE_START")
		}
	}
	return s
}

type Result string

const (
	Applied   Result = "APPLIED"   // görünür durum değişti
	Merged    Result = "MERGED"    // olay kaydedildi ama başka bir cihaz daha önce aynı/ileri durumu bildirmişti
	Duplicate Result = "DUPLICATE" // aynı client_event_id daha önce alındı
	Rejected  Result = "REJECTED"
)

// Classify yeni olayın etkisini önceki ve sonraki durumu karşılaştırarak söyler.
func Classify(before, after State) Result {
	if before.Status != after.Status || !eqTime(before.StartedAt, after.StartedAt) || !eqTime(before.FinishedAt, after.FinishedAt) {
		return Applied
	}
	return Merged
}

func eqTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// SortCanonical olayları deterministik bir sıraya koyar (yalnızca gösterim için;
// Fold sonucu sıradan bağımsızdır).
func SortCanonical(evs []Event) {
	sort.Slice(evs, func(i, j int) bool {
		if !evs[i].OccurredAt.Equal(evs[j].OccurredAt) {
			return evs[i].OccurredAt.Before(evs[j].OccurredAt)
		}
		return evs[i].ClientEventID < evs[j].ClientEventID
	})
}

func (e Event) Validate(now time.Time) string {
	switch {
	case len(e.ClientEventID) != 36:
		return "client_event_id UUID olmalı"
	case e.Type != Started && e.Type != Finished:
		return "type STARTED ya da FINISHED olmalı"
	case e.OccurredAt.IsZero():
		return "occurred_at zorunlu"
	case e.OccurredAt.After(now.Add(5 * time.Minute)):
		return "occurred_at gelecekte"
	case e.OccurredAt.Before(now.Add(-7 * 24 * time.Hour)):
		return "occurred_at bir haftadan eski"
	}
	return ""
}
