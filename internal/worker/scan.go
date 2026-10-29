package worker

import (
	"context"
	"log/slog"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"s3-webx/internal/config"
	"s3-webx/internal/state"
	"s3-webx/internal/store"
)

type Store interface {
	List(ctx context.Context) ([]store.Object, error)
	Download(ctx context.Context, key, path string) error
	Upload(
		ctx context.Context,
		key string,
		path string,
		contentType string,
		cacheControl string,
	) error
	CacheControl(ctx context.Context, key string) (string, error)
}

type Target struct {
	Config config.Bucket
	Store  Store
}

var variantExtension = map[state.Kind]string{
	state.Image: ".webp",
	state.Video: ".webm",
}

var contentType = map[state.Kind]string{
	state.Image: "image/webp",
	state.Video: "video/webm",
}

// KindOf never treats a variant as input, even if a format list were to name its extension.
func KindOf(key string, bucket config.Bucket) (state.Kind, bool) {
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(key)), ".")
	switch {
	case ext == "" || ext == "webp" || ext == "webm":
		return "", false
	case slices.Contains(bucket.Image.Formats, ext):
		return state.Image, true
	case bucket.Video.Enabled && slices.Contains(bucket.Video.Formats, ext):
		return state.Video, true
	}
	return "", false
}

func VariantKey(key string, kind state.Kind) string {
	return key + variantExtension[kind]
}

// Decide is nil-tolerant: variant is nil when it does not exist, entry when state.db has no record.
// With keepIfLarger a skipped entry is planned again, so sources skipped under the old setting get their variant.
func Decide(
	source store.Object,
	variant *store.Object,
	entry *state.Entry,
	keepIfLarger bool,
) bool {
	if entry != nil && entry.SameSource(source.LastModified, source.Size) {
		if entry.Status == state.Skipped && !keepIfLarger {
			return false
		}
		if entry.Status == state.Failed && entry.Attempts >= state.MaxAttempts {
			return false
		}
	}
	return variant == nil || variant.LastModified.Before(source.LastModified)
}

type Scanner struct {
	Target Target
	State  *state.DB
	Notify func(state.Kind)
	Logger *slog.Logger

	mu   sync.Mutex
	last *ScanResult
}

type ScanResult struct {
	At  time.Time
	Err error
}

func (s *Scanner) LastScan() (ScanResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		return ScanResult{}, false
	}
	return *s.last, true
}

func (s *Scanner) Run(ctx context.Context) {
	ticker := time.NewTicker(s.Target.Config.Interval)
	defer ticker.Stop()
	for {
		s.scanAndLog(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Scanner) scanAndLog(ctx context.Context) {
	started := time.Now()
	queued, adopted, err := s.Scan(ctx)
	// A scan cut short by shutdown says nothing about the bucket, so it is not recorded.
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	s.last = &ScanResult{At: time.Now(), Err: err}
	s.mu.Unlock()
	if err != nil {
		s.Logger.Error("scan failed", "bucket", s.Target.Config.Name, "error", err)
		return
	}
	s.Logger.Info(
		"scan done",
		"bucket", s.Target.Config.Name,
		"images", queued[state.Image],
		"videos", queued[state.Video],
		"adopted", adopted,
		"duration", time.Since(started).Round(time.Millisecond).String(),
	)
}

// Scan returns how many originals per kind it found in need of conversion and how many current
// variants without an entry it adopted as done.
func (s *Scanner) Scan(ctx context.Context) (map[state.Kind]int, int, error) {
	bucket := s.Target.Config
	objects, err := s.Target.Store.List(ctx)
	if err != nil {
		return nil, 0, err
	}
	entries, err := s.State.BucketEntries(ctx, bucket.Name)
	if err != nil {
		return nil, 0, err
	}
	byKey := make(map[string]store.Object, len(objects))
	for _, o := range objects {
		byKey[o.Key] = o
	}
	queued := map[state.Kind]int{}
	adopted := 0
	for _, source := range objects {
		kind, ok := KindOf(source.Key, bucket)
		if !ok {
			continue
		}
		var variant *store.Object
		if v, ok := byKey[VariantKey(source.Key, kind)]; ok {
			variant = &v
		}
		var entry *state.Entry
		if e, ok := entries[source.Key]; ok {
			entry = &e
		}
		if !Decide(
			source,
			variant,
			entry,
			bucket.KeepIfLarger,
		) {
			// Without an entry Decide only declines because the variant is current, e.g. after state.db was lost.
			if entry == nil {
				ok, err := s.State.Adopt(
					ctx,
					bucket.Name,
					source.Key,
					kind,
					source.LastModified,
					source.Size,
					variant.LastModified,
					variant.Size,
				)
				if err != nil {
					return nil, 0, err
				}
				if ok {
					adopted++
				}
			}
			continue
		}
		if err := s.State.Enqueue(
			ctx,
			bucket.Name,
			source.Key,
			kind,
			source.LastModified,
			source.Size,
		); err != nil {
			return nil, 0, err
		}
		queued[kind]++
	}
	for kind, n := range queued {
		if n > 0 && s.Notify != nil {
			s.Notify(kind)
		}
	}
	return queued, adopted, nil
}
