// Package feed uçuş bilgi beslemesi olaylarının sırasız/tekrarlı gelmesine karşı
// kuralları içerir. Her olay uçuş için artan bir sıra numarası (seq) taşır ve
// uçuşun o anki durumunun tamamını söyler (fark değil). Böylece "en büyük seq kazanır"
// kuralı, olaylar hangi sırayla gelirse gelsin aynı son durumu verir.
package feed

import "time"

type Kind string

const (
	Estimate Kind = "ESTIMATE"
	Cancel   Kind = "CANCEL"
	Actual   Kind = "ACTUAL"
)

type Event struct {
	Source    string    `json:"source"`
	MessageID string    `json:"message_id"`
	FlightNo  string    `json:"flight_no"`
	OpDate    string    `json:"op_date"`
	Direction string    `json:"direction"`
	Seq       int64     `json:"seq"`
	Kind      Kind      `json:"kind"`
	NewTime   time.Time `json:"new_time,omitempty"`
}

type Outcome string

const (
	Applied       Outcome = "APPLIED"
	Stale         Outcome = "STALE"
	Duplicate     Outcome = "DUPLICATE"
	UnknownFlight Outcome = "UNKNOWN_FLIGHT"
)

type FlightState struct {
	Seq       int64
	Status    string
	EstTime   time.Time
	Actual    time.Time
	Scheduled time.Time
}

// Apply olayı uçuş durumuna uygular. Seq geride kalan olay hiçbir şeyi değiştirmez.
func Apply(s FlightState, e Event) (FlightState, Outcome) {
	if e.Seq <= s.Seq {
		return s, Stale
	}
	s.Seq = e.Seq
	switch e.Kind {
	case Cancel:
		s.Status = "CANCELLED"
	case Actual:
		s.Actual = e.NewTime
		s.EstTime = e.NewTime
		if e.Direction == "ARR" {
			s.Status = "LANDED"
		} else {
			s.Status = "DEPARTED"
		}
	case Estimate:
		s.EstTime = e.NewTime
		s.Status = StatusFor(s.Scheduled, e.NewTime)
	}
	return s, Applied
}

// StatusFor tahmini saat planlanandan 5 dk'dan fazla sonraysa DELAYED.
func StatusFor(sched, est time.Time) string {
	if est.Sub(sched) > 5*time.Minute {
		return "DELAYED"
	}
	return "SCHEDULED"
}

func (e Event) Valid() string {
	switch {
	case e.Source == "" || e.MessageID == "":
		return "source ve message_id zorunlu"
	case e.Seq <= 0:
		return "seq pozitif olmalı"
	case e.Direction != "ARR" && e.Direction != "DEP":
		return "direction ARR ya da DEP olmalı"
	case e.Kind != Estimate && e.Kind != Cancel && e.Kind != Actual:
		return "kind ESTIMATE, CANCEL ya da ACTUAL olmalı"
	case e.Kind != Cancel && e.NewTime.IsZero():
		return "new_time zorunlu"
	}
	if _, err := time.Parse("2006-01-02", e.OpDate); err != nil {
		return "op_date YYYY-MM-DD olmalı"
	}
	return ""
}
