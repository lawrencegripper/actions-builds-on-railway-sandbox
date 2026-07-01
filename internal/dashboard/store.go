// Package dashboard records job lifecycle events to a small SQLite database and
// serves a single status/usage page from it.
package dashboard

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	result      TEXT
);
CREATE INDEX IF NOT EXISTS idx_jobs_started_at ON jobs(started_at);
`

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
	return &Store{db: db, log: log}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// JobStarted records a new running job for the runner.
func (s *Store) JobStarted(runnerName string) {
	if _, err := s.db.Exec(
		`INSERT INTO jobs (runner_name, started_at) VALUES (?, ?)`,
		runnerName, time.Now().Unix(),
	); err != nil {
		s.log.Error("record job started", slog.String("runner", runnerName), slog.String("error", err.Error()))
	}
}

// JobCompleted closes out the most recent open job for the runner.
func (s *Store) JobCompleted(runnerName, result string) {
	if _, err := s.db.Exec(
		`UPDATE jobs SET finished_at = ?, result = ?
		 WHERE id = (
			SELECT id FROM jobs
			WHERE runner_name = ? AND finished_at IS NULL
			ORDER BY started_at DESC LIMIT 1
		 )`,
		time.Now().Unix(), result, runnerName,
	); err != nil {
		s.log.Error("record job completed", slog.String("runner", runnerName), slog.String("error", err.Error()))
	}
}

// DayCount is jobs run on a single day.
type DayCount struct {
	Date  string
	Count int
}

// RecentJob is one row of the recent-jobs list.
type RecentJob struct {
	RunnerName string
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
		`SELECT runner_name, started_at, finished_at, result
		 FROM jobs ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecentJob
	for rows.Next() {
		var name string
		var started int64
		var finished sql.NullInt64
		var result sql.NullString
		if err := rows.Scan(&name, &started, &finished, &result); err != nil {
			return nil, err
		}
		j := RecentJob{
			RunnerName: name,
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
