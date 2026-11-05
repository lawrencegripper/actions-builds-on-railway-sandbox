package forgejo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/railway"
)

const testPolicy = `{"repository":"lawrencegripper/homenet","ref":"refs/heads/master","event":"push","workflow":"build-k3s.yaml","job_name":"deploy-k3s"}`
const testHandle = "8da61b7b-191c-4364-8ad2-94c4ab1e7bf8"

func restrictedConfig(t *testing.T, serverURL string) Config {
	t.Helper()
	t.Setenv("FORGEJO_RESTRICTED_POLICY", testPolicy)
	cfg := Config{URL: serverURL, APIToken: "admin-controller-only", RunnerVersion: "13.2.1", SnapshotNamePrefix: "restricted-homenet-deploy", TailscaleTailnet: "example.ts.net", TailscaleAPIToken: "tailscale-controller-only", RunnerTimeout: time.Second}
	if err := cfg.ApplyEnv(func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	return cfg
}

type countingKeys struct{ calls int }

func (k *countingKeys) AuthKey(context.Context) (string, error) { k.calls++; return "short-key", nil }

type restrictedFixture struct {
	job           map[string]any
	run           map[string]any
	jobs          []map[string]any
	ref           string
	version       string
	failPath      string
	changeHandle  bool
	changeAttempt bool
	changeJobID   bool
	detailCalls   int
	tokenCalls    int
}

func newRestrictedFixture() *restrictedFixture {
	job := map[string]any{"id": float64(7), "repo_id": float64(42), "run_id": float64(123), "attempt": float64(1), "handle": testHandle, "name": "deploy-k3s", "status": "waiting"}
	return &restrictedFixture{job: job, jobs: []map[string]any{job}, ref: "refs/heads/master", version: "17.0.1", run: map[string]any{
		"id": float64(123), "index_in_repo": float64(9), "repository": map[string]any{"id": float64(42), "full_name": "lawrencegripper/homenet", "fork": false},
		"workflow_id": "build-k3s.yaml", "event": "push", "trigger_event": "push", "prettyref": "master", "need_approval": false, "is_fork_pull_request": false, "status": "running",
	}}
}

func (f *restrictedFixture) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == f.failPath {
		http.Error(w, "unavailable", 503)
		return
	}
	var response any
	const repo = "/api/v1/repos/lawrencegripper/homenet"
	switch r.URL.Path {
	case "/api/v1/version":
		response = map[string]string{"version": f.version}
	case "/api/v1/admin/actions/runners/jobs":
		response = []any{f.job}
	case repo + "/actions/runs/123":
		response = f.run
	case repo + "/actions/runs":
		q := r.URL.Query()
		entries := []any{}
		if q.Get("ref") == f.ref && q.Get("event") == f.run["event"] && q.Get("workflow_id") == f.run["workflow_id"] && q.Get("run_number") == "9" && q.Get("run_id") == "" {
			entries = append(entries, f.run)
		}
		response = map[string]any{"workflow_runs": entries, "total_count": len(entries)}
	case repo + "/actions/runs/123/jobs":
		response = f.jobs
	case repo + "/actions/jobs/7":
		f.detailCalls++
		copy := map[string]any{}
		for k, v := range f.job {
			copy[k] = v
		}
		if f.changeHandle && f.detailCalls >= 2 {
			copy["handle"] = "rerun-handle"
			copy["attempt"] = 2
		}
		if f.changeAttempt && f.detailCalls >= 2 {
			copy["attempt"] = 2
		}
		if f.changeJobID && f.detailCalls >= 2 {
			copy["id"] = 8
		}
		response = copy
	case repo + "/actions/runners/registration-token":
		f.tokenCalls++
		response = map[string]string{"token": "repo-registration"}
	case "/api/v1/admin/actions/runners/registration-token":
		f.tokenCalls++
		response = map[string]string{"token": "instance-registration"}
	default:
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(response)
}

func TestRestrictedAuthorizationBeforeCredentials(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*restrictedFixture)
		allowed bool
	}{
		{"allowed", func(*restrictedFixture) {}, true},
		{"release version suffix", func(f *restrictedFixture) { f.version = "17.0.1+gitea-1.22.0" }, true},
		{"wrong repository", func(f *restrictedFixture) { f.run["repository"].(map[string]any)["full_name"] = "attacker/homenet" }, false},
		{"wrong repository ID", func(f *restrictedFixture) { f.run["repository"].(map[string]any)["id"] = 99 }, false},
		{"wrong branch same label", func(f *restrictedFixture) { f.ref = "refs/heads/untrusted" }, false},
		{"tag named master", func(f *restrictedFixture) { f.ref = "refs/tags/master" }, false},
		{"wrong event", func(f *restrictedFixture) { f.run["event"] = "pull_request" }, false},
		{"wrong trigger", func(f *restrictedFixture) { f.run["trigger_event"] = "pull_request_target" }, false},
		{"wrong workflow", func(f *restrictedFixture) { f.run["workflow_id"] = "untrusted.yaml" }, false},
		{"wrong name", func(f *restrictedFixture) { f.job["name"] = "untrusted" }, false},
		{"duplicate names", func(f *restrictedFixture) {
			j := map[string]any{}
			for k, v := range f.job {
				j[k] = v
			}
			j["id"] = 8
			f.jobs = append(f.jobs, j)
		}, false},
		{"matrix name", func(f *restrictedFixture) { f.job["name"] = "deploy-k3s (linux)" }, false},
		{"missing run ID", func(f *restrictedFixture) { delete(f.job, "run_id") }, false},
		{"missing attempt", func(f *restrictedFixture) { delete(f.job, "attempt") }, false},
		{"missing handle", func(f *restrictedFixture) { delete(f.job, "handle") }, false},
		{"invalid handle", func(f *restrictedFixture) { f.job["handle"] = "not-a-handle" }, false},
		{"zero attempt", func(f *restrictedFixture) { f.job["attempt"] = 0 }, false},
		{"missing repository fork metadata", func(f *restrictedFixture) { delete(f.run["repository"].(map[string]any), "fork") }, false},
		{"unknown run status", func(f *restrictedFixture) { f.run["status"] = "unknown" }, false},
		{"missing approval metadata", func(f *restrictedFixture) { delete(f.run, "need_approval") }, false},
		{"missing fork metadata", func(f *restrictedFixture) { delete(f.run, "is_fork_pull_request") }, false},
		{"approval needed", func(f *restrictedFixture) { f.run["need_approval"] = true }, false},
		{"fork PR", func(f *restrictedFixture) { f.run["is_fork_pull_request"] = true }, false},
		{"fork repo", func(f *restrictedFixture) { f.run["repository"].(map[string]any)["fork"] = true }, false},
		{"missing run number", func(f *restrictedFixture) { delete(f.run, "index_in_repo") }, false},
		{"different run", func(f *restrictedFixture) { f.run["id"] = 124 }, false},
		{"empty jobs", func(f *restrictedFixture) { f.jobs = nil }, false},
		{"old server", func(f *restrictedFixture) { f.version = "15.0.3" }, false},
		{"unknown server", func(f *restrictedFixture) { f.version = "18.0.0" }, false},
		{"API failure", func(f *restrictedFixture) { f.failPath = "/api/v1/repos/lawrencegripper/homenet/actions/runs/123/jobs" }, false},
		{"version API failure", func(f *restrictedFixture) { f.failPath = "/api/v1/version" }, false},
		{"run API failure", func(f *restrictedFixture) { f.failPath = "/api/v1/repos/lawrencegripper/homenet/actions/runs/123" }, false},
		{"ref filter API failure", func(f *restrictedFixture) { f.failPath = "/api/v1/repos/lawrencegripper/homenet/actions/runs" }, false},
		{"job detail API failure", func(f *restrictedFixture) { f.failPath = "/api/v1/repos/lawrencegripper/homenet/actions/jobs/7" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newRestrictedFixture()
			tt.mutate(fixture)
			server := httptest.NewServer(http.HandlerFunc(fixture.serve))
			defer server.Close()
			cfg := restrictedConfig(t, server.URL)
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			service := &fakeSandboxService{}
			keys := &countingKeys{}
			controller := NewController(cfg, NewAPI(cfg, server.Client(), nil), keys, fakeImages{}, service, railway.NetworkIsolated, nil, nil)
			raw, _ := json.Marshal(fixture.job)
			var job Job
			if err := json.Unmarshal(raw, &job); err != nil {
				t.Fatal(err)
			}
			_, hydrated := controller.runOne(context.Background(), job, 1, "unused", RunnerImage{Name: "trusted-base", Version: "13.2.1", Kind: "base"})
			if hydrated {
				t.Fatal("restricted job hydrated a snapshot")
			}
			if !tt.allowed {
				if keys.calls != 0 || fixture.tokenCalls != 0 || len(service.creates) != 0 {
					t.Fatalf("unauthorized job obtained credentials/sandbox: keys=%d tokens=%d creates=%d", keys.calls, fixture.tokenCalls, len(service.creates))
				}
				return
			}
			if len(service.execOptions) != 1 {
				t.Fatalf("exec count = %d", len(service.execOptions))
			}
			env := service.execOptions[0].Env
			if env["FORGEJO_JOB_HANDLE"] != testHandle || env["FORGEJO_RUNNER_RESTRICTED"] != "true" || string(service.execOptions[0].Stdin) != "short-key\x00repo-registration\x00" {
				t.Fatalf("runner not bound and repository scoped: %#v", env)
			}
			if len(service.destroyed) != 1 {
				t.Fatal("sandbox not destroyed")
			}
		})
	}
}

