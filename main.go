// Command server is the HTTP entrypoint for the Atari step sequencer.
//
// It serves the embedded single-page frontend and a small JSON API for
// storing sequencer patterns and songs in Postgres (Cloud SQL in prod,
// docker-compose locally). It is built to run on Cloud Run: it listens
// on $PORT, exposes /health, and shuts down cleanly on SIGTERM so
// in-flight requests are not dropped during a revision swap.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/peopleofverso/atari-step-sequencer/internal/auth"
	"github.com/peopleofverso/atari-step-sequencer/internal/handlers"
	"github.com/peopleofverso/atari-step-sequencer/internal/store"
)

// The frontend is compiled into the binary so the runtime image stays a
// single static file (no filesystem layout to ship).
//
//go:embed all:web
var webFS embed.FS

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

type config struct {
	Port        string
	DatabaseURL string
	JWTSecret   string
}

func loadConfig() config {
	return config{
		Port:        env("PORT", "8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		JWTSecret:   os.Getenv("JWT_SECRET"),
	}
}

func run() error {
	// signal.NotifyContext gives us a context cancelled on SIGINT/SIGTERM,
	// which Cloud Run sends before stopping an instance.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := loadConfig()

	// --- Database: a bounded pgx pool ---------------------------------
	// The pool is sized small on purpose: Cloud Run scales horizontally,
	// so each instance keeps only a few connections and Cloud SQL's own
	// connection limits do the fan-in. Local dev points DATABASE_URL at
	// a docker-compose Postgres on 5432.
	var st *store.Store
	if cfg.DatabaseURL != "" {
		poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
		if err != nil {
			return err
		}
		poolCfg.MaxConns = 8
		poolCfg.MinConns = 0
		poolCfg.MaxConnIdleTime = 5 * time.Minute
		poolCfg.MaxConnLifetime = 30 * time.Minute
		poolCfg.HealthCheckPeriod = time.Minute

		pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
		if err != nil {
			return err
		}
		defer pool.Close()
		st = store.New(pool)
		slog.Info("database pool ready", "max_conns", poolCfg.MaxConns)
	} else {
		slog.Warn("DATABASE_URL not set: persistence disabled")
	}

	authSvc := auth.New(cfg.JWTSecret)
	if !authSvc.Configured() {
		slog.Warn("JWT_SECRET not set: signup/login disabled")
	}
	api := handlers.New(st, authSvc)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(requestLogger)
	r.Use(middleware.Timeout(15 * time.Second))

	// Liveness/readiness probe for Cloud Run (no auth, no DB dependency).
	// Path is /health (not /healthz) because Google's frontend reserves
	// /healthz on *.run.app and returns its own 404 before the request
	// reaches the container.
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// /config tells the frontend whether cloud features are available.
	// No secrets here — purely a feature flag for the UI.
	r.Get("/config", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{
			"cloud": st != nil && authSvc.Configured(),
		})
	})

	// JSON API. Signup/login are public; everything else is behind the
	// bearer-token middleware.
	r.Route("/api", func(r chi.Router) {
		r.Post("/signup", api.Signup)
		r.Post("/login", api.Login)
		r.Group(func(r chi.Router) {
			r.Use(authSvc.Middleware)
			r.Get("/patterns", api.ListPatterns)
			r.Post("/patterns", api.CreatePattern)
			r.Get("/patterns/{id}", api.GetPattern)
			r.Put("/patterns/{id}", api.UpdatePattern)
			r.Delete("/patterns/{id}", api.DeletePattern)
			r.Get("/songs", api.ListSongs)
			r.Post("/songs", api.CreateSong)
			r.Get("/songs/{id}", api.GetSong)
			r.Put("/songs/{id}", api.UpdateSong)
			r.Delete("/songs/{id}", api.DeleteSong)
			r.Get("/instruments", api.ListInstruments)
			r.Post("/instruments", api.CreateInstrument)
			r.Get("/instruments/{id}", api.GetInstrument)
			r.Put("/instruments/{id}", api.UpdateInstrument)
			r.Delete("/instruments/{id}", api.DeleteInstrument)
		})
	})

	// Embedded single-page frontend.
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return err
	}
	r.Handle("/*", spaHandler(sub))

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Serve in the background so we can wait for the shutdown signal.
	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// spaHandler serves embedded static files and falls back to index.html for
// any unknown path, so client-side navigation never produces a 404.
func spaHandler(fsys fs.FS) http.Handler {
	files := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if f, err := fsys.Open(p); err == nil {
			_ = f.Close()
			files.ServeHTTP(w, r)
			return
		}
		http.ServeFileFS(w, r, fsys, "index.html")
	})
}

// requestLogger emits one structured log line per request.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"dur_ms", time.Since(start).Milliseconds(),
		)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
