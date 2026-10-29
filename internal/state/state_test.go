package state

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clock := t0
	db.now = func() time.Time {
		clock = clock.Add(time.Second)
		return clock
	}
	return db
}

func mustGet(t *testing.T, db *DB, bucket, key string) Entry {
	t.Helper()
	e, ok, err := db.Get(ctx, bucket, key)
	if err != nil || !ok {
		t.Fatalf("get %s/%s: ok=%v err=%v", bucket, key, ok, err)
	}
	return e
}

func mustClaim(t *testing.T, db *DB, kind Kind, buckets ...string) Entry {
	t.Helper()
	e, ok, err := db.Claim(ctx, kind, buckets)
	if err != nil || !ok {
		t.Fatalf("claim %s: ok=%v err=%v", kind, ok, err)
	}
	return e
}

func TestClaimTakesOldestOfKindAcrossBuckets(t *testing.T) {
	db := openTemp(t)
	db.Enqueue(ctx, "b", "late.jpg", Image, t0, 10)
	db.Enqueue(ctx, "a", "clip.mp4", Video, t0, 10)
	db.Enqueue(ctx, "c", "other.jpg", Image, t0, 10)
	if err := db.Enqueue(ctx, "a", "later.jpg", Image, t0, 10); err != nil {
		t.Fatal(err)
	}

	first := mustClaim(t, db, Image, "a", "b")
	second := mustClaim(t, db, Image, "a", "b")
	if first.Key != "late.jpg" || second.Key != "later.jpg" {
		t.Errorf("claim order: %s, %s", first.Key, second.Key)
	}
	if first.Status != Running {
		t.Errorf("claimed status: %s", first.Status)
	}
	if _, ok, _ := db.Claim(ctx, Image, []string{"a", "b"}); ok {
		t.Error("claimed beyond the queue or outside the bucket list")
	}
	if v := mustClaim(t, db, Video, "a"); v.Key != "clip.mp4" {
		t.Errorf("video claim: %s", v.Key)
	}
}

func TestFinishRecordsResult(t *testing.T) {
	db := openTemp(t)
	db.Enqueue(ctx, "a", "x.jpg", Image, t0, 1000)
	e := mustClaim(t, db, Image, "a")
	if err := db.MarkDone(ctx, e, 200, 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, db, "a", "x.jpg")
	if got.Status != Done || got.OutputSize != 200 || got.Duration != 1500*time.Millisecond || !got.SourceModified.Equal(t0) {
		t.Errorf("done entry: %+v", got)
	}
}

func TestFailuresCountAttemptsPerSourceVersion(t *testing.T) {
	db := openTemp(t)
	for range 2 {
		db.Enqueue(ctx, "a", "x.jpg", Image, t0, 1000)
		db.MarkFailed(ctx, mustClaim(t, db, Image, "a"), errors.New("broken"), time.Second)
	}
	got := mustGet(t, db, "a", "x.jpg")
	if got.Status != Failed || got.Attempts != 2 || got.Error != "broken" {
		t.Fatalf("after two failures: %+v", got)
	}

	db.Enqueue(ctx, "a", "x.jpg", Image, t0.Add(time.Hour), 1000)
	got = mustGet(t, db, "a", "x.jpg")
	if got.Status != Queued || got.Attempts != 0 || got.Error != "" {
		t.Errorf("new source version must reset attempts: %+v", got)
	}
}

func TestEnqueueDoesNotDisturbRunningEntryOfSameVersion(t *testing.T) {
	db := openTemp(t)
	db.Enqueue(ctx, "a", "x.jpg", Image, t0, 1000)
	running := mustClaim(t, db, Image, "a")
	db.Enqueue(ctx, "a", "x.jpg", Image, t0, 1000)
	if got := mustGet(t, db, "a", "x.jpg"); got.Status != Running {
		t.Errorf("status: %s", got.Status)
	}
	db.MarkDone(ctx, running, 1, time.Second)
	if got := mustGet(t, db, "a", "x.jpg"); got.Status != Done {
		t.Errorf("status: %s", got.Status)
	}
}

func TestNewerVersionDuringConversionStaysQueued(t *testing.T) {
	db := openTemp(t)
	db.Enqueue(ctx, "a", "x.jpg", Image, t0, 1000)
	running := mustClaim(t, db, Image, "a")
	db.Enqueue(ctx, "a", "x.jpg", Image, t0.Add(time.Minute), 2000)
	db.MarkDone(ctx, running, 1, time.Second)
	got := mustGet(t, db, "a", "x.jpg")
	if got.Status != Queued || got.SourceSize != 2000 {
		t.Errorf("newer version lost: %+v", got)
	}
}

func TestOpenResetsRunningEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Enqueue(ctx, "a", "x.mp4", Video, t0, 1)
	mustClaim(t, db, Video, "a")
	db.Close()

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := mustGet(t, db, "a", "x.mp4"); got.Status != Queued {
		t.Errorf("status after restart: %s", got.Status)
	}
}

