package worker

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"s3-webx/internal/convert"
	"s3-webx/internal/state"
	"s3-webx/internal/store"
)

var _ Converter = convert.Converter{}

func TestKindOfAndVariantKey(t *testing.T) {
	withoutVideo := bucketConfig("b")
	withoutVideo.Video.Enabled = false
	tests := []struct {
		key     string
		video   bool
		want    state.Kind
		variant string
	}{
		{"foto.jpg", true, state.Image, "foto.jpg.webp"},
		{"dir/FOTO.JPEG", true, state.Image, "dir/FOTO.JPEG.webp"},
		{"anim.Gif", true, state.Image, "anim.Gif.webp"},
		{"film.mp4", true, state.Video, "film.mp4.webm"},
		{"film.MOV", true, state.Video, "film.MOV.webm"},
		{"film.mp4", false, "", ""},
		{"foto.jpg.webp", true, "", ""},
		{"film.mp4.WEBM", true, "", ""},
		{"logo.svg", true, "", ""},
		{"README", true, "", ""},
	}
	for _, tt := range tests {
		b := bucketConfig("b")
		if !tt.video {
			b = withoutVideo
		}
		kind, ok := KindOf(tt.key, b)
		if kind != tt.want || ok != (tt.want != "") {
			t.Errorf("KindOf(%q, video=%v) = %q, %v", tt.key, tt.video, kind, ok)
		}
		if ok && VariantKey(tt.key, kind) != tt.variant {
			t.Errorf("VariantKey(%q) = %q", tt.key, VariantKey(tt.key, kind))
		}
	}
}

func TestDecide(t *testing.T) {
	source := store.Object{Key: "a.jpg", Size: 1000, LastModified: t0}
	older := &store.Object{Key: "a.jpg.webp", Size: 10, LastModified: t0.Add(-time.Hour)}
	newer := &store.Object{Key: "a.jpg.webp", Size: 10, LastModified: t0.Add(time.Hour)}
	sameVersion := func(status state.Status, attempts int) *state.Entry {
		return &state.Entry{Status: status, Attempts: attempts, SourceModified: t0, SourceSize: 1000}
	}
	otherVersion := func(status state.Status, attempts int) *state.Entry {
		return &state.Entry{Status: status, Attempts: attempts, SourceModified: t0.Add(-time.Hour), SourceSize: 900}
	}
	tests := []struct {
		name    string
		variant *store.Object
		entry   *state.Entry
		want    bool
	}{
		{"variant missing", nil, nil, true},
		{"variant older than original", older, nil, true},
		{"variant newer than original", newer, nil, false},
		{"skipped for this version", nil, sameVersion(state.Skipped, 0), false},
		{"skipped for an older version", nil, otherVersion(state.Skipped, 0), true},
		{"failed below the limit", nil, sameVersion(state.Failed, 2), true},
		{"failed at the limit", nil, sameVersion(state.Failed, 3), false},
		{"failed at the limit for an older version", nil, otherVersion(state.Failed, 3), true},
		{"done but variant deleted", nil, sameVersion(state.Done, 0), true},
		{"skipped with same date but other size", nil, &state.Entry{Status: state.Skipped, SourceModified: t0, SourceSize: 1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Decide(source, tt.variant, tt.entry); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScanQueuesCandidatesAndNotifiesWorkers(t *testing.T) {
	db := openState(t)
	s := newStoreDouble().
		givenObject("new.jpg", 1000, t0).
		givenObject("fresh.jpg", 1000, t0).
		givenObject("fresh.jpg.webp", 100, t0.Add(time.Minute)).
		givenObject("stale.png", 1000, t0).
		givenObject("stale.png.webp", 100, t0.Add(-time.Minute)).
		givenObject("skipped.gif", 1000, t0).
		givenObject("clip.mp4", 5000, t0).
		givenObject("notes.txt", 10, t0)
	db.Enqueue(ctx, "content", "skipped.gif", state.Image, t0, 1000)
	claimed, _, _ := db.Claim(ctx, state.Image, []string{"content"})
	db.MarkSkipped(ctx, claimed, 1200, time.Second)
	var notified []state.Kind
	scanner := &Scanner{
		Target: Target{Config: bucketConfig("content"), Store: s},
		State:  db,
		Notify: func(k state.Kind) { notified = append(notified, k) },
		Logger: logger,
	}

	queued, err := scanner.Scan(ctx)

	if err != nil {
		t.Fatal(err)
	}
	if queued[state.Image] != 2 || queued[state.Video] != 1 {
		t.Errorf("queued: %v", queued)
	}
	slices.Sort(notified)
	if !slices.Equal(notified, []state.Kind{state.Image, state.Video}) {
		t.Errorf("notified: %v", notified)
	}
	entries, _ := db.BucketEntries(ctx, "content")
	keys := slices.Sorted(maps.Keys(entries))
	if !slices.Equal(keys, []string{"clip.mp4", "new.jpg", "skipped.gif", "stale.png"}) {
		t.Errorf("entries: %v", keys)
	}
	if entries["skipped.gif"].Status != state.Skipped {
		t.Errorf("skipped entry requeued: %+v", entries["skipped.gif"])
	}
}

func TestScanWithVideoDisabledQueuesNoVideos(t *testing.T) {
	db := openState(t)
	b := bucketConfig("content")
	b.Video.Enabled = false
	scanner := &Scanner{
		Target: Target{Config: b, Store: newStoreDouble().givenObject("clip.mp4", 5000, t0)},
		State:  db,
		Logger: logger,
	}

	queued, err := scanner.Scan(ctx)

	if err != nil || queued[state.Video] != 0 {
		t.Errorf("queued %v err %v", queued, err)
	}
}

func TestLastScanRecordsResultButNotShutdown(t *testing.T) {
	db := openState(t)
	s := newStoreDouble()
	scanner := &Scanner{
		Target: Target{Config: bucketConfig("content"), Store: s},
		State:  db,
		Logger: logger,
	}
	if _, ok := scanner.LastScan(); ok {
		t.Fatal("last scan before any scan")
	}

	scanner.scanAndLog(ctx)
	first, ok := scanner.LastScan()
	if !ok || first.Err != nil || first.At.IsZero() {
		t.Fatalf("after success: %+v %v", first, ok)
	}

	s.givenListFailure(errors.New("dial tcp: connection refused"))
	scanner.scanAndLog(ctx)
	failed, _ := scanner.LastScan()
	if failed.Err == nil || failed.At.Before(first.At) {
		t.Errorf("after failure: %+v", failed)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	scanner.scanAndLog(cancelled)
	if after, _ := scanner.LastScan(); after != failed {
		t.Errorf("shutdown recorded: %+v", after)
	}
}
