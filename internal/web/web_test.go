package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"s3-webx/internal/config"
	"s3-webx/internal/state"
	"s3-webx/internal/worker"
)

var (
	ctx    = context.Background()
	t0     = time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	auth   = config.Auth{UIUser: "admin", UIPassword: "secret", APIToken: "token-123"}
)

type activityDouble struct {
	current map[state.Kind]worker.Progress
	scans   map[string]worker.ScanResult
}

func newActivityDouble() *activityDouble {
	return &activityDouble{
		current: map[state.Kind]worker.Progress{},
		scans:   map[string]worker.ScanResult{},
	}
}

func (a *activityDouble) givenRunning(kind state.Kind, p worker.Progress) *activityDouble {
	a.current[kind] = p
	return a
}

func (a *activityDouble) givenScan(bucket string, r worker.ScanResult) *activityDouble {
	a.scans[bucket] = r
	return a
}

func (a *activityDouble) Current(kind state.Kind) (worker.Progress, bool) {
	p, ok := a.current[kind]
	return p, ok
}

func (a *activityDouble) LastScan(bucket string) (worker.ScanResult, bool) {
	r, ok := a.scans[bucket]
	return r, ok
}

type fixture struct {
	db       *state.DB
	activity *activityDouble
	auth     config.Auth
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &fixture{db: db, activity: newActivityDouble(), auth: auth}
}

func (f *fixture) handler() http.Handler {
	return New(
		f.db,
		[]string{"content", "failover"},
		f.activity,
		f.auth,
		logger,
	)
}

// seed runs one entry through the state machine the way a worker would.
func (f *fixture) seed(
	t *testing.T,
	bucket string,
	key string,
	kind state.Kind,
	result state.Status,
	outputSize int64,
) {
	t.Helper()
	if err := f.db.Enqueue(ctx, bucket, key, kind, t0, 1000); err != nil {
		t.Fatal(err)
	}
	if result == state.Queued {
		return
	}
	e, ok, err := f.db.Claim(ctx, kind, []string{bucket})
	if err != nil || !ok || e.Key != key {
		t.Fatalf("claim %s: %+v %v %v", key, e, ok, err)
	}
	switch result {
	case state.Done:
		err = f.db.MarkDone(ctx, e, outputSize, 2*time.Second)
	case state.Skipped:
		err = f.db.MarkSkipped(ctx, e, outputSize, 2*time.Second)
	case state.Failed:
		err = f.db.MarkFailed(ctx, e, errors.New("vips: bad header"), time.Second)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func get(
	t *testing.T,
	h http.Handler,
	target string,
	header map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return v
}

func TestHealthz(t *testing.T) {
	f := newFixture(t)
	w := get(t, f.handler(), "/healthz", nil)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "ok" {
		t.Errorf("open db: %d %q", w.Code, w.Body.String())
	}

	f.db.Close()
	w = get(t, f.handler(), "/healthz", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("closed db: %d", w.Code)
	}
}

func basicRequest(user, password string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.SetBasicAuth(user, password)
	return r
}

func TestOverviewBasicAuth(t *testing.T) {
	tests := []struct {
		name    string
		auth    config.Auth
		request *http.Request
		want    int
	}{
		{"valid", auth, basicRequest("admin", "secret"), http.StatusOK},
		{"wrong password", auth, basicRequest("admin", "nope"), http.StatusUnauthorized},
		{"wrong user", auth, basicRequest("root", "secret"), http.StatusUnauthorized},
		{"missing", auth, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusUnauthorized},
		{"password not configured", config.Auth{UIUser: "admin"}, basicRequest("admin", ""), http.StatusForbidden},
		{"user not configured", config.Auth{UIPassword: "secret"}, basicRequest("", "secret"), http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.auth = tt.auth
			w := httptest.NewRecorder()
			f.handler().ServeHTTP(w, tt.request)
			if w.Code != tt.want {
				t.Fatalf("status %d, want %d", w.Code, tt.want)
			}
			challenge := w.Header().Get("WWW-Authenticate")
			if tt.want == http.StatusUnauthorized && !strings.HasPrefix(challenge, `Basic realm="s3-webx"`) {
				t.Errorf("challenge %q", challenge)
			}
			if tt.want != http.StatusUnauthorized && challenge != "" {
				t.Errorf("unexpected challenge %q", challenge)
			}
		})
	}
}

func TestAPIBearerAuth(t *testing.T) {
	tests := []struct {
		name   string
		token  string
		header map[string]string
		want   int
	}{
		{"valid", "token-123", bearer("token-123"), http.StatusOK},
		{"lowercase scheme", "token-123", map[string]string{"Authorization": "bearer token-123"}, http.StatusOK},
		{"wrong", "token-123", bearer("token-12"), http.StatusUnauthorized},
		{"basic instead", "token-123", map[string]string{"Authorization": "Basic token-123"}, http.StatusUnauthorized},
		{"missing", "token-123", nil, http.StatusUnauthorized},
		{"not configured", "", bearer(""), http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.auth.APIToken = tt.token
			for _, target := range []string{"/api/status", "/api/buckets/content/files", "/api/unknown"} {
				w := get(t, f.handler(), target, tt.header)
				want := tt.want
				if target == "/api/unknown" && want == http.StatusOK {
					want = http.StatusNotFound
				}
				if w.Code != want {
					t.Errorf("%s: status %d, want %d", target, w.Code, want)
				}
				challenge := w.Header().Get("WWW-Authenticate")
				if (want == http.StatusUnauthorized) != strings.HasPrefix(challenge, "Bearer") {
					t.Errorf("%s: challenge %q", target, challenge)
				}
				if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
					t.Errorf("%s: content type %q", target, ct)
				}
			}
		})
	}
}

