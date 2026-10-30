// Package seed demo verisini yükler. Idempotent: veri varsa dokunmaz; Reset ile
// her şey silinip ilk seed haline döner.
package seed

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pistkoordinat/internal/auth"
	"pistkoordinat/internal/sim"
	"pistkoordinat/internal/store"
)

const DemoPassword = "Demo.2026!"

type DemoUser struct {
	Email, Name string
	Role        auth.Role
	Airline     string
}

var DemoUsers = []DemoUser{
	{"ops@pist.test", "Selin Aydın", auth.OpsController, ""},
	{"sef@pist.test", "Murat Kaya", auth.GroundChief, ""},
	{"ekip@pist.test", "Emre Demir", auth.CrewLead, ""},
	{"havayolu@pist.test", "Deniz Yıldız", auth.AirlineRep, "YK"},
	{"yonetim@pist.test", "Ayşe Korkmaz", auth.Management, ""},
	{"ekip2@pist.test", "Can Öztürk", auth.CrewLead, ""},
	{"ekip3@pist.test", "Zeynep Arslan", auth.CrewLead, ""},
}

type crewDef struct {
	name, typ string
	lead      string
	shift     string         // "early" 05-13, "late" 13-21, "night" 21-05, "badrest"
	certs     map[string]int // belge -> bugünden itibaren geçerlilik günü (negatif: dolmuş)
}

// crews: her tür için erken/geç/gece vardiyalı ekipler. Bilerek iki istisna var:
// Yakıt-10'un sertifikası dün doldu, Bagaj-14'ün bugünkü vardiyası 11 saat dinlenme kuralını ihlal ediyor.
var crews = buildCrews()

func buildCrews() []crewDef {
	var out []crewDef
	add := func(prefix, typ string, shifts []string, certs map[string]int) {
		for i, sh := range shifts {
			out = append(out, crewDef{name: fmt.Sprintf("%s-%d", prefix, i+1), typ: typ, shift: sh, certs: certs})
		}
	}
	add("Yakıt", "FUEL", []string{"early", "early", "early", "early", "late", "late", "late", "late", "night", "early"},
		map[string]int{"FUEL_HANDLING": 200, "RAMP_SAFETY": 300})
	add("Bagaj", "BAGGAGE", []string{"early", "early", "early", "early", "early", "early", "late", "late", "late", "late", "late", "late", "night", "badrest"},
		map[string]int{"RAMP_SAFETY": 300})
	add("Temizlik", "CLEANING", []string{"early", "early", "early", "early", "early", "early", "late", "late", "late", "late", "late", "night"}, nil)
	add("Köprü", "BRIDGE", []string{"early", "early", "late", "late", "night"}, map[string]int{"BRIDGE_OPERATION": 100})
	for i := range out {
		switch out[i].name {
		case "Yakıt-10":
			out[i].certs = map[string]int{"FUEL_HANDLING": -1, "RAMP_SAFETY": 300}
		case "Yakıt-1":
			out[i].lead = "ekip@pist.test"
		case "Bagaj-1":
			out[i].lead = "ekip2@pist.test"
		case "Temizlik-1":
			out[i].lead = "ekip3@pist.test"
		}
	}
	return out
}

type standDef struct {
	code   string
	size   string
	bridge bool
	gate   string
}

var stands = []standDef{
	{"A1", "C", true, "A1"}, {"A2", "C", true, "A2"}, {"A3", "C", true, "A3"}, {"A4", "D", true, "A4"},
	{"A5", "E", true, "A5"}, {"A6", "E", true, "A6"}, {"B1", "C", true, "B1"}, {"B2", "C", true, "B2"},
	{"B3", "C", true, "B3"}, {"B4", "C", true, "B4"},
	{"R1", "C", false, "BUS1"}, {"R2", "C", false, "BUS1"}, {"R3", "C", false, "BUS1"}, {"R4", "C", false, "BUS2"},
	{"R5", "C", false, "BUS2"}, {"R6", "C", false, "BUS2"}, {"R7", "C", false, "BUS3"}, {"R8", "D", false, "BUS3"},
	{"R9", "E", false, "BUS3"}, {"R10", "C", false, "BUS3"}, {"R11", "C", false, "BUS4"}, {"R12", "C", false, "BUS4"},
	{"R13", "C", false, "BUS4"}, {"R14", "D", false, "BUS4"}, {"R15", "C", false, "BUS4"}, {"R16", "C", false, "BUS4"},
}

