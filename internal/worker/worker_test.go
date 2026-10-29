package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"s3-webx/internal/config"
	"s3-webx/internal/state"
	"s3-webx/internal/store"
)

var (
	ctx    = context.Background()
	t0     = time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	logger = slog.New(slog.NewTextHandler(io.Discard, nil))
)

func bucketConfig(name string) config.Bucket {
	return config.Bucket{
		Name:         name,
		Interval:     time.Minute,
		Image:        config.Image{Formats: []string{"jpg", "jpeg", "png", "gif"}, MaxEdge: 1600, Quality: 80},
		Video:        config.Video{Enabled: true, Formats: []string{"mp4", "mov", "m4v"}, MaxHeight: 1080, CRF: 33, AudioBitrate: "96k"},
		CacheControl: config.CacheControlInherit,
	}
}

func openState(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func entry(t *testing.T, db *state.DB, bucket, key string) state.Entry {
	t.Helper()
	e, ok, err := db.Get(ctx, bucket, key)
	if err != nil || !ok {
		t.Fatalf("entry %s/%s: ok=%v err=%v", bucket, key, ok, err)
	}
	return e
}

type fixture struct {
	db      *state.DB
	store   *storeDouble
	convert *converterDouble
	workDir string
	bucket  config.Bucket
}

func newFixture(t *testing.T) *fixture {
	return &fixture{
		db:      openState(t),
		store:   newStoreDouble(),
		convert: newConverterDouble(),
		workDir: filepath.Join(t.TempDir(), "tmp"),
		bucket:  bucketConfig("content"),
	}
}

func (f *fixture) worker(kind state.Kind) *Worker {
	return New(kind, []Target{{Config: f.bucket, Store: f.store}}, f.db, f.convert, f.workDir, logger)
}

func (f *fixture) enqueue(t *testing.T, key string, kind state.Kind, size int64) {
	t.Helper()
	if err := f.db.Enqueue(ctx, f.bucket.Name, key, kind, t0, size); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) assertWorkDirEmpty(t *testing.T) {
	t.Helper()
	left, _ := os.ReadDir(f.workDir)
	if len(left) != 0 {
		t.Errorf("work dir not cleaned: %v", left)
	}
}

func TestImageIsConvertedAndUploadedWithInheritedCacheControl(t *testing.T) {
	f := newFixture(t)
	f.store.givenCacheControl("photos/foto.JPG", "public, max-age=3600")
	f.enqueue(t, "photos/foto.JPG", state.Image, 1000)

	worked, err := f.worker(state.Image).ProcessNext(ctx)
	if err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}

	want := upload{Key: "photos/foto.JPG.webp", ContentType: "image/webp", CacheControl: "public, max-age=3600", Size: 10}
	if len(f.store.uploads) != 1 || f.store.uploads[0] != want {
		t.Errorf("uploads: %+v", f.store.uploads)
	}
	if len(f.convert.images) != 1 || filepath.Base(f.convert.images[0].In) != "source.jpg" || f.convert.images[0].Image.MaxEdge != 1600 {
		t.Errorf("conversions: %+v", f.convert.images)
	}
	if e := entry(t, f.db, "content", "photos/foto.JPG"); e.Status != state.Done || e.OutputSize != 10 {
		t.Errorf("entry: %+v", e)
	}
	f.assertWorkDirEmpty(t)
}

func TestFixedCacheControlNeedsNoLookup(t *testing.T) {
	f := newFixture(t)
	f.bucket.CacheControl = "no-cache"
	f.enqueue(t, "a.png", state.Image, 1000)

	f.worker(state.Image).ProcessNext(ctx)

	if len(f.store.cacheLookups) != 0 || len(f.store.uploads) != 1 || f.store.uploads[0].CacheControl != "no-cache" {
		t.Errorf("lookups %v uploads %+v", f.store.cacheLookups, f.store.uploads)
	}
}