func TestRestrictedRejectsInvalidConfig(t *testing.T) {
	for _, tc := range []struct {
		name, policy string
		edit         func(*Config)
	}{
		{"invalid JSON", `{`, func(*Config) {}},
		{"unknown key", strings.TrimSuffix(testPolicy, "}") + `,"job_key":"deploy-k3s"}`, func(*Config) {}},
		{"null policy", `null`, func(*Config) {}},
		{"missing tuple", `{}`, func(*Config) {}},
		{"unqualified ref", strings.Replace(testPolicy, "refs/heads/master", "master", 1), func(*Config) {}},
		{"matrix selector", strings.Replace(testPolicy, "deploy-k3s", "deploy-k3s (linux)", 1), func(*Config) {}},
		{"hydration", testPolicy, func(c *Config) { c.SnapshotDefaultBranchJobs = true }},
		{"shared prefix", testPolicy, func(c *Config) { c.SnapshotNamePrefix = "forgejo-runner" }},
		{"latest runner", testPolicy, func(c *Config) { c.RunnerVersion = "latest" }},
		{"old runner", testPolicy, func(c *Config) { c.RunnerVersion = "11.0.1" }},
		{"empty policy", "", func(*Config) {}},
		{"second JSON object", testPolicy + testPolicy, func(*Config) {}},
		{"unsupported event", strings.Replace(testPolicy, "push", "pull_request_target", 1), func(*Config) {}},
		{"jobs endpoint override", testPolicy, func(c *Config) { c.JobsURL = "https://other.example/jobs" }},
		{"token endpoint override", testPolicy, func(c *Config) { c.RegistrationTokenURL = "https://other.example/token" }},
		{"ambiguous prefix", testPolicy, func(c *Config) { c.SnapshotNamePrefix = "restricted-/shared" }},
		{"matrix wildcard", strings.Replace(testPolicy, "deploy-k3s", "deploy-*", 1), func(*Config) {}},
		{"tag policy", strings.Replace(testPolicy, "refs/heads/master", "refs/tags/master", 1), func(*Config) {}},
		{"workflow path", strings.Replace(testPolicy, "build-k3s.yaml", ".forgejo/workflows/build-k3s.yaml", 1), func(*Config) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := restrictedConfig(t, "https://forgejo.example")
			t.Setenv("FORGEJO_RESTRICTED_POLICY", tc.policy)
			err := cfg.ApplyEnv(func(string) bool { return false })
			tc.edit(&cfg)
			if err == nil {
				err = cfg.Validate()
			}
			if err == nil {
				t.Fatal("unsafe restricted configuration accepted")
			}
		})
	}
}

