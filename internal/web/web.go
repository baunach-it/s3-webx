package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"s3-webx/internal/config"
	"s3-webx/internal/state"
	"s3-webx/internal/worker"
)

const (
	defaultLimit = 100
	maxLimit     = 1000
	pageSize     = 25
)

var (
	kinds    = []state.Kind{state.Image, state.Video}
	statuses = []state.Status{state.Queued, state.Running, state.Done, state.Skipped, state.Failed}
)

type Activity interface {
	Current(kind state.Kind) (worker.Progress, bool)
	LastScan(bucket string) (worker.ScanResult, bool)
}

type Handler struct {
	db       *state.DB
	buckets  []string
	activity Activity
	auth     config.Auth
	logger   *slog.Logger
	mux      *http.ServeMux
}

func New(
	db *state.DB,
	buckets []string,
	activity Activity,
	auth config.Auth,
	logger *slog.Logger,
) *Handler {
	h := &Handler{
		db:       db,
		buckets:  buckets,
		activity: activity,
		auth:     auth,
		logger:   logger,
		mux:      http.NewServeMux(),
	}
	h.mux.HandleFunc("GET /healthz", h.healthz)
	h.mux.HandleFunc("GET /{$}", h.basic(h.overview))
	h.mux.HandleFunc("GET /assets/app.css", h.basic(h.stylesheet))
	h.mux.HandleFunc("GET /api/status", h.bearer(h.apiStatus))
	h.mux.HandleFunc("GET /api/buckets/{name}/files", h.bearer(h.apiFiles))
	h.mux.HandleFunc("GET /api/buckets/{name}/files/{key...}", h.bearer(h.apiFile))
	// Unknown API paths answer only after the token check, so probing reveals nothing.
	h.mux.HandleFunc("/api/", h.bearer(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	}))
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// Hashing first gives both sides the same length, so the comparison time does not reveal it.
func equal(given, want string) int {
	g := sha256.Sum256([]byte(given))
	w := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:])
}

