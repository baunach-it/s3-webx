package worker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"s3-webx/internal/config"
	"s3-webx/internal/state"
)

type Converter interface {
	Image(
		ctx context.Context,
		in string,
		out string,
		opts config.Image,
	) error
	Video(
		ctx context.Context,
		in string,
		out string,
		opts config.Video,
		progress func(percent float64),
	) error
}

// An empty object cannot be decoded, and retrying the same version would fail the same way.
var errEmptySource = errors.New("source object is empty (0 bytes)")

type Progress struct {
	Bucket  string
	Key     string
	Started time.Time
	Percent float64
}

type Worker struct {
	kind    state.Kind
	targets map[string]Target
	names   []string
	state   *state.DB
	convert Converter
	workDir string
	logger  *slog.Logger
	wake    chan struct{}

	mu      sync.Mutex
	current *Progress
}

// A video worker only serves buckets with video enabled, so entries left from an earlier config stay put.
func New(
	kind state.Kind,
	targets []Target,
	db *state.DB,
	convert Converter,
	workDir string,
	logger *slog.Logger,
) *Worker {
	w := &Worker{
		kind:    kind,
		targets: map[string]Target{},
		state:   db,
		convert: convert,
		workDir: workDir,
		logger:  logger,
		wake:    make(chan struct{}, 1),
	}
	for _, t := range targets {
		if kind == state.Video && !t.Config.Video.Enabled {
			continue
		}
		w.targets[t.Config.Name] = t
		w.names = append(w.names, t.Config.Name)
	}
	return w
}

func (w *Worker) Kind() state.Kind {
	return w.kind
}

func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *Worker) Current() (Progress, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.current == nil {
		return Progress{}, false
	}
	return *w.current, true
}

func (w *Worker) setCurrent(p *Progress) {
	w.mu.Lock()
	w.current = p
	w.mu.Unlock()
}

func (w *Worker) setPercent(percent float64) {
	w.mu.Lock()
	if w.current != nil {
		w.current.Percent = percent
	}
	w.mu.Unlock()
}

func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		worked, err := w.ProcessNext(ctx)
		if err != nil {
			w.logger.Error("worker failed", "kind", w.kind, "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(time.Minute):
			}
			continue
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
		case <-w.wake:
		}
	}
}

// ProcessNext converts the oldest queued entry of this worker's kind and reports whether there was one.
func (w *Worker) ProcessNext(ctx context.Context) (bool, error) {
	entry, ok, err := w.state.Claim(ctx, w.kind, w.names)
	if err != nil || !ok {
		return false, err
	}
	target := w.targets[entry.Bucket]
	started := time.Now()
	w.setCurrent(&Progress{Bucket: entry.Bucket, Key: entry.Key, Started: started})
	defer w.setCurrent(nil)

	skipped, outputSize, convErr := w.process(ctx, target, entry)
	duration := time.Since(started)
	// The state write must outlive a shutdown, otherwise the entry would stay running until restart.
	writeCtx := context.WithoutCancel(ctx)
	result := state.Done
	switch {
	case convErr != nil && ctx.Err() != nil:
		result = state.Queued
		err = w.state.Requeue(writeCtx, entry)
	case errors.Is(convErr, errEmptySource):
		result = state.Failed
		err = w.state.MarkGivenUp(writeCtx, entry, convErr, duration)
	case convErr != nil:
		result = state.Failed
		err = w.state.MarkFailed(writeCtx, entry, convErr, duration)
	case skipped:
		result = state.Skipped
		err = w.state.MarkSkipped(writeCtx, entry, outputSize, duration)
	default:
		err = w.state.MarkDone(writeCtx, entry, outputSize, duration)
	}
	attrs := []any{
		"bucket", entry.Bucket,
		"key", entry.Key,
		"kind", entry.Kind,
		"result", result,
		"bytes_before", entry.SourceSize,
		"bytes_after", outputSize,
		"duration", duration.Round(time.Millisecond).String(),
	}
	if convErr != nil {
		attrs = append(attrs, "error", convErr.Error())
	}
	w.logger.Info("file", attrs...)
	return true, err
}

func (w *Worker) process(
	ctx context.Context,
	target Target,
	entry state.Entry,
) (bool, int64, error) {
	if entry.SourceSize == 0 {
		return false, 0, errEmptySource
	}
	if err := os.MkdirAll(w.workDir, 0o755); err != nil {
		return false, 0, err
	}
	dir, err := os.MkdirTemp(w.workDir, "job-")
	if err != nil {
		return false, 0, err
	}
	defer os.RemoveAll(dir)

	// The converters pick the decoder from the extension, so the temp file keeps it.
	in := filepath.Join(dir, "source"+strings.ToLower(path.Ext(entry.Key)))
	out := filepath.Join(dir, "variant"+variantExtension[w.kind])
	if err := target.Store.Download(ctx, entry.Key, in); err != nil {
		return false, 0, err
	}
	cfg := target.Config
	switch w.kind {
	case state.Image:
		err = w.convert.Image(ctx, in, out, cfg.Image)
	case state.Video:
		err = w.convert.Video(ctx, in, out, cfg.Video, w.setPercent)
	default:
		err = errors.New("unknown kind " + string(w.kind))
	}
	if err != nil {
		return false, 0, err
	}
	info, err := os.Stat(out)
	if err != nil {
		return false, 0, err
	}
	if info.Size() >= entry.SourceSize && !cfg.KeepIfLarger {
		return true, info.Size(), nil
	}
	cacheControl := cfg.CacheControl
	if cacheControl == config.CacheControlInherit {
		if cacheControl, err = target.Store.CacheControl(ctx, entry.Key); err != nil {
			return false, info.Size(), err
		}
	}
	err = target.Store.Upload(
		ctx,
		VariantKey(entry.Key, w.kind),
		out,
		contentType[w.kind],
		cacheControl,
	)
	return false, info.Size(), err
}

// CleanWorkDir removes job folders a killed process left behind.
func CleanWorkDir(workDir string) error {
	leftovers, err := filepath.Glob(filepath.Join(workDir, "job-*"))
	if err != nil {
		return err
	}
	for _, dir := range leftovers {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return nil
}