func TestRestrictedStartScriptRequiresHandle(t *testing.T) {
	if !strings.Contains(forgejoRunnerStartScript, `forgejo-runner one-job --handle "$FORGEJO_JOB_HANDLE"`) || !strings.Contains(forgejoRunnerStartScript, `${FORGEJO_JOB_HANDLE:?`) {
		t.Fatal("restricted start script has no mandatory bound one-job invocation")
	}
}

func TestRestrictedRechecksAttemptBeforeExec(t *testing.T) {
	for _, change := range []string{"handle", "attempt", "job ID"} {
		t.Run(change, func(t *testing.T) {
			fixture := newRestrictedFixture()
			fixture.changeHandle = change == "handle"
			fixture.changeAttempt = change == "attempt"
			fixture.changeJobID = change == "job ID"
			server := httptest.NewServer(http.HandlerFunc(fixture.serve))
			defer server.Close()
			cfg := restrictedConfig(t, server.URL)
			service := &fakeSandboxService{}
			keys := &countingKeys{}
			controller := NewController(cfg, NewAPI(cfg, server.Client(), nil), keys, fakeImages{}, service, railway.NetworkIsolated, nil, nil)
			raw, _ := json.Marshal(fixture.job)
			var job Job
			_ = json.Unmarshal(raw, &job)
			controller.runOne(context.Background(), job, 1, "unused", RunnerImage{Name: "trusted-base", Version: "13.2.1", Kind: "base"})
			if len(service.creates) != 1 || len(service.destroyed) != 1 || len(service.execs) != 0 {
				t.Fatalf("changed attempt launched or leaked sandbox: creates=%d destroyed=%d execs=%d", len(service.creates), len(service.destroyed), len(service.execs))
			}
		})
	}
}

