package scaler

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/lawrencegripper/railway-sandbox-actions/internal/railway"
)

type fakeProvisioner struct {
	mu          sync.Mutex
	createCount atomic.Int32
	execCount   atomic.Int32
	destroyed   []string
	created     []string
	blockExec   chan struct{} // if set, Exec blocks until closed
}

func (f *fakeProvisioner) CreateSandbox(_ context.Context, _ railway.CreateOptions) (*railway.Sandbox, error) {
	n := f.createCount.Add(1)
	id := fmt.Sprintf("sbx_%d", n)
	f.mu.Lock()
	f.created = append(f.created, id)
	f.mu.Unlock()
	return &railway.Sandbox{ID: id, Status: railway.StatusRunning}, nil
}

func (f *fakeProvisioner) DestroySandbox(_ context.Context, id string) error {
	f.mu.Lock()
	f.destroyed = append(f.destroyed, id)
	f.mu.Unlock()
	return nil
}

func (f *fakeProvisioner) Exec(_ context.Context, _, _ string, _ railway.ExecOptions) (*railway.ExecResult, error) {
	f.execCount.Add(1)
	if f.blockExec != nil {
		<-f.blockExec
	}
	return &railway.ExecResult{ExitCode: 0}, nil
}

func (f *fakeProvisioner) destroyedList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.destroyed...)
}

type fakeJit struct{ count atomic.Int32 }

func (f *fakeJit) GenerateJitRunnerConfig(_ context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.count.Add(1)
	return &scaleset.RunnerScaleSetJitRunnerConfig{
		EncodedJITConfig: "jit-" + s.Name,
		Runner:           &scaleset.RunnerReference{Name: s.Name},
	}, nil
}

func newTestScaler(p Provisioner, j JitGenerator, min, max int) *Scaler {
	return New(Config{
		ScaleSetID:   1,
		MinRunners:   min,
		MaxRunners:   max,
		Provisioner:  p,
		JitGenerator: j,
	})
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

func TestScaleUpToDesired(t *testing.T) {
	p := &fakeProvisioner{blockExec: make(chan struct{})}
	j := &fakeJit{}
	s := newTestScaler(p, j, 0, 5)

	got, err := s.HandleDesiredRunnerCount(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Fatalf("expected reserved count 3, got %d", got)
	}
	waitFor(t, func() bool { return p.createCount.Load() == 3 }, "3 sandboxes created")
	waitFor(t, func() bool { return p.execCount.Load() == 3 }, "3 runners started")
	if j.count.Load() != 3 {
		t.Fatalf("expected 3 jit configs, got %d", j.count.Load())
	}
	close(p.blockExec)
}

func TestNoOverProvisionOnRepeatedCalls(t *testing.T) {
	p := &fakeProvisioner{blockExec: make(chan struct{})}
	s := newTestScaler(p, &fakeJit{}, 0, 5)

	s.HandleDesiredRunnerCount(context.Background(), 2)
	waitFor(t, func() bool { return p.createCount.Load() == 2 }, "first 2 created")
	// Same desired count again: must not create more.
	s.HandleDesiredRunnerCount(context.Background(), 2)
	time.Sleep(50 * time.Millisecond)
	if p.createCount.Load() != 2 {
		t.Fatalf("over-provisioned: %d sandboxes", p.createCount.Load())
	}
	close(p.blockExec)
}

func TestCapAtMaxRunners(t *testing.T) {
	p := &fakeProvisioner{blockExec: make(chan struct{})}
	s := newTestScaler(p, &fakeJit{}, 0, 2)

	got, _ := s.HandleDesiredRunnerCount(context.Background(), 10)
	if got != 2 {
		t.Fatalf("expected cap at 2, got %d", got)
	}
	waitFor(t, func() bool { return p.createCount.Load() == 2 }, "exactly 2 created")
	time.Sleep(50 * time.Millisecond)
	if p.createCount.Load() != 2 {
		t.Fatalf("exceeded max: %d", p.createCount.Load())
	}
	close(p.blockExec)
}

func TestJobCompletedDestroysSandbox(t *testing.T) {
	p := &fakeProvisioner{}
	s := newTestScaler(p, &fakeJit{}, 0, 5)

	// Reserve+provision one runner with a known name.
	name := "runner-x"
	if !s.runner.reserve(name) {
		t.Fatal("reserve failed")
	}
	s.provision(context.Background(), name)
	waitFor(t, func() bool {
		_, ok := s.runner.idleSandbox(name)
		return ok
	}, "runner idle")

	id, _ := s.runner.idleSandbox(name)
	if err := s.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: name}); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: name, Result: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	destroyed := p.destroyedList()
	if len(destroyed) != 1 || destroyed[0] != id {
		t.Fatalf("expected sandbox %s destroyed, got %v", id, destroyed)
	}
	if s.runner.count() != 0 {
		t.Fatalf("expected 0 tracked runners, got %d", s.runner.count())
	}
}

func TestShutdownDestroysAll(t *testing.T) {
	p := &fakeProvisioner{blockExec: make(chan struct{})}
	s := newTestScaler(p, &fakeJit{}, 0, 5)
	s.HandleDesiredRunnerCount(context.Background(), 3)
	waitFor(t, func() bool { return p.createCount.Load() == 3 }, "3 created")
	waitFor(t, func() bool { return s.runner.idleCount() == 3 }, "3 idle")

	s.Shutdown(context.Background())
	if len(p.destroyedList()) != 3 {
		t.Fatalf("expected 3 destroyed, got %d", len(p.destroyedList()))
	}
	close(p.blockExec)
}

func TestRunnerStateTransitions(t *testing.T) {
	r := &runnerState{
		provisioning: map[string]struct{}{},
		idle:         map[string]string{},
		busy:         map[string]string{},
	}
	if !r.reserve("a") {
		t.Fatal("reserve")
	}
	if r.reserve("a") {
		t.Fatal("double reserve should fail")
	}
	if r.count() != 1 {
		t.Fatal("count after reserve")
	}
	if !r.markIdle("a", "sbx_a") {
		t.Fatal("markIdle")
	}
	if r.markIdle("missing", "x") {
		t.Fatal("markIdle on missing should fail")
	}
	r.markBusy("a")
	id, ok := r.remove("a")
	if !ok || id != "sbx_a" {
		t.Fatalf("remove busy: %s %v", id, ok)
	}
	if _, ok := r.remove("a"); ok {
		t.Fatal("remove twice should report unknown")
	}
}
