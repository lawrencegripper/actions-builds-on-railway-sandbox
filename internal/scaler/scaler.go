// Package scaler implements listener.Scaler by provisioning each GitHub Actions
// runner inside its own Railway sandbox.
package scaler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"

	"github.com/lawrencegripper/railway-sandbox-github-actions-runner/internal/railway"
)

// Provisioner is the subset of the Railway client the scaler needs (kept small
// for testability).
type Provisioner interface {
	CreateSandbox(ctx context.Context, opts railway.CreateOptions) (*railway.Sandbox, error)
	DestroySandbox(ctx context.Context, id string) error
	Exec(ctx context.Context, sandboxID, command string, opts railway.ExecOptions) (*railway.ExecResult, error)
}

// JitGenerator mints just-in-time runner configs (satisfied by scaleset.Client).
type JitGenerator interface {
	GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
}

// Counts is a point-in-time view of the runner pool.
type Counts struct {
	Provisioning int
	Idle         int
	Busy         int
}

func (c Counts) Total() int { return c.Provisioning + c.Idle + c.Busy }

// EventSink observes job lifecycle for reporting (e.g. the dashboard store). All
// methods must tolerate being called concurrently and should not block; the
// scaler runs them inline on the listener/runner paths. A nil sink is replaced
// with a no-op so the scaler works without one.
type EventSink interface {
	JobStarted(runnerName string)
	JobCompleted(runnerName, result string)
}

type nopSink struct{}

func (nopSink) JobStarted(string)           {}
func (nopSink) JobCompleted(string, string) {}

// Config configures the Scaler.
type Config struct {
	ScaleSetID int
	MinRunners int
	MaxRunners int

	// TemplateInstructions builds the runner image (passed to every create so
	// the backend reuses the cached build).
	TemplateInstructions []string
	// StartCommand is the baked start script that launches the runner.
	StartCommand       string
	IdleTimeoutMinutes int
	NetworkIsolation   railway.NetworkIsolation

	Provisioner  Provisioner
	JitGenerator JitGenerator
	Logger       *slog.Logger
	// Sink observes job lifecycle events; optional (defaults to a no-op).
	Sink EventSink
}

type Scaler struct {
	cfg    Config
	log    *slog.Logger
	runner *runnerState

	// Runner goroutines run under bgCtx (not the listener's ctx) so a job
	// outlives a single poll, and are tracked by wg so Shutdown can wait them out.
	bgCtx  context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var _ listener.Scaler = (*Scaler)(nil)

func New(cfg Config) *Scaler {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.StartCommand == "" {
		cfg.StartCommand = "/usr/local/bin/start-runner"
	}
	if cfg.Sink == nil {
		cfg.Sink = nopSink{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Scaler{
		cfg:    cfg,
		log:    cfg.Logger,
		bgCtx:  ctx,
		cancel: cancel,
		runner: &runnerState{
			provisioning: make(map[string]struct{}),
			idle:         make(map[string]string),
			busy:         make(map[string]string),
		},
	}
}

// HandleDesiredRunnerCount reserves capacity synchronously, then provisions the
// sandboxes asynchronously so the listener's poll loop is not blocked by the
// minute-or-so a sandbox boot can take.
func (s *Scaler) HandleDesiredRunnerCount(_ context.Context, count int) (int, error) {
	target := min(s.cfg.MaxRunners, s.cfg.MinRunners+count)

	// Bounded: reserve up to (target - current) slots, provisioning each async so
	// the listener's poll loop is not blocked by the minute-or-so a boot takes.
	for need := target - s.runner.count(); need > 0; need-- {
		name := fmt.Sprintf("runner-%s", uuid.NewString()[:8])
		if !s.runner.reserve(name) {
			continue
		}
		s.log.Info("scaling up", slog.Int("target", target), slog.String("runner", name))
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.provision(s.bgCtx, name)
		}()
	}
	return s.runner.count(), nil
}

func (s *Scaler) provision(ctx context.Context, name string) {
	jit, err := s.cfg.JitGenerator.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, s.cfg.ScaleSetID)
	if err != nil {
		s.log.Error("jit config failed", slog.String("runner", name), slog.String("error", err.Error()))
		s.runner.release(name)
		return
	}

	sb, err := s.cfg.Provisioner.CreateSandbox(ctx, railway.CreateOptions{
		IdleTimeoutMinutes:   s.cfg.IdleTimeoutMinutes,
		NetworkIsolation:     s.cfg.NetworkIsolation,
		TemplateInstructions: s.cfg.TemplateInstructions,
	})
	if err != nil {
		s.log.Error("sandbox create failed", slog.String("runner", name), slog.String("error", err.Error()))
		s.runner.release(name)
		return
	}

	if !s.runner.markIdle(name, sb.ID) {
		// Job already completed (or runner removed) before the sandbox was ready.
		s.log.Warn("runner gone before ready, destroying sandbox", slog.String("runner", name), slog.String("sandbox", sb.ID))
		s.destroy(ctx, sb.ID)
		return
	}
	s.log.Info("sandbox ready, starting runner", slog.String("runner", name), slog.String("sandbox", sb.ID))
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runRunner(ctx, name, sb.ID, jit.EncodedJITConfig)
	}()
}