func TestRestrictedNeverUsesHydratedCache(t *testing.T) {
	fixture := newRestrictedFixture()
	server := httptest.NewServer(http.HandlerFunc(fixture.serve))
	defer server.Close()
	cfg := restrictedConfig(t, server.URL)
	service := &fakeSandboxService{}
	keys := &countingKeys{}
	controller := NewController(cfg, NewAPI(cfg, server.Client(), nil), keys, fakeImages{}, service, railway.NetworkIsolated, nil, nil)
	var job Job
	raw, _ := json.Marshal(fixture.job)
	_ = json.Unmarshal(raw, &job)
	poisoned := RunnerImage{Name: "untrusted-cache", Kind: "hydrated-build", Version: "13.2.1"}
	controller.jobImages[snapshotKey(job)] = poisoned
	controller.runOne(context.Background(), job, 1, "unused", poisoned)
	if keys.calls != 0 || len(service.creates) != 0 {
		t.Fatal("restricted runner accepted hydrated image")
	}
	if err := controller.poll(context.Background(), RunnerImage{Name: "trusted-base", Kind: "base", Version: "13.2.1"}); err != nil {
		t.Fatal(err)
	}
	controller.wg.Wait()
	if len(service.creates) != 1 || service.creates[0].CheckpointName != "trusted-base" {
		t.Fatalf("restricted cache selection = %+v", service.creates)
	}
	manager := NewImageManager(cfg, service, railway.NetworkIsolated, nil, nil)
	if _, err := manager.Hydrate(context.Background(), "sbx", poisoned, "job"); err == nil || len(service.checkpoints) != 0 {
		t.Fatal("restricted image manager allowed hydration")
	}
}

