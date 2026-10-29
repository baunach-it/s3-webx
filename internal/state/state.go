package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Kind string

const (
	Image Kind = "image"
	Video Kind = "video"
)

type Status string

const (
	Queued  Status = "queued"
	Running Status = "running"
	Done    Status = "done"
	Skipped Status = "skipped"
	Failed  Status = "failed"
)

const MaxAttempts = 3

type Entry struct {
	Bucket         string
	Key            string
	Kind           Kind
	Status         Status
	SourceModified time.Time
	SourceSize     int64
	OutputSize     int64
	Attempts       int
	Error          string
	Duration       time.Duration
	QueuedAt       time.Time
	UpdatedAt      time.Time
}

func (e Entry) SameSource(modified time.Time, size int64) bool {
	return e.SourceModified.Equal(modified) && e.SourceSize == size
}

type Count struct {
	Bucket string
	Kind   Kind
	Status Status
	Count  int
}

type DB struct {
	db  *sql.DB
	now func() time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS entries (
	bucket          TEXT    NOT NULL,
	key             TEXT    NOT NULL,
	kind            TEXT    NOT NULL,
	status          TEXT    NOT NULL,
	source_modified INTEGER NOT NULL,
	source_size     INTEGER NOT NULL,
	output_size     INTEGER NOT NULL DEFAULT 0,
	attempts        INTEGER NOT NULL DEFAULT 0,
	error           TEXT    NOT NULL DEFAULT '',
	duration_ms     INTEGER NOT NULL DEFAULT 0,
	queued_at       INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL,
	PRIMARY KEY (bucket, key)
);
CREATE INDEX IF NOT EXISTS entries_queue ON entries (kind, status, queued_at);
`

// Open resets entries left running by a previous process, since nothing can be running at startup.
func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open state: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create state schema: %w", err)
	}
	if _, err := db.Exec(`UPDATE entries SET status = ? WHERE status = ?`, Queued, Running); err != nil {
		db.Close()
		return nil, fmt.Errorf("reset running entries: %w", err)
	}
	return &DB{db: db, now: time.Now}, nil
}

func (d *DB) Close() error {
	return d.db.Close()
}

const columns = `bucket, key, kind, status, source_modified, source_size, output_size, attempts, error, duration_ms, queued_at, updated_at`

func scanEntry(row interface{ Scan(...any) error }) (Entry, error) {
	var e Entry
	var modified, durationMS, queuedAt, updatedAt int64
	err := row.Scan(
		&e.Bucket,
		&e.Key,
		&e.Kind,
		&e.Status,
		&modified,
		&e.SourceSize,
		&e.OutputSize,
		&e.Attempts,
		&e.Error,
		&durationMS,
		&queuedAt,
		&updatedAt,
	)
	e.SourceModified = time.Unix(0, modified).UTC()
	e.Duration = time.Duration(durationMS) * time.Millisecond
	e.QueuedAt = time.Unix(0, queuedAt).UTC()
	e.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return e, err
}

func (d *DB) Get(ctx context.Context, bucket, key string) (Entry, bool, error) {
	row := d.db.QueryRowContext(ctx, `SELECT `+columns+` FROM entries WHERE bucket = ? AND key = ?`, bucket, key)
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("get entry: %w", err)
	}
	return e, true, nil
}

func (d *DB) BucketEntries(ctx context.Context, bucket string) (map[string]Entry, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+columns+` FROM entries WHERE bucket = ?`, bucket)
	if err != nil {
		return nil, fmt.Errorf("list bucket entries: %w", err)
	}
	defer rows.Close()
	entries := map[string]Entry{}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("scan entry: %w", err)
		}
		entries[e.Key] = e
	}
	return entries, rows.Err()
}

// Enqueue leaves a queued or running entry of the same source version alone and resets the attempts
// only when the source has changed.
func (d *DB) Enqueue(
	ctx context.Context,
	bucket string,
	key string,
	kind Kind,
	modified time.Time,
	size int64,
) error {
	now := d.now().UnixNano()
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO entries (bucket, key, kind, status, source_modified, source_size, queued_at, updated_at)
		VALUES (?, ?, ?, 'queued', ?, ?, ?, ?)
		ON CONFLICT (bucket, key) DO UPDATE SET
			kind = excluded.kind,
			status = 'queued',
			attempts = CASE WHEN source_modified = excluded.source_modified AND source_size = excluded.source_size
				THEN attempts ELSE 0 END,
			error = '',
			source_modified = excluded.source_modified,
			source_size = excluded.source_size,
			queued_at = excluded.queued_at,
			updated_at = excluded.updated_at
		WHERE NOT (status IN ('queued', 'running')
			AND source_modified = excluded.source_modified AND source_size = excluded.source_size)`,
		bucket,
		key,
		kind,
		modified.UnixNano(),
		size,
		now,
		now,
	)
	if err != nil {
		return fmt.Errorf("enqueue %s/%s: %w", bucket, key, err)
	}
	return nil
}

