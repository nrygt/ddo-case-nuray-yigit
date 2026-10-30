// Package sim seed ve simülatörün ortak kullandığı deterministik veri üreticisi.
// Aynı tohum + aynı gün => birebir aynı uçuş çizelgesi ve olay akışı.
package sim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"sort"
	"time"

	"pistkoordinat/internal/domain/feed"
)

type Airline struct{ Code, Name string }

type AircraftType struct {
	Code       string
	Name       string
	Size       string
	Wingspan   string
	MinTurn    int
	TargetTurn int
}

type Aircraft struct{ Tail, Airline, Type string }

var Airlines = []Airline{
	{"YK", "Yıldız Kanat"},
	{"MR", "Marmara Air"},
	{"EG", "Ege Hava"},
	{"KD", "Kapadokya Jet"},
	{"PX", "Pist Express"},
}

var Types = []AircraftType{
	{"AT72", "ATR 72-600", "C", "27.05", 25, 35},
	{"E190", "Embraer 190", "C", "28.72", 30, 40},
	{"A320", "Airbus A320neo", "C", "35.80", 35, 45},
	{"B738", "Boeing 737-800", "C", "35.79", 35, 45},
	{"A321", "Airbus A321neo", "C", "35.80", 40, 55},
	{"B752", "Boeing 757-200", "D", "38.05", 45, 60},
	{"A333", "Airbus A330-300", "E", "60.30", 60, 90},
	{"B77W", "Boeing 777-300ER", "E", "64.80", 75, 105},
}

// Fleet uçak filosu: havayolu başına tip karışımı sabittir.
func Fleet() []Aircraft {
	mix := map[string][]string{
		"YK": {"A320", "A320", "A321", "A321", "B738", "A333", "B77W", "A320", "A321", "B738", "A320"},
		"MR": {"B738", "B738", "B738", "B738", "B752", "B738", "B738", "B738", "B738"},
		"EG": {"A320", "A320", "A320", "A321", "A320", "A320", "A321", "A320"},
		"KD": {"E190", "E190", "AT72", "AT72", "E190", "AT72", "E190"},
		"PX": {"A321", "A321", "A333", "A321", "A320", "A321", "A320", "A321"},
	}
	var out []Aircraft
	for _, al := range Airlines {
		for i, t := range mix[al.Code] {
			out = append(out, Aircraft{Tail: fmt.Sprintf("TC-%s%c", al.Code, 'A'+rune(i)), Airline: al.Code, Type: t})
		}
	}
	return out
}

var airports = []string{"ESB", "ADB", "AYT", "TZX", "DIY", "VAN", "ERZ", "GZT", "FRA", "MUC", "AMS", "CDG", "LHR", "DXB", "DOH", "BAK", "TBS", "VIE", "ZRH", "BEY"}

type Flight struct {
	FlightNo  string
	Airline   string
	Direction string
	Tail      string
	Other     string
	Sched     time.Time // UTC
}

func typeOf(code string) AircraftType {
	for _, t := range Types {
		if t.Code == code {
			return t
		}
	}
	return Types[0]
}

// Schedule bir günün uçuşlarını üretir (~target uçuş). Uçakların yarısı burada
// geceler: güne kalkışla başlar, gelişle biter.
func Schedule(seed int64, day time.Time, target int) []Flight {
	rng := rand.New(rand.NewPCG(uint64(seed), uint64(day.Year()*1000+day.YearDay())))
	loc := day.Location()
	fleet := Fleet()
	used := map[string]bool{}
	var out []Flight

	num := func(al string) string {
		for {
			n := fmt.Sprintf("%s%d", al, 100+rng.IntN(8900))
			if !used[n] {
				used[n] = true
				return n
			}
		}
	}
	at := func(min int) time.Time {
		return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc).Add(time.Duration(min) * time.Minute).UTC()
	}

	for len(out) < target {
		progress := false
		for i, ac := range fleet {
			if len(out) >= target {
				break
			}
			t := typeOf(ac.Type)
			based := i%2 == 0
			// her uçak için bir sonraki boş zamanı bul
			last := 0
			for _, f := range out {
				if f.Tail == ac.Tail {
					m := int(f.Sched.Sub(at(0)).Minutes())
					if m > last {
						last = m
					}
				}
			}
			first := last == 0
			var arrMin int
			switch {
			case first && based:
				dep := 6*60 + rng.IntN(120)
				out = append(out, Flight{num(ac.Airline), ac.Airline, "DEP", ac.Tail, airports[rng.IntN(len(airports))], at(dep)})
				arrMin = dep + 90 + rng.IntN(150)
			case first:
				arrMin = 6*60 + 30 + rng.IntN(150)
			default:
				arrMin = last + 90 + rng.IntN(150)
			}
			ground := t.TargetTurn + rng.IntN(45)
			if arrMin+ground > 23*60+30 {
				continue
			}
			origin := airports[rng.IntN(len(airports))]
			out = append(out, Flight{num(ac.Airline), ac.Airline, "ARR", ac.Tail, origin, at(arrMin)})
			// based uçak son gelişte geceler; diğerleri kalkar
			if based && arrMin > 20*60 {
				progress = true
				continue
			}
			out = append(out, Flight{num(ac.Airline), ac.Airline, "DEP", ac.Tail, airports[rng.IntN(len(airports))], at(arrMin + ground)})
			progress = true
		}
		if !progress {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Sched.Equal(out[j].Sched) {
			return out[i].Sched.Before(out[j].Sched)
		}
		return out[i].FlightNo < out[j].FlightNo
	})
	return out
}