func TestRestrictedPollSkipsUnauthorizedSameLabelJob(t *testing.T) {
	fixture := newRestrictedFixture()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/admin/actions/runners/jobs" {
			unauthorized := map[string]any{}
			for k, v := range fixture.job {
				unauthorized[k] = v
			}
			unauthorized["id"] = 8
			unauthorized["run_id"] = 124
			_ = json.NewEncoder(w).Encode([]any{unauthorized, fixture.job})
			return
		}
		fixture.serve(w, r)
	}))
	defer server.Close()
	cfg := restrictedConfig(t, server.URL)
	cfg.MaxLaunchPerPoll = 1
	service := &fakeSandboxService{}
	keys := &countingKeys{}
	controller := NewController(cfg, NewAPI(cfg, server.Client(), nil), keys, fakeImages{}, service, railway.NetworkIsolated, nil, nil)
	if err := controller.poll(context.Background(), RunnerImage{Name: "trusted-base", Kind: "base", Version: "13.2.1"}); err != nil {
		t.Fatal(err)
	}
	controller.wg.Wait()
	if keys.calls != 1 || len(service.execOptions) != 1 || service.execOptions[0].Env["FORGEJO_JOB_HANDLE"] != testHandle {
		t.Fatal("same-label unauthorized job blocked or replaced the authorized attempt")
	}
}

func TestRunnerScriptBindingAndNoFallback(t *testing.T) {
	for _, tc := range []struct {
		name, restricted, handle, version string
		failFetch, wantError              bool
		invocation                        string
	}{
		{"bound", "true", testHandle, "13.2.1", false, false, "one-job --handle " + testHandle},
		{"failed bound fetch", "true", testHandle, "13.2.1", true, true, "one-job --handle " + testHandle},
		{"missing handle", "true", "", "13.2.1", false, true, ""},
		{"incompatible runner", "true", testHandle, "11.0.1", false, true, ""},
		{"legacy", "false", "", "11.0.1", false, false, "one-job"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, "calls")
			stub := `#!/bin/bash
name="${0##*/}"
case "$name" in
  forgejo-runner)
    if [[ "$1" == --version ]]; then echo "forgejo-runner version v$TEST_VERSION"; exit 0; fi
    printf '%s\n' "$*" >> "$TEST_CALLS"
    if [[ "$1" == one-job && "$TEST_FAIL_FETCH" == true ]]; then exit 1; fi
    ;;
  seq) echo 1 ;;
esac
`
			for _, name := range []string{"forgejo-runner", "mkdir", "chmod", "docker", "tailscale", "tailscaled", "seq", "curl"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0700); err != nil {
					t.Fatal(err)
				}
			}
			scriptPath := filepath.Join(dir, "start-forgejo-runner")
			if err := os.WriteFile(scriptPath, []byte(forgejoRunnerStartScript), 0700); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("bash", scriptPath)
			command.Stdin = strings.NewReader("short-key\x00repo-token\x00")
			command.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "FORGEJO_URL=https://forgejo.example", "FORGEJO_RUNNER_LABELS=deploy:host", "RUNNER_NAME=test-runner", "FORGEJO_RUNNER_RESTRICTED=" + tc.restricted, "FORGEJO_JOB_HANDLE=" + tc.handle, "TEST_VERSION=" + tc.version, "TEST_CALLS=" + logPath}
			if tc.failFetch {
				command.Env = append(command.Env, "TEST_FAIL_FETCH=true")
			}
			output, err := command.CombinedOutput()
			if (err != nil) != tc.wantError {
				t.Fatalf("script error=%v output=%s", err, output)
			}
			calls, _ := os.ReadFile(logPath)
			if tc.invocation == "" {
				if len(calls) != 0 {
					t.Fatalf("invalid runner registered or fetched: %s", calls)
				}
				return
			}
			lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
			if len(lines) != 2 || lines[1] != tc.invocation || !strings.Contains(lines[0], "--ephemeral") {
				t.Fatalf("runner invocations = %q", lines)
			}
		})
	}
}