// runRunner execs the start script, which runs exactly one job then exits. On
// any return path (success, exec error, or the job's ctx being cancelled at
// shutdown) it tears the sandbox down, so nothing leaks even when GitHub never
// sends a JobCompleted. Job stdout/stderr is intentionally not streamed into the
// orchestrator's logs — it can contain secrets and already lives in GitHub's job
// log.
func (s *Scaler) runRunner(ctx context.Context, name, sandboxID, jitConfig string) {
	rlog := s.log.With(slog.String("runner", name), slog.String("sandbox", sandboxID))
	defer s.finish(name, sandboxID)

	res, err := s.cfg.Provisioner.Exec(ctx, sandboxID, s.cfg.StartCommand, railway.ExecOptions{
		Env: map[string]string{
			"ACTIONS_RUNNER_INPUT_JITCONFIG": jitConfig,
			"RUNNER_ALLOW_RUNASROOT":         "1",
		},
	})
	if err != nil {
		rlog.Error("runner exec failed", slog.String("error", err.Error()))
		return
	}
	rlog.Info("runner exec exited", slog.Int("exitCode", res.ExitCode))
}

// finish removes the runner from tracking and destroys its sandbox, unless a
// concurrent HandleJobCompleted already claimed it (remove reports not-found).
func (s *Scaler) finish(name, sandboxID string) {
	if _, ok := s.runner.remove(name); ok {
		s.destroy(context.WithoutCancel(s.bgCtx), sandboxID)
	}
}

func (s *Scaler) HandleJobStarted(ctx context.Context, jobInfo *scaleset.JobStarted) error {
	s.log.Info("job started", slog.String("runner", jobInfo.RunnerName), slog.String("jobId", jobInfo.JobID))
	s.runner.markBusy(jobInfo.RunnerName)
	s.cfg.Sink.JobStarted(jobInfo.RunnerName)
	return nil
}

func (s *Scaler) HandleJobCompleted(ctx context.Context, jobInfo *scaleset.JobCompleted) error {
	s.log.Info("job completed", slog.String("runner", jobInfo.RunnerName), slog.String("result", jobInfo.Result))
	s.cfg.Sink.JobCompleted(jobInfo.RunnerName, jobInfo.Result)
	sandboxID, ok := s.runner.remove(jobInfo.RunnerName)
	if !ok {
		s.log.Warn("job completed for unknown runner", slog.String("runner", jobInfo.RunnerName))
		return nil
	}
	if sandboxID == "" {
		// Still provisioning; markIdle will see it's gone and clean up.
		return nil
	}
	s.destroy(ctx, sandboxID)
	return nil
}

func (s *Scaler) destroy(ctx context.Context, sandboxID string) {
	if err := s.cfg.Provisioner.DestroySandbox(ctx, sandboxID); err != nil {
		s.log.Error("sandbox destroy failed", slog.String("sandbox", sandboxID), slog.String("error", err.Error()))
		return
	}
	s.log.Info("sandbox destroyed", slog.String("sandbox", sandboxID))
}

// Shutdown stops provisioning, cancels in-flight runner goroutines and waits for
// them to tear their own sandboxes down, then destroys any stragglers. Call on
// exit. Safe to call once.
func (s *Scaler) Shutdown(ctx context.Context) {
	s.cancel()
	s.wg.Wait()
	for _, id := range s.runner.drain() {
		s.destroy(ctx, id)
	}
}

// runnerState tracks runners across three phases: provisioning (reserved, no
// sandbox yet), idle (sandbox booted), and busy (running a job). All maps are
// keyed by runner name; idle/busy values are sandbox ids.
type runnerState struct {
	mu           sync.Mutex
	provisioning map[string]struct{}
	idle         map[string]string
	busy         map[string]string
}

func (r *runnerState) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.provisioning) + len(r.idle) + len(r.busy)
}

// Snapshot returns the current runner counts by phase.
func (s *Scaler) Snapshot() Counts {
	r := s.runner
	r.mu.Lock()
	defer r.mu.Unlock()
	return Counts{
		Provisioning: len(r.provisioning),
		Idle:         len(r.idle),
		Busy:         len(r.busy),
	}
}

// MinRunners and MaxRunners expose the configured bounds for reporting.
func (s *Scaler) MinRunners() int { return s.cfg.MinRunners }
func (s *Scaler) MaxRunners() int { return s.cfg.MaxRunners }

func (r *runnerState) reserve(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.provisioning[name]; ok {
		return false
	}
	if _, ok := r.idle[name]; ok {
		return false
	}
	if _, ok := r.busy[name]; ok {
		return false
	}
	r.provisioning[name] = struct{}{}
	return true
}

func (r *runnerState) release(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.provisioning, name)
}

// markIdle promotes a reservation to idle. Returns false if the reservation was
// already removed (e.g. the job completed before the sandbox booted).
func (r *runnerState) markIdle(name, sandboxID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.provisioning[name]; !ok {
		return false
	}
	delete(r.provisioning, name)
	r.idle[name] = sandboxID
	return true
}

func (r *runnerState) markBusy(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.idle[name]; ok {
		delete(r.idle, name)
		r.busy[name] = id
	}
}

// remove drops a runner from tracking and returns its sandbox id. The bool is
// false if the runner was unknown; the string is empty if it was still
// provisioning (no sandbox id yet).
func (r *runnerState) remove(name string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.busy[name]; ok {
		delete(r.busy, name)
		return id, true
	}
	if id, ok := r.idle[name]; ok {
		delete(r.idle, name)
		return id, true
	}
	if _, ok := r.provisioning[name]; ok {
		delete(r.provisioning, name)
		return "", true
	}
	return "", false
}

func (r *runnerState) drain() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.idle)+len(r.busy))
	for _, id := range r.idle {
		ids = append(ids, id)
	}
	for _, id := range r.busy {
		ids = append(ids, id)
	}
	r.provisioning = make(map[string]struct{})
	r.idle = make(map[string]string)
	r.busy = make(map[string]string)
	return ids
}
