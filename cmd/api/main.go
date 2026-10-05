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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"example.com/subscriptions/internal/auth"
	"example.com/subscriptions/internal/config"
	"example.com/subscriptions/internal/httpapi"
	"example.com/subscriptions/internal/observability"
	"example.com/subscriptions/internal/store"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "configs/local.json", "configuration JSON")
	flag.Parse()
	c, e := config.Load(*path)
	if e != nil {
		return e
	}
	verifier, e := auth.New(c)
	if e != nil {
		return e
	}
	cursor, e := c.CursorKey()
	if e != nil {
		return e
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	s, e := store.Open(startup, c, false)
	cancel()
	if e != nil {
		return e
	}
	defer s.Close()
	startup, cancel = context.WithTimeout(ctx, 10*time.Second)
	initialHealth := s.Health(startup)
	cancel()
	initialAvailable := int32(0)
	for _, ok := range initialHealth {
		if ok {
			initialAvailable++
		}
	}
	if initialAvailable == 0 {
		return fmt.Errorf("topology is not initialized or does not match configuration")
	}
	m := observability.New()
	api := &httpapi.API{Repo: s, Auth: verifier, Cursor: httpapi.CursorCodec{Key: cursor}, Log: log, Metrics: m, MaxConcurrent: c.MaxConcurrent, RPS: c.RPS}
	var available atomic.Int32
	available.Store(initialAvailable)
	for id, ok := range initialHealth {
		value := 0.
		if ok {
			value = 1
		}
		m.ShardReady.WithLabelValues(id).Set(value)
	}
	var draining atomic.Bool
	admin := http.NewServeMux()
	admin.Handle("GET /metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{MaxRequestsInFlight: 4, Timeout: 5 * time.Second}))
	admin.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); w.Write([]byte("alive\n")) })
	admin.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if draining.Load() || available.Load() == 0 {
			http.Error(w, "not ready", 503)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte("ready\n"))
	})
	// A single shard failure should not evict every API pod. Requests to that shard
	// fail independently; metrics expose partial outages. All-frozen => not ready.
	monitorCtx, stopMonitor := context.WithCancel(context.Background())
	defer stopMonitor()
	var monitor sync.WaitGroup
	monitor.Add(1)
	go func() {
		defer monitor.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-monitorCtx.Done():
				return
			case <-ticker.C:
				var healthy int32
				for id, ok := range s.Health(monitorCtx) {
					value := 0.
					if ok {
						healthy++
						value = 1
					}
					m.ShardReady.WithLabelValues(id).Set(value)
					stats := s.Pools[id].Stat()
					m.Pool.WithLabelValues(id, "acquired").Set(float64(stats.AcquiredConns()))
					m.Pool.WithLabelValues(id, "idle").Set(float64(stats.IdleConns()))
				}
				available.Store(healthy)
			}
		}
	}()
	newServer := func(addr string, h http.Handler) *http.Server {
		return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	}
	servers := []*http.Server{newServer(c.HTTPAddr, api.Handler()), newServer(c.AdminAddr, admin)}
	failures := make(chan error, 2)
	for _, srv := range servers {
		go func() { failures <- srv.ListenAndServe() }()
	}
	log.Info("starting", "http", c.HTTPAddr, "admin", c.AdminAddr, "topology", s.Ring.Fingerprint, "epoch", s.Ring.Spec.Epoch)
	var serveError error
	select {
	case <-ctx.Done():
	case serveError = <-failures:
	}
	draining.Store(true)
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := srv.Shutdown(shutdown); e != nil {
				srv.Close()
			}
		}()
	}
	wg.Wait()
	stopMonitor()
	monitor.Wait()
	if serveError != nil && !errors.Is(serveError, http.ErrServerClosed) {
		return serveError
	}
	log.Info("stopped")
	return nil
}
