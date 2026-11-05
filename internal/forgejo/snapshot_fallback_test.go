package forgejo

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/railway"
)

type unavailableSnapshots struct {
	fakeImages
	hang   bool
	setups atomic.Int32
}

func (f *unavailableSnapshots) ResolveVersion(context.Context) (string, error) {
	return "13.2.1", nil
}

func (f *unavailableSnapshots) PrepareBase(ctx context.Context) (RunnerImage, error) {
	if f.hang {
		<-ctx.Done()
	}
	return RunnerImage{}, errors.New("checkpoint unavailable")
}

func (f *unavailableSnapshots) PrepareSandbox(context.Context, string, string) error {
	f.setups.Add(1)
	return nil
}

type pollingAPI struct {
	fakeForgejoAPI
	polls atomic.Int32
}

func (f *pollingAPI) WaitingJobs(ctx context.Context) ([]Job, error) {
	f.polls.Add(1)
	return f.fakeForgejoAPI.WaitingJobs(ctx)
}

func TestControllerRunsJobsWhileBaseSnapshotFailsOrHangs(t *testing.T) {
	for _, hang := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "hang"}[hang], func(t *testing.T) {
			images := &unavailableSnapshots{hang: hang}
			api := &pollingAPI{fakeForgejoAPI: fakeForgejoAPI{jobs: []Job{{ID: 7, RepoID: 42, Name: "build"}}}}
			service := &fakeSandboxService{execBlock: make(chan struct{})}
			cfg := Config{RunnerVersion: "13.2.1", PollInterval: 5 * time.Millisecond, SnapshotRefresh: time.Hour,
				SnapshotBuildTimeout: 50 * time.Millisecond, RunnerTimeout: time.Second, MaxConcurrentSandboxes: 1, MaxLaunchPerPoll: 1}
			controller := NewController(cfg, api, fakeKeys{}, images, service, railway.NetworkIsolated, nil, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- controller.Run(ctx) }()
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) && (api.polls.Load() < 3 || images.setups.Load() == 0) {
				time.Sleep(time.Millisecond)
			}
			cancel()
			if err := <-done; err != nil {
				t.Errorf("snapshot failure stopped controller: %v", err)
			}
			if api.polls.Load() < 3 || images.setups.Load() == 0 {
				t.Fatalf("polls=%d cold setups=%d; snapshot blocked job dispatch", api.polls.Load(), images.setups.Load())
			}
			service.mu.Lock()
			defer service.mu.Unlock()
			if len(service.creates) != 1 || service.creates[0].CheckpointName != "" || len(service.destroyed) != 1 {
				t.Fatalf("cold sandbox lifecycle: creates=%+v destroyed=%v", service.creates, service.destroyed)
			}
		})
	}
}
