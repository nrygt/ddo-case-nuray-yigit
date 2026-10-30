// web: arayüz dosyalarını sunar ve /api, /health isteklerini backend'e aktarır.
// Aynı kökten servis edildiği için tarayıcıda CORS gerekmez; SSE akışı
// FlushInterval=-1 ile tamponlanmadan geçer.
package main

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

//go:embed public
var public embed.FS

func main() {
	addr := env("WEB_ADDR", ":3000")
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		c := http.Client{Timeout: 3 * time.Second}
		resp, err := c.Get("http://127.0.0.1" + addr + "/")
		if err != nil || resp.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	target, err := url.Parse(env("API_URL", "http://api:8080"))
	if err != nil {
		slog.Error("API_URL", "err", err)
		os.Exit(1)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":{"code":"api_unreachable","message":"Sunucuya ulaşılamıyor"}}`))
	}

	root, _ := fs.Sub(public, "public")
	files := http.FileServer(http.FS(root))
	mux := http.NewServeMux()
	mux.Handle("/api/", proxy)
	mux.Handle("/health", proxy)
	mux.Handle("/openapi.yaml", proxy)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// tek sayfa uygulama: bilinmeyen yollar index.html'e düşer
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" || strings.HasSuffix(p, "/") {
			p = "index.html"
		}
		if _, err := fs.Stat(root, p); err != nil {
			r.URL.Path = "/"
		}
		if strings.HasSuffix(r.URL.Path, ".js") || r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, ".html") {
			w.Header().Set("Cache-Control", "no-cache")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		files.ServeHTTP(w, r)
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		slog.Info("web dinliyor", "addr", addr, "api", target.String())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("web", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
