package forgejo

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/railway"
)

func TestColdSetupSkipsSnapshotsAndCacheWarmup(t *testing.T) {
	service := &fakeSandboxService{}
	cfg := Config{RunnerVersion: "13.2.1", SnapshotBuildTimeout: time.Second,
		ExtraPackages: []string{"ripgrep"}, ExtraSetup: "echo extra-setup",
		PreloadDockerImages: []string{"unused:image"}, DockerWarmupSetup: "echo optional-warmup"}
	manager := NewImageManager(cfg, service, railway.NetworkIsolated, nil, nil)
	if err := manager.PrepareSandbox(context.Background(), "cold", "13.2.1"); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(service.execs, "\n")
	for _, required := range []string{"ripgrep", "tailscale.com/install.sh", "forgejo-runner-13.2.1-linux", "extra-setup", "start-forgejo-runner"} {
		if !strings.Contains(commands, required) {
			t.Errorf("missing cold setup: %s", required)
		}
	}
	if strings.Contains(commands, "unused:image") || strings.Contains(commands, "optional-warmup") || len(service.checkpoints) != 0 {
		t.Fatal("cold setup required a cache or snapshot")
	}
}

type coldImages struct {
	fakeImages
	setupErr error
	setups   int
}

func (f *coldImages) PrepareSandbox(context.Context, string, string) error {
	f.setups++
	return f.setupErr
}

type expiredCheckpoint struct{ fakeSandboxService }

func (f *expiredCheckpoint) CreateSandbox(ctx context.Context, options railway.CreateOptions) (*railway.Sandbox, error) {
	if options.CheckpointName != "" {
		return nil, errors.New("checkpoint unavailable")
	}
	return f.fakeSandboxService.CreateSandbox(ctx, options)
}

func TestColdSetupFailureCleansUpWithoutIssuingCredentials(t *testing.T) {
	service := &fakeSandboxService{}
	keys := &countingKeys{}
	images := &coldImages{setupErr: errors.New("setup failed")}
	controller := NewController(Config{SnapshotBuildTimeout: time.Second}, &fakeForgejoAPI{}, keys, images, service, railway.NetworkIsolated, nil, nil)
	controller.runOne(context.Background(), Job{ID: 7}, 1, "job", RunnerImage{Version: "13.2.1", Kind: "base"})
	if keys.calls != 0 || images.setups != 1 || len(service.destroyed) != 1 || len(service.execs) != 0 {
		t.Fatalf("failed setup: keys=%d setups=%d cleanup=%v", keys.calls, images.setups, service.destroyed)
	}
}

func TestCheckpointRestoreFailureFallsBackToColdRunner(t *testing.T) {
	service := &expiredCheckpoint{}
	images := &coldImages{}
	controller := NewController(Config{SnapshotBuildTimeout: time.Second, RunnerTimeout: time.Second}, &fakeForgejoAPI{}, fakeKeys{}, images, service, railway.NetworkIsolated, nil, nil)
	controller.runOne(context.Background(), Job{ID: 7}, 1, "job", RunnerImage{Name: "missing", Version: "13.2.1", Kind: "base"})
	if images.setups != 1 || len(service.execOptions) != 1 || len(service.destroyed) != 1 {
		t.Fatal("checkpoint failure did not run and clean up a cold job")
	}
	if string(service.execOptions[0].Stdin) != "tailscale-short\x00registration-short\x00" {
		t.Fatal("cold job did not retain stdin-only bootstrap")
	}
}

type hydratingAPI struct{ fakeForgejoAPI }

func (f *hydratingAPI) Completion(context.Context, Job, time.Time) (JobCompletion, error) {
	return JobCompletion{Outcome: "succeeded", Hydrate: true}, nil
}

type hangingHydration struct {
	fakeImages
	started chan struct{}
}

