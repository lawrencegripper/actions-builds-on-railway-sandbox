package forgejo

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/railway"
)

type fakeSandboxService struct {
	mu          sync.Mutex
	creates     []railway.CreateOptions
	execs       []string
	execOptions []railway.ExecOptions
	destroyed   []string
	checkpoints []railway.Checkpoint
	deleted     []string
	execBlock   chan struct{}
}

func (f *fakeSandboxService) CreateSandbox(_ context.Context, options railway.CreateOptions) (*railway.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, options)
	return &railway.Sandbox{ID: "sbx-1", Status: railway.StatusRunning}, nil
}
func (f *fakeSandboxService) DestroySandbox(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyed = append(f.destroyed, id)
	return nil
}
func (f *fakeSandboxService) Exec(ctx context.Context, _ string, command string, options railway.ExecOptions) (*railway.ExecResult, error) {
	f.mu.Lock()
	f.execs = append(f.execs, command)
	f.execOptions = append(f.execOptions, options)
	block := f.execBlock
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &railway.ExecResult{ExitCode: 0}, nil
}
func (f *fakeSandboxService) CreateCheckpoint(_ context.Context, _ string, name string) (*railway.Checkpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	checkpoint := railway.Checkpoint{ID: "cp-new", Key: name, CreatedAt: time.Now().Format(time.RFC3339)}
	f.checkpoints = append(f.checkpoints, checkpoint)
	return &checkpoint, nil
}
func (f *fakeSandboxService) ListCheckpoints(_ context.Context) ([]railway.Checkpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]railway.Checkpoint(nil), f.checkpoints...), nil
}
func (f *fakeSandboxService) DeleteCheckpoint(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

func TestPrepareBaseAndHydrate(t *testing.T) {
	cfg := Config{
		RunnerVersion:             "11.0.1",
		SnapshotNamePrefix:        "test-runner",
		SnapshotBuildTimeout:      time.Minute,
		SandboxIdleTimeoutMinutes: 10,
		RetainedSnapshots:         2,
		ExtraPackages:             []string{"ripgrep"},
		PreloadDockerImages:       []string{"ubuntu:latest"},
	}
	service := &fakeSandboxService{}
	manager := NewImageManager(cfg, service, railway.NetworkIsolated, nil, nil)
	base, err := manager.PrepareBase(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if base.Kind != "base" || base.Version != "11.0.1" || !strings.HasPrefix(base.Name, "test-runner-11.0.1-base-") {
		t.Fatalf("base image = %+v", base)
	}
	if len(service.destroyed) != 1 || service.destroyed[0] != "sbx-1" {
		t.Fatalf("builder cleanup = %v", service.destroyed)
	}
	commands := strings.Join(service.execs, "\n")
	for _, want := range []string{"ripgrep", "forgejo-runner-11.0.1-linux-$ARCH", "ubuntu:latest", "start-forgejo-runner"} {
		if !strings.Contains(commands, want) {
			t.Errorf("base build commands missing %q", want)
		}
	}

	hydrated, err := manager.Hydrate(context.Background(), "sbx-job", base, "repo-42-build")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hydrated.Name, "hydrated-repo-42-build") {
		t.Fatalf("hydrated image = %+v", hydrated)
	}
	if !strings.Contains(service.execs[len(service.execs)-1], "/root/.docker/config.json") {
		t.Fatal("hydration did not scrub Docker credentials")
	}
}

func TestSanitizeCheckpointPart(t *testing.T) {
	if got := sanitizeCheckpointPart(" repo/with spaces "); got != "repo-with-spaces" {
		t.Fatalf("sanitize = %q", got)
	}
	if got := sanitizeCheckpointPart("///"); got != "snapshot" {
		t.Fatalf("empty sanitize = %q", got)
	}
}