func (h *Handler) basic(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.auth.UIUser == "" || h.auth.UIPassword == "" {
			http.Error(w, "overview disabled: auth.ui_user and auth.ui_password are not configured", http.StatusForbidden)
			return
		}
		user, password, ok := r.BasicAuth()
		valid := equal(user, h.auth.UIUser) & equal(password, h.auth.UIPassword)
		if !ok || valid != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="s3-webx", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (h *Handler) bearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.auth.APIToken == "" {
			writeError(w, http.StatusForbidden, "api disabled: auth.api_token is not configured")
			return
		}
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || equal(strings.TrimSpace(token), h.auth.APIToken) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="s3-webx"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	w.Header().Set("Cache-Control", "no-store")
	if err := h.db.Ping(ctx); err != nil {
		h.logger.Error("health check failed", "error", err)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ok")
}

type workerStatus struct {
	Kind      state.Kind `json:"kind"`
	Idle      bool       `json:"idle"`
	Bucket    string     `json:"bucket,omitempty"`
	Key       string     `json:"key,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	Percent   *float64   `json:"percent,omitempty"`
}

type bucketStatus struct {
	Name          string                              `json:"name"`
	LastScan      *time.Time                          `json:"lastScan"`
	LastScanError string                              `json:"lastScanError,omitempty"`
	Queued        int                                 `json:"queued"`
	Counts        map[state.Kind]map[state.Status]int `json:"counts"`
	BytesBefore   int64                               `json:"bytesBefore"`
	BytesAfter    int64                               `json:"bytesAfter"`
	BytesSaved    int64                               `json:"bytesSaved"`
}

type status struct {
	Workers []workerStatus `json:"workers"`
	Buckets []bucketStatus `json:"buckets"`
}

func (h *Handler) status(ctx context.Context) (status, error) {
	counts, err := h.db.Counts(ctx)
	if err != nil {
		return status{}, err
	}
	totals, err := h.db.Totals(ctx)
	if err != nil {
		return status{}, err
	}
	s := status{Workers: []workerStatus{}, Buckets: []bucketStatus{}}
	for _, kind := range kinds {
		ws := workerStatus{Kind: kind, Idle: true}
		if p, ok := h.activity.Current(kind); ok {
			ws = workerStatus{Kind: kind, Bucket: p.Bucket, Key: p.Key, StartedAt: utc(p.Started)}
			if kind == state.Video {
				ws.Percent = &p.Percent
			}
		}
		s.Workers = append(s.Workers, ws)
	}
	for _, name := range h.buckets {
		b := bucketStatus{Name: name, Counts: map[state.Kind]map[state.Status]int{}}
		for _, kind := range kinds {
			b.Counts[kind] = map[state.Status]int{}
			for _, st := range statuses {
				b.Counts[kind][st] = 0
			}
		}
		for _, c := range counts {
			if c.Bucket != name || b.Counts[c.Kind] == nil {
				continue
			}
			b.Counts[c.Kind][c.Status] = c.Count
			if c.Status == state.Queued {
				b.Queued += c.Count
			}
		}
		if scan, ok := h.activity.LastScan(name); ok {
			b.LastScan = utc(scan.At)
			if scan.Err != nil {
				b.LastScanError = scan.Err.Error()
			}
		}
		t := totals[name]
		b.BytesBefore, b.BytesAfter, b.BytesSaved = t.Before, t.After, t.Before-t.After
		s.Buckets = append(s.Buckets, b)
	}
	return s, nil
}

func (h *Handler) apiStatus(w http.ResponseWriter, r *http.Request) {
	s, err := h.status(r.Context())
	if err != nil {
		h.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

type file struct {
	Bucket           string       `json:"bucket"`
	Key              string       `json:"key"`
	Kind             state.Kind   `json:"kind"`
	Status           state.Status `json:"status"`
	SourceModified   time.Time    `json:"sourceModified"`
	SourceSize       int64        `json:"sourceSize"`
	VariantKey       string       `json:"variantKey"`
	VariantSize      int64        `json:"variantSize"`
	VariantAvailable bool         `json:"variantAvailable"`
	Attempts         int          `json:"attempts"`
	Error            string       `json:"error,omitempty"`
	DurationMs       int64        `json:"durationMs"`
	QueuedAt         time.Time    `json:"queuedAt"`
	UpdatedAt        time.Time    `json:"updatedAt"`
}

func toFile(e state.Entry) file {
	return file{
		Bucket:           e.Bucket,
		Key:              e.Key,
		Kind:             e.Kind,
		Status:           e.Status,
		SourceModified:   e.SourceModified,
		SourceSize:       e.SourceSize,
		VariantKey:       worker.VariantKey(e.Key, e.Kind),
		VariantSize:      e.OutputSize,
		VariantAvailable: e.Status == state.Done,
		Attempts:         e.Attempts,
		Error:            e.Error,
		DurationMs:       e.Duration.Milliseconds(),
		QueuedAt:         e.QueuedAt,
		UpdatedAt:        e.UpdatedAt,
	}
}

type fileList struct {
	Total  int    `json:"total"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
	Files  []file `json:"files"`
}

func (h *Handler) apiFiles(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !slices.Contains(h.buckets, name) {
		writeError(w, http.StatusNotFound, "unknown bucket")
		return
	}
	filter, limit, offset, problem := parseListQuery(r)
	if problem != "" {
		writeError(w, http.StatusBadRequest, problem)
		return
	}
	filter.Bucket = name
	entries, total, err := h.db.List(r.Context(), filter, limit, offset)
	if err != nil {
		h.internalError(w, err)
		return
	}
	list := fileList{Total: total, Limit: limit, Offset: offset, Files: []file{}}
	for _, e := range entries {
		list.Files = append(list.Files, toFile(e))
	}
	writeJSON(w, http.StatusOK, list)
}

func parseListQuery(r *http.Request) (state.Filter, int, int, string) {
	q := r.URL.Query()
	var f state.Filter
	for _, value := range q["status"] {
		for st := range strings.SplitSeq(value, ",") {
			if !slices.Contains(statuses, state.Status(st)) {
				return f, 0, 0, fmt.Sprintf("unknown status %q", st)
			}
			f.Statuses = append(f.Statuses, state.Status(st))
		}
	}
	if kind := q.Get("kind"); kind != "" {
		if !slices.Contains(kinds, state.Kind(kind)) {
			return f, 0, 0, fmt.Sprintf("unknown kind %q", kind)
		}
		f.Kind = state.Kind(kind)
	}
	limit := defaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			return f, 0, 0, fmt.Sprintf("limit must be between 1 and %d", maxLimit)
		}
		limit = n
	}
	offset := 0
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return f, 0, 0, "offset must be 0 or greater"
		}
		offset = n
	}
	return f, limit, offset, ""
}