// FeedScript gün için deterministik bir uçuş bilgi beslemesi senaryosu üretir:
// tahmini saat güncellemeleri (çoğu küçük, bazıları büyük gecikme), iptaller,
// gerçekleşen saatler; %10 tekrar ve parti içinde sıra bozukluğu içerir.
func FeedScript(seed int64, day time.Time, flights []Flight) [][]feed.Event {
	rng := rand.New(rand.NewPCG(uint64(seed)+7, uint64(day.YearDay())))
	opDate := day.Format("2006-01-02")
	var events []feed.Event
	msg := 0
	for _, f := range flights {
		seq := int64(0)
		est := f.Sched
		emit := func(kind feed.Kind, t time.Time) {
			seq++
			msg++
			events = append(events, feed.Event{Source: "AODB", MessageID: fmt.Sprintf("%s-%06d", opDate, msg),
				FlightNo: f.FlightNo, OpDate: opDate, Direction: f.Direction, Seq: seq, Kind: kind, NewTime: t})
		}
		r := rng.Float64()
		switch {
		case r < 0.015:
			emit(feed.Cancel, time.Time{})
			continue
		case r < 0.12:
			est = f.Sched.Add(time.Duration(25+rng.IntN(90)) * time.Minute) // ciddi gecikme
		case r < 0.45:
			est = f.Sched.Add(time.Duration(rng.IntN(20)-5) * time.Minute)
		}
		updates := 1 + rng.IntN(3)
		for i := 1; i <= updates; i++ {
			frac := float64(i) / float64(updates)
			step := f.Sched.Add(time.Duration(float64(est.Sub(f.Sched)) * frac)).Truncate(time.Minute)
			emit(feed.Estimate, step)
		}
		emit(feed.Actual, est.Add(time.Duration(rng.IntN(5))*time.Minute).Truncate(time.Minute))
	}
	// olayları "gönderim" sırasına göre partilere böl, parti içinde karıştır, tekrar ekle
	var batches [][]feed.Event
	for i := 0; i < len(events); i += 25 {
		end := min(i+25, len(events))
		b := append([]feed.Event(nil), events[i:end]...)
		for _, e := range events[i:end] {
			if rng.IntN(10) == 0 {
				b = append(b, e)
			}
		}
		rng.Shuffle(len(b), func(a, c int) { b[a], b[c] = b[c], b[a] })
		batches = append(batches, b)
	}
	// komşu partiler arasında da bir miktar kayma: bazı olaylar bir sonraki partiye kalır
	for i := 0; i+1 < len(batches); i++ {
		if rng.IntN(3) == 0 && len(batches[i]) > 2 {
			last := batches[i][len(batches[i])-1]
			batches[i] = batches[i][:len(batches[i])-1]
			batches[i+1] = append(batches[i+1], last)
		}
	}
	return batches
}

// Fingerprint üretilen verinin özeti; determinizm testi ve doğrulama için.
func Fingerprint(v any) string {
	h := sha256.New()
	fmt.Fprintf(h, "%v", v)
	return hex.EncodeToString(h.Sum(nil))[:16]
}
