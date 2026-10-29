package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"s3-webx/internal/config"
	"s3-webx/internal/convert"
	"s3-webx/internal/state"
	"s3-webx/internal/store"
	"s3-webx/internal/web"
	"s3-webx/internal/worker"
)

// Set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	if err := run(logger); err != nil {
		logger.Error("s3-webx stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	configPath := flag.String("config", "/config/s3-webx.yaml", "path to the YAML configuration")
	flag.Parse()
	logger.Info("s3-webx starting", "version", version)

	cfg, err := config.Load(*configPath, os.LookupEnv)
	if err != nil {
		return err
	}
	logger.Info("config loaded", "path", *configPath, "buckets", len(cfg.Buckets))

	if err := os.MkdirAll(filepath.Dir(cfg.State), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	if err := os.MkdirAll(cfg.WorkDir(), 0o755); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	if err := worker.CleanWorkDir(cfg.WorkDir()); err != nil {
		return fmt.Errorf("clean work dir: %w", err)
	}
	db, err := state.Open(cfg.State)
	if err != nil {
		return err
	}
	defer db.Close()
	logger.Info("state opened", "path", cfg.State, "work_dir", cfg.WorkDir())

	var targets []worker.Target
	for _, b := range cfg.Buckets {
		s, err := store.New(
			b.Endpoint,
			b.Region,
			b.Bucket,
			b.Prefix,
			b.PathStyle,
			b.AccessKey,
			b.SecretKey,
		)
		if err != nil {
			return fmt.Errorf("bucket %q: %w", b.Name, err)
		}
		targets = append(targets, worker.Target{Config: b, Store: s})
		logger.Info(
			"bucket configured",
			"bucket", b.Name,
			"endpoint", b.Endpoint,
			"prefix", b.Prefix,
			"interval", b.Interval.String(),
			"video", b.Video.Enabled,
		)
	}

	converter := convert.Converter{}
	workers := map[state.Kind]*worker.Worker{
		state.Image: worker.New(state.Image, targets, db, converter, cfg.WorkDir(), logger),
		state.Video: worker.New(state.Video, targets, db, converter, cfg.WorkDir(), logger),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	scanners := map[string]*worker.Scanner{}
	var names []string
	for _, t := range targets {
		scanners[t.Config.Name] = &worker.Scanner{
			Target: t,
			State:  db,
			Notify: func(kind state.Kind) { workers[kind].Wake() },
			Logger: logger,
		}
		names = append(names, t.Config.Name)
	}

	// Binding before the workers start lets a taken port stop the service at once.
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	server := &http.Server{
		Handler: web.New(
			db,
			names,
			activity{workers: workers, scanners: scanners},
			cfg.Auth,
			logger,
		),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			stop()
		}
	})
	for _, w := range workers {
		wg.Go(func() { w.Run(ctx) })
	}
	for _, s := range scanners {
		wg.Go(func() { s.Run(ctx) })
	}
	logger.Info("s3-webx running", "listen", listener.Addr().String())

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown", "error", err)
	}
	wg.Wait()
	logger.Info("s3-webx stopped")
	return nil
}

type activity struct {
	workers  map[state.Kind]*worker.Worker
	scanners map[string]*worker.Scanner
}

func (a activity) Current(kind state.Kind) (worker.Progress, bool) {
	if w, ok := a.workers[kind]; ok {
		return w.Current()
	}
	return worker.Progress{}, false
}

func (a activity) LastScan(bucket string) (worker.ScanResult, bool) {
	if s, ok := a.scanners[bucket]; ok {
		return s.LastScan()
	}
	return worker.ScanResult{}, false
}