func (h *Handler) apiFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !slices.Contains(h.buckets, name) {
		writeError(w, http.StatusNotFound, "unknown bucket")
		return
	}
	e, ok, err := h.db.Get(r.Context(), name, r.PathValue("key"))
	if err != nil {
		h.internalError(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "unknown file")
		return
	}
	writeJSON(w, http.StatusOK, toFile(e))
}

//go:embed overview.html
var overviewSource string

// app.css is generated by Tailwind from tailwind.css and overview.html (./dev css, Dockerfile stage css).
//
//go:embed app.css
var appCSS []byte

// The hash in the link lets the browser cache the stylesheet for a year and still get every new build.
var cssVersion = func() string {
	sum := sha256.Sum256(appCSS)
	return hex.EncodeToString(sum[:6])
}()

var overviewTemplate = template.Must(template.New("overview.html").Funcs(template.FuncMap{
	"bytes":    formatBytes,
	"change":   sizeChange,
	"duration": formatDuration,
	"time":     func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") },
	"percent":  func(p float64) string { return strconv.FormatFloat(p, 'f', 1, 64) + " %" },
	"clamp":    func(p float64) string { return strconv.FormatFloat(min(max(p, 0), 100), 'f', 1, 64) },
	"share":    share,
}).Parse(overviewSource))

type overviewWorker struct {
	workerStatus
	Label   string
	Plural  string
	Queued  int
	Running time.Duration
}

type overviewTotals struct {
	Before  int64
	After   int64
	Saved   int64
	Queued  int
	Running int
	Done    int
	Skipped int
	Failed  int
	Images  int
	Videos  int
}

type overviewPage struct {
	Now        time.Time
	CSSVersion string
	Converting bool
	Kinds      []state.Kind
	Statuses   []state.Status
	Totals     overviewTotals
	Workers    []overviewWorker
	Buckets    []bucketStatus
	Recent     []state.Entry
	Failures   []state.Entry
	RecentPage pager
	FailPage   pager
}

type pager struct {
	Page  int
	Pages int
	Total int
	Newer string
	Older string
}

// listPage never fails on a bad page number: anything unreadable is page 1, anything past the end the last page.
func (h *Handler) listPage(
	ctx context.Context,
	filter state.Filter,
	raw string,
) ([]state.Entry, pager, error) {
	p, err := strconv.Atoi(raw)
	if err != nil || p < 1 {
		p = 1
	}
	entries, total, err := h.db.List(ctx, filter, pageSize, (p-1)*pageSize)
	if err != nil {
		return nil, pager{}, err
	}
	pages := max(1, (total+pageSize-1)/pageSize)
	if p > pages {
		p = pages
		if entries, total, err = h.db.List(ctx, filter, pageSize, (p-1)*pageSize); err != nil {
			return nil, pager{}, err
		}
	}
	return entries, pager{Page: p, Pages: pages, Total: total}, nil
}

func pageLink(recent, failures int, anchor string) string {
	return fmt.Sprintf("?recent=%d&failures=%d#%s", recent, failures, anchor)
}

