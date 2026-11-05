package forgejo

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/railway"
)

type fakeForgejoAPI struct {
	jobs []Job
}

func (f *fakeForgejoAPI) WaitingJobs(context.Context) ([]Job, error) {
	return append([]Job(nil), f.jobs...), nil
}
func (f *fakeForgejoAPI) RegistrationToken(context.Context) (string, error) {
	return "registration-short", nil
}
func (f *fakeForgejoAPI) Completion(context.Context, Job, time.Time) (JobCompletion, error) {
	return JobCompletion{Outcome: "succeeded"}, nil
}

type fakeKeys struct{}

func (fakeKeys) AuthKey(context.Context) (string, error) { return "tailscale-short", nil }

type fakeImages struct{}

func (fakeImages) ResolveVersion(context.Context) (string, error)       { return "13.2.1", nil }
func (fakeImages) PrepareSandbox(context.Context, string, string) error { return nil }
func (fakeImages) PrepareBase(context.Context) (RunnerImage, error)     { return RunnerImage{}, nil }
func (fakeImages) Hydrate(context.Context, string, RunnerImage, string) (RunnerImage, error) {
	return RunnerImage{}, nil
}

type fakeProviderSink struct {
	mu        sync.Mutex
	started   int
	completed int
	outcome   string
}

func (f *fakeProviderSink) ProviderJobStarted(string, string, string, string, string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started++
}
func (f *fakeProviderSink) ProviderJobCompleted(_, _, _, _, outcome string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed++
	f.outcome = outcome
}

func TestControllerDeduplicatesQueuedJobAndKeepsSecretsOutOfCreate(t *testing.T) {
	block := make(chan struct{})
	service := &fakeSandboxService{execBlock: block}
	api := &fakeForgejoAPI{jobs: []Job{{ID: 7, RepoID: 42, Name: "build", Status: "waiting"}}}
	sink := &fakeProviderSink{}
	cfg := Config{
		URL:                       "https://forgejo.example",
		RunnerLabels:              "railway:host",
		MaxConcurrentSandboxes:    2,
		MaxLaunchPerPoll:          2,
		SandboxIdleTimeoutMinutes: 10,
		RunnerTimeout:             time.Minute,
		SnapshotBuildTimeout:      time.Minute,
	}
	controller := NewController(cfg, api, fakeKeys{}, fakeImages{}, service, railway.NetworkIsolated, sink, nil)
	base := RunnerImage{Name: "base-checkpoint", Version: "1.0.0", RefreshedAt: time.Now()}
	if err := controller.poll(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		service.mu.Lock()
		created := len(service.creates)
		service.mu.Unlock()
		if created == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := controller.poll(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	if len(service.creates) != 1 {
		t.Fatalf("created %d sandboxes for one queued job", len(service.creates))
	}
	if service.creates[0].CheckpointName != "base-checkpoint" {
		t.Fatalf("create options = %+v", service.creates[0])
	}
	if len(service.creates[0].Variables) != 0 {
		t.Fatalf("short-lived credentials persisted as sandbox variables: %#v", service.creates[0].Variables)
	}
	service.mu.Unlock()

	close(block)
	controller.wg.Wait()
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.execOptions) != 1 || string(service.execOptions[0].Stdin) != "tailscale-short\x00registration-short\x00" {
		t.Fatal("bootstrap credentials not sent through stdin")
	}
	if service.execOptions[0].Env["TS_AUTHKEY"] != "" || service.execOptions[0].Env["FORGEJO_RUNNER_REGISTRATION_TOKEN"] != "" {
		t.Fatal("bootstrap credentials exposed in exec environment")
	}
	if len(service.destroyed) != 1 {
		t.Fatalf("destroyed = %v", service.destroyed)
	}
	if controller.Snapshot().Total() != 0 {
		t.Fatalf("counts after completion = %+v", controller.Snapshot())
	}
	if sink.outcome != "succeeded" {
		t.Fatalf("dashboard outcome = %q", sink.outcome)
	}
}