// Claim takes the longest-waiting queued entry of a kind among the given buckets and marks it running.
func (d *DB) Claim(ctx context.Context, kind Kind, buckets []string) (Entry, bool, error) {
	if len(buckets) == 0 {
		return Entry{}, false, nil
	}
	args := []any{Running, d.now().UnixNano(), kind}
	placeholders := ""
	for i, b := range buckets {
		if i > 0 {
			placeholders += ", "
		}
		placeholders += "?"
		args = append(args, b)
	}
	row := d.db.QueryRowContext(ctx, `
		UPDATE entries SET status = ?, updated_at = ?
		WHERE rowid = (
			SELECT rowid FROM entries
			WHERE kind = ? AND status = 'queued' AND bucket IN (`+placeholders+`)
			ORDER BY queued_at, source_modified LIMIT 1)
		RETURNING `+columns, args...)
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("claim %s: %w", kind, err)
	}
	return e, true, nil
}

// Finishing is tied to the source version the worker converted, so a newer version enqueued
// meanwhile stays queued.
func (d *DB) finish(ctx context.Context, e Entry, set string, args ...any) error {
	args = append(args, d.now().UnixNano(), e.Bucket, e.Key, e.SourceModified.UnixNano(), e.SourceSize)
	_, err := d.db.ExecContext(ctx, `UPDATE entries SET `+set+`, updated_at = ?
		WHERE bucket = ? AND key = ? AND status = 'running' AND source_modified = ? AND source_size = ?`, args...)
	if err != nil {
		return fmt.Errorf("update %s/%s: %w", e.Bucket, e.Key, err)
	}
	return nil
}

func (d *DB) MarkDone(
	ctx context.Context,
	e Entry,
	outputSize int64,
	duration time.Duration,
) error {
	return d.finish(ctx, e, `status = 'done', output_size = ?, duration_ms = ?, error = ''`, outputSize, duration.Milliseconds())
}

func (d *DB) MarkSkipped(
	ctx context.Context,
	e Entry,
	outputSize int64,
	duration time.Duration,
) error {
	return d.finish(ctx, e, `status = 'skipped', output_size = ?, duration_ms = ?, error = ''`, outputSize, duration.Milliseconds())
}

func (d *DB) MarkFailed(
	ctx context.Context,
	e Entry,
	cause error,
	duration time.Duration,
) error {
	return d.finish(ctx, e, `status = 'failed', attempts = attempts + 1, error = ?, duration_ms = ?`, cause.Error(), duration.Milliseconds())
}

// MarkGivenUp fails an entry without retries; only a new source version queues it again.
func (d *DB) MarkGivenUp(
	ctx context.Context,
	e Entry,
	cause error,
	duration time.Duration,
) error {
	return d.finish(ctx, e, `status = 'failed', attempts = ?, error = ?, duration_ms = ?`, MaxAttempts, cause.Error(), duration.Milliseconds())
}

func (d *DB) Requeue(ctx context.Context, e Entry) error {
	return d.finish(ctx, e, `status = 'queued'`)
}

func (d *DB) Counts(ctx context.Context) ([]Count, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT bucket, kind, status, COUNT(*) FROM entries
		GROUP BY bucket, kind, status ORDER BY bucket, kind, status`)
	if err != nil {
		return nil, fmt.Errorf("count entries: %w", err)
	}
	defer rows.Close()
	var counts []Count
	for rows.Next() {
		var c Count
		if err := rows.Scan(&c.Bucket, &c.Kind, &c.Status, &c.Count); err != nil {
			return nil, fmt.Errorf("scan count: %w", err)
		}
		counts = append(counts, c)
	}
	return counts, rows.Err()
}

type Filter struct {
	Bucket   string
	Kind     Kind
	Statuses []Status
}

func (f Filter) where() (string, []any) {
	clauses := []string{"1 = 1"}
	var args []any
	if f.Bucket != "" {
		clauses = append(clauses, "bucket = ?")
		args = append(args, f.Bucket)
	}
	if f.Kind != "" {
		clauses = append(clauses, "kind = ?")
		args = append(args, f.Kind)
	}
	if len(f.Statuses) > 0 {
		clauses = append(clauses, "status IN (?"+strings.Repeat(", ?", len(f.Statuses)-1)+")")
		for _, st := range f.Statuses {
			args = append(args, st)
		}
	}
	return strings.Join(clauses, " AND "), args
}

// List returns one page of the matching entries, newest-updated first, and how many match in total.
func (d *DB) List(
	ctx context.Context,
	f Filter,
	limit int,
	offset int,
) ([]Entry, int, error) {
	where, args := f.where()
	var total int
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entries WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count entries: %w", err)
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+columns+` FROM entries WHERE `+where+`
		ORDER BY updated_at DESC, bucket, key LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list entries: %w", err)
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan entry: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}

type Bytes struct {
	Before int64
	After  int64
}

// Totals sums only done entries, because only those have a variant in the bucket.
func (d *DB) Totals(ctx context.Context) (map[string]Bytes, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT bucket, SUM(source_size), SUM(output_size) FROM entries
		WHERE status = 'done' GROUP BY bucket`)
	if err != nil {
		return nil, fmt.Errorf("sum entries: %w", err)
	}
	defer rows.Close()
	totals := map[string]Bytes{}
	for rows.Next() {
		var bucket string
		var b Bytes
		if err := rows.Scan(&bucket, &b.Before, &b.After); err != nil {
			return nil, fmt.Errorf("scan totals: %w", err)
		}
		totals[bucket] = b
	}
	return totals, rows.Err()
}

func (d *DB) Ping(ctx context.Context) error {
	_, err := d.db.ExecContext(ctx, `SELECT 1 FROM entries LIMIT 1`)
	return err
}