func TestStatusShape(t *testing.T) {
	f := newFixture(t)
	started := time.Date(2026, 10, 29, 9, 30, 0, 0, time.UTC)
	f.activity.
		givenRunning(state.Video, worker.Progress{Bucket: "content", Key: "film.mp4", Started: started, Percent: 42.5}).
		givenScan("content", worker.ScanResult{At: started}).
		givenScan("failover", worker.ScanResult{At: started, Err: errors.New("dial tcp: connection refused")})
	f.seed(t, "content", "a.jpg", state.Image, state.Done, 200)
	f.seed(t, "content", "b.jpg", state.Image, state.Skipped, 1200)
	f.seed(t, "content", "c.jpg", state.Image, state.Queued, 0)
	f.seed(t, "content", "d.mp4", state.Video, state.Done, 300)
	f.seed(t, "content", "e.mp4", state.Video, state.Failed, 0)
	f.seed(t, "removed", "x.jpg", state.Image, state.Done, 1)

	w := get(t, f.handler(), "/api/status", bearer("token-123"))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	want := `{"workers":[{"kind":"image","idle":true},` +
		`{"kind":"video","idle":false,"bucket":"content","key":"film.mp4","startedAt":"2026-10-29T09:30:00Z","percent":42.5}],` +
		`"buckets":[{"name":"content","lastScan":"2026-10-29T09:30:00Z","queued":1,` +
		`"counts":{"image":{"done":1,"failed":0,"queued":1,"running":0,"skipped":1},` +
		`"video":{"done":1,"failed":1,"queued":0,"running":0,"skipped":0}},` +
		`"bytesBefore":2000,"bytesAfter":500,"bytesSaved":1500},` +
		`{"name":"failover","lastScan":"2026-10-29T09:30:00Z","lastScanError":"dial tcp: connection refused","queued":0,` +
		`"counts":{"image":{"done":0,"failed":0,"queued":0,"running":0,"skipped":0},` +
		`"video":{"done":0,"failed":0,"queued":0,"running":0,"skipped":0}},` +
		`"bytesBefore":0,"bytesAfter":0,"bytesSaved":0}]}`
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Errorf("body\n got %s\nwant %s", got, want)
	}
}

func TestStatusBeforeFirstScan(t *testing.T) {
	f := newFixture(t)
	s := decode[map[string][]map[string]any](t, get(t, f.handler(), "/api/status", bearer("token-123")))
	if v, ok := s["buckets"][0]["lastScan"]; !ok || v != nil {
		t.Errorf("lastScan before first scan: %v %v", v, ok)
	}
}

func TestFileListFiltersAndLimits(t *testing.T) {
	f := newFixture(t)
	f.seed(t, "content", "a.jpg", state.Image, state.Done, 200)
	f.seed(t, "content", "b.jpg", state.Image, state.Done, 200)
	f.seed(t, "content", "c.jpg", state.Image, state.Failed, 0)
	f.seed(t, "content", "d.mp4", state.Video, state.Done, 300)
	f.seed(t, "failover", "e.jpg", state.Image, state.Done, 200)
	h := f.handler()

	tests := []struct {
		query string
		total int
		keys  []string
		limit int
	}{
		{"", 4, []string{"d.mp4", "c.jpg", "b.jpg", "a.jpg"}, 100},
		{"?kind=image", 3, []string{"c.jpg", "b.jpg", "a.jpg"}, 100},
		{"?status=done&kind=image", 2, []string{"b.jpg", "a.jpg"}, 100},
		{"?status=done,failed&kind=image", 3, []string{"c.jpg", "b.jpg", "a.jpg"}, 100},
		{"?status=done&status=failed&kind=video", 1, []string{"d.mp4"}, 100},
		{"?limit=2&offset=1", 4, []string{"c.jpg", "b.jpg"}, 2},
		{"?limit=1000&offset=10", 4, nil, 1000},
	}
	for _, tt := range tests {
		w := get(t, h, "/api/buckets/content/files"+tt.query, bearer("token-123"))
		if w.Code != http.StatusOK {
			t.Errorf("%s: status %d %s", tt.query, w.Code, w.Body.String())
			continue
		}
		list := decode[fileList](t, w)
		var keys []string
		for _, file := range list.Files {
			keys = append(keys, file.Key)
		}
		if list.Total != tt.total || list.Limit != tt.limit || strings.Join(keys, " ") != strings.Join(tt.keys, " ") {
			t.Errorf("%s: total %d limit %d keys %v", tt.query, list.Total, list.Limit, keys)
		}
	}
	if body := get(t, h, "/api/buckets/content/files?offset=10", bearer("token-123")).Body.String(); !strings.Contains(body, `"files":[]`) {
		t.Errorf("empty page is not an empty array: %s", body)
	}

	for _, query := range []string{"?limit=0", "?limit=1001", "?limit=x", "?offset=-1", "?status=done,nope", "?kind=audio"} {
		w := get(t, h, "/api/buckets/content/files"+query, bearer("token-123"))
		if w.Code != http.StatusBadRequest || decode[map[string]string](t, w)["error"] == "" {
			t.Errorf("%s: status %d %s", query, w.Code, w.Body.String())
		}
	}
}