var workerLabels = map[state.Kind][2]string{
	state.Image: {"Image worker", "images"},
	state.Video: {"Video worker", "videos"},
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s, err := h.status(ctx)
	if err != nil {
		h.internalError(w, err)
		return
	}
	q := r.URL.Query()
	recent, rp, err := h.listPage(ctx, state.Filter{Statuses: []state.Status{state.Done, state.Skipped}}, q.Get("recent"))
	if err != nil {
		h.internalError(w, err)
		return
	}
	failures, fp, err := h.listPage(ctx, state.Filter{Statuses: []state.Status{state.Failed}}, q.Get("failures"))
	if err != nil {
		h.internalError(w, err)
		return
	}
	if rp.Page > 1 {
		rp.Newer = pageLink(rp.Page-1, fp.Page, "recent")
	}
	if rp.Page < rp.Pages {
		rp.Older = pageLink(rp.Page+1, fp.Page, "recent")
	}
	if fp.Page > 1 {
		fp.Newer = pageLink(rp.Page, fp.Page-1, "failures")
	}
	if fp.Page < fp.Pages {
		fp.Older = pageLink(rp.Page, fp.Page+1, "failures")
	}
	page := overviewPage{
		Now:        time.Now(),
		CSSVersion: cssVersion,
		Kinds:      kinds,
		Statuses:   statuses,
		Buckets:    s.Buckets,
		Recent:     recent,
		Failures:   failures,
		RecentPage: rp,
		FailPage:   fp,
	}
	t := &page.Totals
	for _, b := range s.Buckets {
		t.Before += b.BytesBefore
		t.After += b.BytesAfter
		t.Saved += b.BytesSaved
		for _, kind := range kinds {
			c := b.Counts[kind]
			t.Queued += c[state.Queued]
			t.Running += c[state.Running]
			t.Done += c[state.Done]
			t.Skipped += c[state.Skipped]
			t.Failed += c[state.Failed]
		}
		t.Images += b.Counts[state.Image][state.Done]
		t.Videos += b.Counts[state.Video][state.Done]
	}
	for _, ws := range s.Workers {
		ow := overviewWorker{workerStatus: ws, Label: workerLabels[ws.Kind][0], Plural: workerLabels[ws.Kind][1]}
		for _, b := range s.Buckets {
			ow.Queued += b.Counts[ws.Kind][state.Queued]
		}
		if ws.StartedAt != nil {
			ow.Running = page.Now.Sub(*ws.StartedAt)
			page.Converting = true
		}
		page.Workers = append(page.Workers, ow)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", overviewCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if err := overviewTemplate.Execute(w, page); err != nil {
		h.logger.Error("render overview", "error", err)
	}
}

const overviewCSP = "default-src 'none'; style-src 'self'; img-src 'self' data:; frame-ancestors 'none'"

// private, because the response answers an authenticated request and must not sit in a shared cache.
func (h *Handler) stylesheet(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(appCSS)
}

// share is part as a percentage of whole, 0 when there is nothing to compare against.
func share(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) / float64(whole) * 100
}

func (h *Handler) internalError(w http.ResponseWriter, err error) {
	h.logger.Error("http request failed", "error", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]string{"error": message})
}

func utc(t time.Time) *time.Time {
	u := t.UTC()
	return &u
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < 0 {
		return "-" + formatBytes(-n)
	}
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

type change struct {
	Text  string
	Class string
}

// sizeChange compares the rounded value, so a change that shows as 0.0 is never coloured.
func sizeChange(source, output int64) change {
	if source <= 0 {
		return change{Text: "–", Class: "same"}
	}
	p := math.Round(float64(output-source)/float64(source)*1000) / 10
	switch {
	case p < 0:
		return change{Text: "−" + strconv.FormatFloat(-p, 'f', 1, 64) + " %", Class: "smaller"}
	case p > 0:
		return change{Text: "+" + strconv.FormatFloat(p, 'f', 1, 64) + " %", Class: "larger"}
	default:
		return change{Text: "±0.0 %", Class: "same"}
	}
}
