// api: PistKoordinat HTTP sunucusu.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // distroless imajda zoneinfo yok

	"pistkoordinat/internal/api"
	"pistkoordinat/internal/config"
	"pistkoordinat/internal/db"
	"pistkoordinat/openapi"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "kendi /health ucunu yokla ve çık (Docker HEALTHCHECK)")
	flag.Parse()
	if *healthcheck {
		os.Exit(probe("http://127.0.0.1" + addrPort(os.Getenv("HTTP_ADDR"), ":8080") + "/health"))
	}

	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("yapılandırma", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("veritabanı", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := db.WaitReady(ctx, pool, 60*time.Second); err != nil {
		slog.Error("veritabanı hazır değil", "err", err)
		os.Exit(1)
	}

	srv := api.New(cfg, pool, openapi.Spec)
	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout yok: SSE bağlantıları uzun ömürlü. İstek süreleri context ile sınırlı.
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("api dinliyor", "addr", cfg.HTTPAddr)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		slog.Info("kapanış sinyali alındı")
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("sunucu durdu", "err", err)
			os.Exit(1)
		}
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	// SSE akışları sonsuz; önce onları kapat ki Shutdown süren normal istekleri beklesin.
	srv.Hub().Close()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		slog.Warn("düzgün kapanış zaman aşımı", "err", err)
		_ = httpSrv.Close()
	}
	slog.Info("api kapandı")
}

func addrPort(addr, def string) string {
	if addr == "" {
		addr = def
	}
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return def
}

func probe(url string) int {
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