func TestPruneDropsEntriesOfUnconfiguredBuckets(t *testing.T) {
	db := openTemp(t)
	db.Enqueue(ctx, "a", "1.jpg", Image, t0, 1)
	db.Enqueue(ctx, "b", "2.jpg", Image, t0, 1)
	db.Enqueue(ctx, "old", "3.jpg", Image, t0, 1)
	db.Enqueue(ctx, "old", "4.mp4", Video, t0, 1)

	pruned, err := db.Prune(ctx, []string{"a", "b"})

	if err != nil || pruned != 2 {
		t.Fatalf("pruned %d err %v", pruned, err)
	}
	if left, _ := db.BucketEntries(ctx, "old"); len(left) != 0 {
		t.Errorf("entries of removed bucket left: %+v", left)
	}
	mustGet(t, db, "a", "1.jpg")
	mustGet(t, db, "b", "2.jpg")
	if pruned, err := db.Prune(ctx, []string{"a", "b"}); err != nil || pruned != 0 {
		t.Errorf("second prune: %d %v", pruned, err)
	}
}

func TestAdoptNeverOverwritesAnEntry(t *testing.T) {
	db := openTemp(t)
	db.Enqueue(ctx, "a", "x.jpg", Image, t0, 1000)
	before := mustGet(t, db, "a", "x.jpg")

	adopted, err := db.Adopt(
		ctx,
		"a",
		"x.jpg",
		Image,
		t0,
		1000,
		t0.Add(time.Hour),
		100,
	)

	if err != nil || adopted {
		t.Fatalf("adopted %v err %v", adopted, err)
	}
	if got := mustGet(t, db, "a", "x.jpg"); got != before {
		t.Errorf("entry changed: %+v", got)
	}
}

func TestCountsAndList(t *testing.T) {
	db := openTemp(t)
	db.Enqueue(ctx, "a", "1.jpg", Image, t0, 1)
	db.Enqueue(ctx, "a", "2.jpg", Image, t0, 1)
	db.Enqueue(ctx, "b", "3.mp4", Video, t0, 1)
	db.MarkSkipped(ctx, mustClaim(t, db, Image, "a"), 5, time.Second)

	counts, err := db.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []Count{
		{Bucket: "a", Kind: Image, Status: Queued, Count: 1},
		{Bucket: "a", Kind: Image, Status: Skipped, Count: 1},
		{Bucket: "b", Kind: Video, Status: Queued, Count: 1},
	}
	if len(counts) != len(want) {
		t.Fatalf("counts: %+v", counts)
	}
	for i := range want {
		if counts[i] != want[i] {
			t.Errorf("count %d: got %+v want %+v", i, counts[i], want[i])
		}
	}

	queued, total, err := db.List(ctx, Filter{Statuses: []Status{Queued}}, 10, 0)
	if err != nil || len(queued) != 2 || total != 2 {
		t.Fatalf("queued list: %v %d %+v", err, total, queued)
	}
	skipped, _, _ := db.List(ctx, Filter{Bucket: "a", Statuses: []Status{Skipped}}, 10, 0)
	if len(skipped) != 1 || skipped[0].Key != "1.jpg" || skipped[0].OutputSize != 5 {
		t.Errorf("skipped list: %+v", skipped)
	}
	videos, total, _ := db.List(ctx, Filter{Kind: Video, Statuses: []Status{Queued, Skipped}}, 10, 0)
	if len(videos) != 1 || total != 1 || videos[0].Key != "3.mp4" {
		t.Errorf("video list: %d %+v", total, videos)
	}
	page, total, _ := db.List(ctx, Filter{}, 1, 1)
	if len(page) != 1 || total != 3 {
		t.Errorf("paging: %d %+v", total, page)
	}
	beyond, total, _ := db.List(ctx, Filter{}, 10, 5)
	if len(beyond) != 0 || total != 3 {
		t.Errorf("past the end: %d %+v", total, beyond)
	}

	all, err := db.BucketEntries(ctx, "a")
	if err != nil || len(all) != 2 || all["2.jpg"].Status != Queued {
		t.Errorf("bucket entries: %v %+v", err, all)
	}
}

func TestTotalsSumDoneEntriesOnly(t *testing.T) {
	db := openTemp(t)
	db.Enqueue(ctx, "a", "1.jpg", Image, t0, 1000)
	db.Enqueue(ctx, "a", "2.jpg", Image, t0, 500)
	db.Enqueue(ctx, "a", "3.jpg", Image, t0, 300)
	db.Enqueue(ctx, "b", "4.mp4", Video, t0, 4000)
	db.MarkDone(ctx, mustClaim(t, db, Image, "a"), 200, time.Second)
	db.MarkDone(ctx, mustClaim(t, db, Image, "a"), 100, time.Second)
	db.MarkSkipped(ctx, mustClaim(t, db, Image, "a"), 400, time.Second)
	db.MarkDone(ctx, mustClaim(t, db, Video, "b"), 1000, time.Second)

	totals, err := db.Totals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if totals["a"] != (Bytes{Before: 1500, After: 300}) || totals["b"] != (Bytes{Before: 4000, After: 1000}) {
		t.Errorf("totals: %+v", totals)
	}
	if err := db.Ping(ctx); err != nil {
		t.Errorf("ping: %v", err)
	}
}
