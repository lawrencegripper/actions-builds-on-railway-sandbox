package forgejo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAPIWaitingJobsAndRegistrationToken(t *testing.T) {
	var sawLabels bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/jobs":
			sawLabels = r.URL.Query().Get("labels") == "railway-ubuntu"
			_ = json.NewEncoder(w).Encode([]Job{
				{ID: 1, Status: "waiting"},
				{ID: 2, Status: "queued"},
				{ID: 3, Status: "running"},
			})
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "short-lived"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := Config{
		APIToken:             "secret",
		JobsURL:              server.URL + "/jobs",
		RegistrationTokenURL: server.URL + "/token",
		JobLabelFilter:       "railway-ubuntu",
	}
	api := NewAPI(cfg, server.Client(), nil)
	jobs, err := api.WaitingJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].ID != 1 || jobs[1].ID != 2 {
		t.Fatalf("waiting jobs = %+v", jobs)
	}
	if !sawLabels {
		t.Fatal("jobs request did not include label filter")
	}
	token, err := api.RegistrationToken(context.Background())
	if err != nil || token != "short-lived" {
		t.Fatalf("registration token = %q, %v", token, err)
	}
}

func TestAPICompletionHydratesSuccessfulDefaultBranchJob(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repositories/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"full_name": "owner/repo", "default_branch": "main",
			})
		case "/api/v1/repos/owner/repo/actions/tasks":
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]any{
				{"id": 9, "name": "build", "head_branch": "refs/heads/main", "status": "success", "updated_at": now.Format(time.RFC3339)},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := Config{URL: server.URL, APIToken: "secret", SnapshotDefaultBranchJobs: true}
	api := NewAPI(cfg, server.Client(), nil)
	got, err := api.Completion(context.Background(), Job{ID: 7, RepoID: 42, Name: "build"}, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Hydrate || got.Outcome != "succeeded" {
		t.Fatalf("completion = %+v, want successful hydration", got)
	}
}

func TestAPIDoesNotHydrateFailedDefaultBranchJob(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repositories/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"full_name": "owner/repo", "default_branch": "main",
			})
		case "/api/v1/repos/owner/repo/actions/tasks":
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]any{
				{"id": 9, "name": "build", "head_branch": "main", "status": "failure", "updated_at": now.Format(time.RFC3339)},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := Config{URL: server.URL, APIToken: "secret", SnapshotDefaultBranchJobs: true}
	api := NewAPI(cfg, server.Client(), nil)
	got, err := api.Completion(context.Background(), Job{ID: 7, RepoID: 42, Name: "build"}, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got.Hydrate || got.Outcome != "failed" {
		t.Fatalf("completion = %+v, failed workflow must not hydrate", got)
	}
}

func TestConfigValidation(t *testing.T) {
	cfg := Config{
		URL:                       "https://forgejo.example",
		APIToken:                  "token",
		RunnerLabels:              "railway:host",
		JobLabelFilter:            "railway",
		TailscaleTailnet:          "example.ts.net",
		TailscaleOAuthClientID:    "client",
		TailscaleOAuthSecret:      "secret",
		MaxConcurrentSandboxes:    2,
		MaxLaunchPerPoll:          1,
		SnapshotDefaultBranchJobs: true,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.JobsURL == "" || cfg.PollInterval == 0 || cfg.SnapshotRefresh == 0 {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}