func TestLargerOutputIsSkippedUnlessKept(t *testing.T) {
	f := newFixture(t)
	f.convert.givenOutputSize(1000)
	f.enqueue(t, "a.jpg", state.Image, 1000)

	f.worker(state.Image).ProcessNext(ctx)

	if len(f.store.uploads) != 0 {
		t.Errorf("uploaded a variant that is not smaller: %+v", f.store.uploads)
	}
	if e := entry(t, f.db, "content", "a.jpg"); e.Status != state.Skipped || e.OutputSize != 1000 {
		t.Errorf("entry: %+v", e)
	}

	kept := newFixture(t)
	kept.bucket.KeepIfLarger = true
	kept.convert.givenOutputSize(2000)
	kept.enqueue(t, "a.jpg", state.Image, 1000)
	kept.worker(state.Image).ProcessNext(ctx)
	if len(kept.store.uploads) != 1 {
		t.Errorf("keep_if_larger: uploads %+v", kept.store.uploads)
	}
}

func TestConversionFailureCountsAnAttempt(t *testing.T) {
	f := newFixture(t)
	f.convert.givenFailure(errors.New("vips: bad header"))
	f.enqueue(t, "a.jpg", state.Image, 1000)

	worked, err := f.worker(state.Image).ProcessNext(ctx)

	if err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if e := entry(t, f.db, "content", "a.jpg"); e.Status != state.Failed || e.Attempts != 1 || e.Error != "vips: bad header" {
		t.Errorf("entry: %+v", e)
	}
	f.assertWorkDirEmpty(t)
}

func TestShutdownDuringConversionRequeues(t *testing.T) {
	f := newFixture(t)
	runCtx, cancel := context.WithCancel(ctx)
	f.convert.givenShutdownDuringConversion(cancel)
	f.enqueue(t, "clip.mp4", state.Video, 1000)

	f.worker(state.Video).ProcessNext(runCtx)

	if e := entry(t, f.db, "content", "clip.mp4"); e.Status != state.Queued || e.Attempts != 0 {
		t.Errorf("entry: %+v", e)
	}
	if len(f.store.uploads) != 0 {
		t.Errorf("uploads: %+v", f.store.uploads)
	}
	f.assertWorkDirEmpty(t)
}

func TestVideoProgressIsVisibleWhileConverting(t *testing.T) {
	f := newFixture(t)
	f.enqueue(t, "film.mov", state.Video, 1000)
	w := f.worker(state.Video)
	var during Progress
	var running bool
	f.convert.givenDuringConversion(func() { during, running = w.Current() })

	w.ProcessNext(ctx)

	if !running || during.Key != "film.mov" || during.Bucket != "content" || during.Percent != 50 {
		t.Errorf("progress during conversion: running=%v %+v", running, during)
	}
	if _, still := w.Current(); still {
		t.Error("progress kept after conversion")
	}
	if len(f.store.uploads) != 1 || f.store.uploads[0].Key != "film.mov.webm" || f.store.uploads[0].ContentType != "video/webm" {
		t.Errorf("uploads: %+v", f.store.uploads)
	}
}

func TestWorkersOnlyTakeTheirKind(t *testing.T) {
	f := newFixture(t)
	f.enqueue(t, "clip.mp4", state.Video, 1000)

	worked, _ := f.worker(state.Image).ProcessNext(ctx)

	if worked || len(f.convert.images)+len(f.convert.videos) != 0 {
		t.Error("image worker took a video")
	}
}

func TestVideoWorkerSkipsBucketsWithVideoDisabled(t *testing.T) {
	f := newFixture(t)
	f.bucket.Video.Enabled = false
	f.enqueue(t, "clip.mp4", state.Video, 1000)

	worked, _ := f.worker(state.Video).ProcessNext(ctx)

	if worked {
		t.Error("video worker served a bucket with video disabled")
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	f := newFixture(t)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	w := f.worker(state.Image)
	go func() {
		w.Run(runCtx)
		close(done)
	}()
	f.enqueue(t, "a.jpg", state.Image, 1000)
	w.Wake()
	deadline := time.After(5 * time.Second)
	for entry(t, f.db, "content", "a.jpg").Status != state.Done {
		select {
		case <-deadline:
			t.Fatal("woken worker did not process the entry")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestCleanWorkDirRemovesOnlyJobFolders(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "job-123", "nested"), 0o755)
	os.WriteFile(filepath.Join(dir, "keep.txt"), nil, 0o644)

	if err := CleanWorkDir(dir); err != nil {
		t.Fatal(err)
	}
	left, _ := os.ReadDir(dir)
	if len(left) != 1 || left[0].Name() != "keep.txt" {
		t.Errorf("left: %v", left)
	}
}

var _ Store = (*store.Bucket)(nil)
