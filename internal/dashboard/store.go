// Package dashboard records job lifecycle events to a small SQLite database and
// serves a single status/usage page from it.
package dashboard

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store persists job lifecycle events and answers the dashboard's queries. It
// satisfies scaler.EventSink.
type Store struct {
	db  *sql.DB
	log *slog.Logger
}

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	runner_name TEXT    NOT NULL,
	started_at  INTEGER NOT NULL,
	finished_at INTEGER,
	result      TEXT,
	sandbox_id  TEXT
);
CREATE INDEX IF NOT EXISTS idx_jobs_started_at ON jobs(started_at);

CREATE TABLE IF NOT EXISTS template_builds (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	build_id       TEXT,
	runner_version TEXT,
	recipe         TEXT,
	status         TEXT    NOT NULL,
	error          TEXT,
	logs           TEXT,
	started_at     INTEGER NOT NULL,
	finished_at    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_builds_started_at ON template_builds(started_at);
`

// migrations bring an older on-disk schema up to date. Idempotent: each ALTER is
// tolerated to fail when the column already exists.
var migrations = []string{
	`ALTER TABLE jobs ADD COLUMN sandbox_id TEXT`,
}

// DBPath resolves where the SQLite file lives: on a Railway volume when
// RAILWAY_VOLUME_MOUNT_PATH is set, otherwise a local ./data dir for dev.
func DBPath() string {
	if v := os.Getenv("RAILWAY_VOLUME_MOUNT_PATH"); v != "" {
		return filepath.Join(v, "dashboard.db")
	}
	return filepath.Join("data", "dashboard.db")
}

// Open opens (creating if needed) the SQLite database at path and migrates the
// schema.
func Open(path string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1) // one writer; keeps a file-backed SQLite simple
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return &Store{db: db, log: log}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// JobStarted records a new running job for the runner, tagged with the sandbox
// it runs in.
func (s *Store) JobStarted(runnerName, sandboxID string) {
	if _, err := s.db.Exec(
		`INSERT INTO jobs (runner_name, sandbox_id, started_at) VALUES (?, ?, ?)`,
		runnerName, nullable(sandboxID), time.Now().Unix(),
	); err != nil {
		s.log.Error("record job started", slog.String("runner", runnerName), slog.String("error", err.Error()))
	}
}

// JobCompleted closes out the most recent open job for the runner. The sandbox
// id backfills any row that started before the sandbox was known.
func (s *Store) JobCompleted(runnerName, sandboxID, result string) {
	if _, err := s.db.Exec(
		`UPDATE jobs SET finished_at = ?, result = ?, sandbox_id = COALESCE(sandbox_id, ?)
		 WHERE id = (
			SELECT id FROM jobs
			WHERE runner_name = ? AND finished_at IS NULL
			ORDER BY started_at DESC LIMIT 1
		 )`,
		time.Now().Unix(), result, nullable(sandboxID), runnerName,
	); err != nil {
		s.log.Error("record job completed", slog.String("runner", runnerName), slog.String("error", err.Error()))
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// DayCount is jobs run on a single day.
type DayCount struct {
	Date  string
	Count int
}

// RecentJob is one row of the recent-jobs list.
type RecentJob struct {
	RunnerName string
	SandboxID  string
	Started    time.Time
	Duration   time.Duration
	Running    bool
	Result     string
}

// Usage is the 7-day rollup shown on the page.
type Usage struct {
	PerDay        []DayCount
	Total         int
	Succeeded     int
	Failed        int
	AvgDuration   time.Duration
	TotalDuration time.Duration
	Recent        []RecentJob
}

// Usage computes the rollup over the last 7 days (including today).
func (s *Store) Usage(now time.Time) (Usage, error) {
	var u Usage
	days := 7
	start := now.AddDate(0, 0, -(days - 1))
	startDay := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())

	counts := map[string]int{}
	for d := 0; d < days; d++ {
		counts[startDay.AddDate(0, 0, d).Format("2006-01-02")] = 0
	}

	rows, err := s.db.Query(
		`SELECT started_at, finished_at, result FROM jobs WHERE started_at >= ?`,
		startDay.Unix(),
	)
	if err != nil {
		return u, err
	}
	defer rows.Close()
	for rows.Next() {
		var started int64
		var finished sql.NullInt64
		var result sql.NullString
		if err := rows.Scan(&started, &finished, &result); err != nil {
			return u, err
		}
		day := time.Unix(started, 0).In(now.Location()).Format("2006-01-02")
		if _, ok := counts[day]; ok {
			counts[day]++
		}
		u.Total++
		if finished.Valid {
			u.TotalDuration += time.Duration(finished.Int64-started) * time.Second
		}
		switch result.String {
		case "succeeded":
			u.Succeeded++
		case "failed", "cancelled", "canceled", "abandoned":
			u.Failed++
		}
	}
	if err := rows.Err(); err != nil {
		return u, err
	}

	for d := 0; d < days; d++ {
		day := startDay.AddDate(0, 0, d).Format("2006-01-02")
		u.PerDay = append(u.PerDay, DayCount{Date: day, Count: counts[day]})
	}
	if finishedCount := u.Succeeded + u.Failed; finishedCount > 0 {
		u.AvgDuration = u.TotalDuration / time.Duration(finishedCount)
	}

	recent, err := s.recent(now, 15)
	if err != nil {
		return u, err
	}
	u.Recent = recent
	return u, nil
}

func (s *Store) recent(now time.Time, limit int) ([]RecentJob, error) {
	rows, err := s.db.Query(
		`SELECT runner_name, sandbox_id, started_at, finished_at, result
		 FROM jobs ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecentJob
	for rows.Next() {
		var name string
		var sandbox sql.NullString
		var started int64
		var finished sql.NullInt64
		var result sql.NullString
		if err := rows.Scan(&name, &sandbox, &started, &finished, &result); err != nil {
			return nil, err
		}
		j := RecentJob{
			RunnerName: name,
			SandboxID:  sandbox.String,
			Started:    time.Unix(started, 0).In(now.Location()),
			Result:     result.String,
		}
		if finished.Valid {
			j.Duration = time.Duration(finished.Int64-started) * time.Second
		} else {
			j.Running = true
			j.Result = "running"
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// TemplateBuild is one recorded attempt to build the runner image template.
type TemplateBuild struct {
	ID            int64
	BuildID       string
	RunnerVersion string
	Recipe        string
	Status        string
	Error         string
	Logs          string
	Started       time.Time
	Finished      time.Time
	Running       bool
	Duration      time.Duration
}

// RecordTemplateBuild inserts a finished (or failed) build attempt and returns
// its row id. buildID is Railway's content-addressed build id; recipe is the
// build instructions; logs is the per-line build log when the API exposes it
// (currently empty, the API does not surface it).
func (s *Store) RecordTemplateBuild(b TemplateBuild) (int64, error) {
	var finished any
	if !b.Finished.IsZero() {
		finished = b.Finished.Unix()
	}
	res, err := s.db.Exec(
		`INSERT INTO template_builds (build_id, runner_version, recipe, status, error, logs, started_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		nullable(b.BuildID), nullable(b.RunnerVersion), nullable(b.Recipe),
		b.Status, nullable(b.Error), nullable(b.Logs), b.Started.Unix(), finished,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// TemplateBuilds returns the most recent build attempts, newest first.
func (s *Store) TemplateBuilds(now time.Time, limit int) ([]TemplateBuild, error) {
	rows, err := s.db.Query(
		`SELECT id, build_id, runner_version, recipe, status, error, logs, started_at, finished_at
		 FROM template_builds ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TemplateBuild
	for rows.Next() {
		b, err := scanBuild(rows, now)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// TemplateBuildByID returns a single build attempt by row id.
func (s *Store) TemplateBuildByID(now time.Time, id int64) (*TemplateBuild, error) {
	row := s.db.QueryRow(
		`SELECT id, build_id, runner_version, recipe, status, error, logs, started_at, finished_at
		 FROM template_builds WHERE id = ?`, id)
	b, err := scanBuild(row, now)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanBuild(row scanner, now time.Time) (TemplateBuild, error) {
	var b TemplateBuild
	var buildID, version, recipe, status, errText, logs sql.NullString
	var started int64
	var finished sql.NullInt64
	if err := row.Scan(&b.ID, &buildID, &version, &recipe, &status, &errText, &logs, &started, &finished); err != nil {
		return b, err
	}
	b.BuildID = buildID.String
	b.RunnerVersion = version.String
	b.Recipe = recipe.String
	b.Status = status.String
	b.Error = errText.String
	b.Logs = logs.String
	b.Started = time.Unix(started, 0).In(now.Location())
	if finished.Valid {
		b.Finished = time.Unix(finished.Int64, 0).In(now.Location())
		b.Duration = time.Duration(finished.Int64-started) * time.Second
	} else {
		b.Running = true
	}
	return b, nil
}