func (f *hangingHydration) Hydrate(ctx context.Context, _ string, _ RunnerImage, _ string) (RunnerImage, error) {
	close(f.started)
	<-ctx.Done()
	return RunnerImage{}, ctx.Err()
}

func TestHydrationDoesNotReserveJobCapacity(t *testing.T) {
	images := &hangingHydration{started: make(chan struct{})}
	api := &hydratingAPI{fakeForgejoAPI{jobs: []Job{{ID: 7}}}}
	service := &fakeSandboxService{}
	cfg := Config{MaxConcurrentSandboxes: 1, MaxLaunchPerPoll: 1, RunnerTimeout: time.Second, SnapshotBuildTimeout: time.Second}
	controller := NewController(cfg, api, fakeKeys{}, images, service, railway.NetworkIsolated, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := RunnerImage{Name: "base", Version: "13.2.1"}
	if err := controller.poll(ctx, base); err != nil {
		t.Fatal(err)
	}
	select {
	case <-images.started:
	case <-time.After(time.Second):
		t.Fatal("snapshot never started")
	}
	deadline := time.Now().Add(time.Second)
	for controller.Snapshot().Total() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	api.jobs = []Job{{ID: 8}}
	if err := controller.poll(ctx, base); err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline) {
		service.mu.Lock()
		created := len(service.creates)
		service.mu.Unlock()
		if created == 2 && controller.Snapshot().Total() == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	controller.wg.Wait()
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.creates) != 2 || len(service.destroyed) != 2 || controller.Snapshot().Total() != 0 {
		t.Fatalf("snapshot blocked dispatch or cleanup: creates=%d destroyed=%d counts=%+v", len(service.creates), len(service.destroyed), controller.Snapshot())
	}
}

type refreshingImages struct {
	fakeImages
	calls atomic.Int32
}

func (f *refreshingImages) PrepareBase(ctx context.Context) (RunnerImage, error) {
	if f.calls.Add(1) == 1 {
		return RunnerImage{Name: "ready", Version: "13.2.1", Kind: "base", RefreshedAt: time.Now()}, nil
	}
	<-ctx.Done()
	return RunnerImage{}, ctx.Err()
}

type refreshAPI struct {
	fakeForgejoAPI
	images *refreshingImages
}

func (f *refreshAPI) WaitingJobs(context.Context) ([]Job, error) {
	if f.images.calls.Load() < 2 {
		return nil, nil
	}
	return []Job{{ID: 7}}, nil
}

func TestHangingRefreshKeepsExistingBaseUsable(t *testing.T) {
	images := &refreshingImages{}
	service := &fakeSandboxService{execBlock: make(chan struct{})}
	cfg := Config{PollInterval: time.Millisecond, SnapshotRefresh: 5 * time.Millisecond, SnapshotBuildTimeout: time.Second,
		RunnerTimeout: time.Second, MaxConcurrentSandboxes: 1, MaxLaunchPerPoll: 1}
	controller := NewController(cfg, &refreshAPI{images: images}, fakeKeys{}, images, service, railway.NetworkIsolated, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- controller.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		service.mu.Lock()
		created := len(service.creates)
		service.mu.Unlock()
		if created > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.creates) != 1 || service.creates[0].CheckpointName != "ready" {
		t.Fatalf("existing base not used during refresh: %+v", service.creates)
	}
}

func TestProvisioningFailureDoesNotConsumeAnotherJobsBusyCount(t *testing.T) {
	c := NewController(Config{}, nil, nil, nil, nil, railway.NetworkIsolated, nil, nil)
	c.activeJobIDs["cold"] = false
	c.activeJobIDs["running"] = true
	c.counts.Provisioning, c.counts.Busy = 1, 1
	c.release("cold")
	if c.counts.Provisioning != 0 || c.counts.Busy != 1 {
		t.Fatalf("counts=%+v", c.counts)
	}
	c.release("running")
	if c.Snapshot().Total() != 0 {
		t.Fatalf("counts=%+v", c.counts)
	}
}
