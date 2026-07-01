package dashboard

import (
	"database/sql"
	"math/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lawrencegripper/railway-sandbox-github-actions-runner/internal/scaler"
)

type fakeSnap struct{}

func (fakeSnap) Snapshot() scaler.Counts { return scaler.Counts{Provisioning: 1, Idle: 2, Busy: 3} }
func (fakeSnap) MinRunners() int         { return 1 }
func (fakeSnap) MaxRunners() int         { return 8 }

func TestStoreAndRender(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "dashboard.db") // exercises MkdirAll
	store, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// lifecycle via the sink methods
	store.JobStarted("runner-live01", "sbx-live01")
	store.JobStarted("runner-done01", "sbx-done01")
	store.JobCompleted("runner-done01", "sbx-done01", "succeeded")

	// sandbox_id round-trips through the recent-jobs query.
	recent, err := store.recent(time.Now(), 15)
	if err != nil {
		t.Fatal(err)
	}
	var sawSandbox bool
	for _, j := range recent {
		if j.RunnerName == "runner-done01" {
			if j.SandboxID != "sbx-done01" {
				t.Errorf("sandbox id = %q, want sbx-done01", j.SandboxID)
			}
			sawSandbox = true
		}
	}
	if !sawSandbox {
		t.Fatal("runner-done01 not found in recent jobs")
	}

	// template build recording + rollup.
	nowB := time.Now()
	if _, err := store.RecordTemplateBuild(TemplateBuild{
		BuildID: "hash-ok", RunnerVersion: "2.320.0", Recipe: "apt-get update",
		Status: "READY", Started: nowB.Add(-time.Minute), Finished: nowB,
	}); err != nil {
		t.Fatal(err)
	}
	failID, err := store.RecordTemplateBuild(TemplateBuild{
		BuildID: "hash-bad", RunnerVersion: "2.320.0", Recipe: "bad step",
		Status: "FAILED", Error: "apt failed", Started: nowB.Add(-2 * time.Minute), Finished: nowB.Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	builds, err := store.TemplateBuilds(nowB, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(builds) != 2 {
		t.Fatalf("want 2 builds, got %d", len(builds))
	}
	one, err := store.TemplateBuildByID(nowB, failID)
	if err != nil {
		t.Fatal(err)
	}
	if one == nil || one.Status != "FAILED" || one.Error != "apt failed" {
		t.Fatalf("build by id = %+v", one)
	}
	if missing, err := store.TemplateBuildByID(nowB, 99999); err != nil || missing != nil {
		t.Fatalf("missing build lookup = %+v, %v", missing, err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	names := []string{"runner-a1b2c3", "runner-d4e5f6", "runner-99aa11", "runner-7c2f0e", "runner-bb44cc"}
	results := []string{"succeeded", "succeeded", "succeeded", "failed", "cancelled"}
	now := time.Now()
	rng := rand.New(rand.NewSource(42))
	for d := 6; d >= 0; d-- {
		n := rng.Intn(6) + (6 - d)
		for i := 0; i < n; i++ {
			start := now.AddDate(0, 0, -d).Add(-time.Duration(rng.Intn(8)) * time.Hour)
			dur := time.Duration(20+rng.Intn(600)) * time.Second
			res := results[rng.Intn(len(results))]
			name := names[rng.Intn(len(names))]
			if _, err := db.Exec(
				`INSERT INTO jobs (runner_name, sandbox_id, started_at, finished_at, result) VALUES (?,?,?,?,?)`,
				name, "sbx-"+name[7:], start.Unix(), start.Add(dur).Unix(), res); err != nil {
				t.Fatal(err)
			}
		}
	}

	u, err := store.Usage(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.PerDay) != 7 {
		t.Fatalf("want 7 day buckets, got %d", len(u.PerDay))
	}
	if u.Total == 0 || u.Succeeded == 0 || u.Failed == 0 {
		t.Fatalf("expected non-zero totals, got %+v", u)
	}

	srv := NewServer(store, fakeSnap{}, "railway-runners", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"railway-runners", "Provisioning", "Recent jobs", "running", "succeeded", "Template builds", "READY", "FAILED", "sbx-done01"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}

	recB := httptest.NewRecorder()
	srv.Routes().ServeHTTP(recB, httptest.NewRequest("GET", "/builds/"+strconv.FormatInt(failID, 10), nil))
	if recB.Code != 200 {
		t.Fatalf("build page status %d", recB.Code)
	}
	buildBody := recB.Body.String()
	for _, want := range []string{"apt failed", "bad step", "does not expose per-line"} {
		if !strings.Contains(buildBody, want) {
			t.Errorf("build page missing %q", want)
		}
	}
	if out := os.Getenv("BUILD_HTML_OUT"); out != "" {
		if err := os.WriteFile(out, recB.Body.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if out := os.Getenv("DASHBOARD_HTML_OUT"); out != "" {
		if err := os.WriteFile(out, rec.Body.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", out, rec.Body.Len())
	}

	rec2 := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec2, httptest.NewRequest("GET", "/healthz", nil))
	if rec2.Code != 200 || rec2.Body.String() != "ok" {
		t.Fatalf("healthz: %d %q", rec2.Code, rec2.Body.String())
	}
}