func TestSingleFile(t *testing.T) {
	f := newFixture(t)
	key := "aktion/2026-10-rstorc/Foto 1.JPG"
	f.seed(t, "content", key, state.Image, state.Done, 200)
	f.seed(t, "content", "film #2?.mp4", state.Video, state.Queued, 0)
	f.seed(t, "content", "big.png", state.Image, state.Skipped, 1500)
	h := f.handler()
	path := func(k string) string {
		return "/api/buckets/content/files/" + (&url.URL{Path: k}).EscapedPath()
	}

	w := get(t, h, path(key), bearer("token-123"))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	done := decode[file](t, w)
	if done.Key != key || done.VariantKey != key+".webp" || !done.VariantAvailable ||
		done.Status != state.Done || done.SourceSize != 1000 || done.VariantSize != 200 || done.DurationMs != 2000 {
		t.Errorf("done file: %+v", done)
	}

	queued := decode[file](t, get(t, h, path("film #2?.mp4"), bearer("token-123")))
	if queued.VariantKey != "film #2?.mp4.webm" || queued.VariantAvailable || queued.Status != state.Queued {
		t.Errorf("queued file: %+v", queued)
	}
	skipped := decode[file](t, get(t, h, path("big.png"), bearer("token-123")))
	if skipped.VariantAvailable || skipped.Status != state.Skipped {
		t.Errorf("skipped file: %+v", skipped)
	}

	for _, target := range []string{path("aktion/2026-10-rstorc/Foto 2.JPG"), path(key + ".webp"), "/api/buckets/content/files/"} {
		w := get(t, h, target, bearer("token-123"))
		if w.Code != http.StatusNotFound || decode[map[string]string](t, w)["error"] != "unknown file" {
			t.Errorf("%s: status %d %s", target, w.Code, w.Body.String())
		}
	}
}

func TestUnknownBucket(t *testing.T) {
	f := newFixture(t)
	f.seed(t, "removed", "a.jpg", state.Image, state.Done, 1)
	for _, target := range []string{"/api/buckets/removed/files", "/api/buckets/removed/files/a.jpg"} {
		w := get(t, f.handler(), target, bearer("token-123"))
		if w.Code != http.StatusNotFound || decode[map[string]string](t, w)["error"] != "unknown bucket" {
			t.Errorf("%s: status %d %s", target, w.Code, w.Body.String())
		}
	}
}

func TestOverviewShowsWorkersBucketsAndResults(t *testing.T) {
	f := newFixture(t)
	f.activity.
		givenRunning(state.Video, worker.Progress{Bucket: "content", Key: "clips/<b>film</b>.mp4", Started: time.Now(), Percent: 37}).
		givenScan("content", worker.ScanResult{At: t0})
	f.seed(t, "content", "fotos/a.jpg", state.Image, state.Done, 200)
	f.seed(t, "content", "fotos/broken.jpg", state.Image, state.Failed, 0)

	r := basicRequest("admin", "secret")
	w := httptest.NewRecorder()
	f.handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("status %d type %q", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	for _, want := range []string{
		`<meta http-equiv="refresh" content="10">`,
		"idle",
		"clips/&lt;b&gt;film&lt;/b&gt;.mp4",
		"37.0 %",
		"width: 37.0%",
		"2026-10-29 08:00:00 UTC",
		"fotos/a.jpg",
		"1000 B",
		"vips: bad header",
		"failover",
		"not yet",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
	if strings.Contains(body, "<script") || strings.Contains(body, "<b>film") {
		t.Error("overview contains script or unescaped key")
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 9472360: "9.0 MiB", -2048: "-2.0 KiB"} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
