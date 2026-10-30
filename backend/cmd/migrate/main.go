// migrate: şemayı uygular ve seed'i yükler. Compose'da api'den önce bir kez çalışır.
//
//	migrate            migrasyon + seed (seed varsa atlanır)
//	migrate -reset     tüm veriyi silip ilk seed haline döndürür (make reset-demo)
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"time"

	"pistkoordinat/internal/config"
	"pistkoordinat/internal/db"
	"pistkoordinat/internal/seed"
	"pistkoordinat/migrations"
)

func main() {
	reset := flag.Bool("reset", false, "veriyi sıfırla ve seed'i yeniden yükle")
	noSeed := flag.Bool("no-seed", false, "yalnızca migrasyon")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := config.Load()
	if err != nil {
		fail("yapılandırma", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		fail("bağlantı", err)
	}
	defer pool.Close()
	if err := db.WaitReady(ctx, pool, 90*time.Second); err != nil {
		fail("veritabanı hazır değil", err)
	}
	n, err := db.Migrate(ctx, pool, migrations.FS)
	if err != nil {
		fail("migrasyon", err)
	}
	slog.Info("migrasyon tamam", "applied", n)
	if *noSeed {
		return
	}
	day, err := cfg.OpDate()
	if err != nil {
		fail("SEED_DATE", err)
	}
	if *reset {
		err = seed.Reset(ctx, pool, cfg.SeedValue, day)
	} else {
		err = seed.Run(ctx, pool, cfg.SeedValue, day)
	}
	if err != nil {
		fail("seed", err)
	}
}

func fail(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}