// IsSeeded referans veri yüklü mü?
func IsSeeded(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n > 0, err
}

func Run(ctx context.Context, pool *pgxpool.Pool, seedValue int64, day time.Time) error {
	ok, err := IsSeeded(ctx, pool)
	if err != nil {
		return err
	}
	if ok {
		slog.Info("seed zaten yüklü, atlanıyor")
		return nil
	}
	return load(ctx, pool, seedValue, day)
}

// Reset tüm operasyon verisini siler ve seed'i baştan yükler.
func Reset(ctx context.Context, pool *pgxpool.Pool, seedValue int64, day time.Time) error {
	_, err := pool.Exec(ctx, `TRUNCATE task_events, tasks, crew_shifts, crew_certifications, crews, equipment,
		stand_allocations, allocation_runs, feed_events, turnarounds, flights, aircraft, aircraft_types, airlines,
		stands, gates, refresh_tokens, idempotency_keys, audit_log, users, analytics_refreshes RESTART IDENTITY CASCADE`)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `REFRESH MATERIALIZED VIEW mv_turnaround_durations`); err != nil {
		return err
	}
	return load(ctx, pool, seedValue, day)
}

func load(ctx context.Context, pool *pgxpool.Pool, seedValue int64, day time.Time) error {
	start := time.Now()
	hash, err := auth.HashPassword(DemoPassword)
	if err != nil {
		return err
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, a := range sim.Airlines {
			if _, err := tx.Exec(ctx, `INSERT INTO airlines(code, name) VALUES ($1,$2)`, a.Code, a.Name); err != nil {
				return err
			}
		}
		for _, t := range sim.Types {
			if _, err := tx.Exec(ctx, `INSERT INTO aircraft_types(code, name, size_class, wingspan_m, min_turnaround_min, target_turnaround_min)
				VALUES ($1,$2,$3,$4::numeric,$5,$6)`, t.Code, t.Name, t.Size, t.Wingspan, t.MinTurn, t.TargetTurn); err != nil {
				return err
			}
		}
		for _, a := range sim.Fleet() {
			if _, err := tx.Exec(ctx, `INSERT INTO aircraft(tail_number, airline_code, type_code) VALUES ($1,$2,$3)`, a.Tail, a.Airline, a.Type); err != nil {
				return err
			}
		}
		gates := map[string]int{}
		for _, s := range stands {
			if _, ok := gates[s.gate]; ok {
				continue
			}
			var id int
			term := "İç Hatlar"
			if s.gate[0] == 'B' && len(s.gate) == 2 {
				term = "Dış Hatlar"
			}
			if err := tx.QueryRow(ctx, `INSERT INTO gates(code, terminal, is_bus) VALUES ($1,$2,$3) RETURNING id`,
				s.gate, term, !s.bridge).Scan(&id); err != nil {
				return err
			}
			gates[s.gate] = id
		}
		for _, s := range stands {
			if _, err := tx.Exec(ctx, `INSERT INTO stands(code, max_size_class, has_bridge, gate_id) VALUES ($1,$2,$3,$4)`,
				s.code, s.size, s.bridge, gates[s.gate]); err != nil {
				return err
			}
		}
		users := map[string]int64{}
		for _, u := range DemoUsers {
			var id int64
			var al *string
			if u.Airline != "" {
				al = &u.Airline
			}
			if err := tx.QueryRow(ctx, `INSERT INTO users(email, full_name, password_hash, role, airline_code)
				VALUES ($1,$2,$3,$4,$5) RETURNING id`, u.Email, u.Name, hash, string(u.Role), al).Scan(&id); err != nil {
				return err
			}
			users[u.Email] = id
		}
		for i := 1; i <= 4; i++ {
			if _, err := tx.Exec(ctx, `INSERT INTO equipment(code, kind) VALUES ($1,'FUEL_TRUCK')`, fmt.Sprintf("YT-%02d", i)); err != nil {
				return err
			}
		}
		for i := 1; i <= 6; i++ {
			if _, err := tx.Exec(ctx, `INSERT INTO equipment(code, kind) VALUES ($1,'BELT_LOADER')`, fmt.Sprintf("BK-%02d", i)); err != nil {
				return err
			}
		}
		for _, c := range crews {
			var lead *int64
			if c.lead != "" {
				id := users[c.lead]
				lead = &id
			}
			var id int
			if err := tx.QueryRow(ctx, `INSERT INTO crews(name, task_type, lead_user_id) VALUES ($1,$2,$3) RETURNING id`,
				c.name, c.typ, lead).Scan(&id); err != nil {
				return err
			}
			for cert, days := range c.certs {
				if _, err := tx.Exec(ctx, `INSERT INTO crew_certifications(crew_id, cert_type, valid_until) VALUES ($1,$2,$3)`,
					id, cert, day.AddDate(0, 0, days).Format("2006-01-02")); err != nil {
					return err
				}
			}
			for _, w := range shiftsFor(c.shift, day) {
				if _, err := tx.Exec(ctx, `INSERT INTO crew_shifts(crew_id, period) VALUES ($1, tstzrange($2,$3))`, id, w[0], w[1]); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("referans veri: %w", err)
	}
	if err := LoadDay(ctx, pool, seedValue, day); err != nil {
		return err
	}
	// Raporlar ilk açılışta boş görünmesin diye ~1 yıllık küçük bir geçmiş (20 bin turnaround).
	// T-5 doğrulaması bunu 5 milyon olaya tamamlar.
	if _, err := pool.Exec(ctx, `SELECT analytics_generate(240000, $1)`, float64(seedValue%1000)/1000); err != nil {
		return fmt.Errorf("analitik geçmiş: %w", err)
	}
	if _, err := pool.Exec(ctx, `REFRESH MATERIALIZED VIEW mv_turnaround_durations; ANALYZE mv_turnaround_durations`); err != nil {
		return err
	}
	slog.Info("seed yüklendi", "op_date", day.Format("2006-01-02"), "seed", seedValue, "duration_ms", time.Since(start).Milliseconds())
	return nil
}

// LoadDay günün uçuşlarını üretir, turnaroundları kurar, stand ve ekip atar.
func LoadDay(ctx context.Context, pool *pgxpool.Pool, seedValue int64, day time.Time) error {
	opDate := day.Format("2006-01-02")
	st := store.New(pool)
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, f := range sim.Schedule(seedValue, day, 300) {
			if _, err := st.CreateFlight(ctx, tx, store.NewFlight{FlightNo: f.FlightNo, Direction: f.Direction,
				TailNumber: f.Tail, OtherAirport: f.Other, SchedTime: f.Sched}, opDate); err != nil {
				return fmt.Errorf("uçuş %s: %w", f.FlightNo, err)
			}
		}
		_, err := st.BuildTurnarounds(ctx, tx, opDate)
		return err
	})
	if err != nil {
		return err
	}
	if _, err := st.RunAllocation(ctx, opDate, 0); err != nil {
		return fmt.Errorf("tahsis: %w", err)
	}
	if _, err := st.RunAssignment(ctx, opDate, 0); err != nil {
		return fmt.Errorf("ekip atama: %w", err)
	}
	return nil
}

// shiftsFor dün, bugün ve yarın için vardiya aralıkları (yerel saat).
func shiftsFor(kind string, day time.Time) [][2]time.Time {
	at := func(d, h int) time.Time {
		return time.Date(day.Year(), day.Month(), day.Day()+d, h, 0, 0, 0, day.Location())
	}
	var out [][2]time.Time
	for d := -1; d <= 1; d++ {
		switch kind {
		case "early":
			out = append(out, [2]time.Time{at(d, 5), at(d, 13)})
		case "late":
			out = append(out, [2]time.Time{at(d, 13), at(d, 21)})
		case "night":
			out = append(out, [2]time.Time{at(d, 21), at(d+1, 5)})
		case "badrest":
			// dün geç vardiya, bugün erken: 8 saat dinlenme -> bugünkü vardiya kullanılamaz
			if d == -1 {
				out = append(out, [2]time.Time{at(d, 13), at(d, 21)})
			} else {
				out = append(out, [2]time.Time{at(d, 5), at(d, 13)})
			}
		}
	}
	return out
}
