package forgejo

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/railway"
	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/scaler"
)

type ForgejoService interface {
	WaitingJobs(context.Context) ([]Job, error)
	RegistrationToken(context.Context) (string, error)
	Completion(context.Context, Job, time.Time) (JobCompletion, error)
}

type AuthKeyIssuer interface {
	AuthKey(context.Context) (string, error)
}

type ImageService interface {
	PrepareBase(context.Context) (RunnerImage, error)
	Hydrate(context.Context, string, RunnerImage, string) (RunnerImage, error)
}

type ProviderEventSink interface {
	ProviderJobStarted(provider, externalJobID, jobName, repository, runnerName, sandboxID string)
	ProviderJobCompleted(provider, externalJobID, runnerName, sandboxID, result string)
}

type nopProviderSink struct{}

func (nopProviderSink) ProviderJobStarted(string, string, string, string, string, string) {}
func (nopProviderSink) ProviderJobCompleted(string, string, string, string, string)       {}

type Controller struct {
	cfg     Config
	api     ForgejoService
	keys    AuthKeyIssuer
	images  ImageService
	railway SandboxService
	network railway.NetworkIsolation
	sink    ProviderEventSink
	logger  *slog.Logger

	mu           sync.Mutex
	counts       scaler.Counts
	activeJobIDs map[string]bool
	jobImages    map[string]RunnerImage
	sequence     uint64
	wg           sync.WaitGroup
}

func NewController(cfg Config, api ForgejoService, keys AuthKeyIssuer, images ImageService, service SandboxService, network railway.NetworkIsolation, sink ProviderEventSink, logger *slog.Logger) *Controller {
	if sink == nil {
		sink = nopProviderSink{}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Controller{
		cfg: cfg, api: api, keys: keys, images: images, railway: service,
		network: network, sink: sink, logger: logger,
		activeJobIDs: make(map[string]bool), jobImages: make(map[string]RunnerImage),
	}
}

// Run builds the base checkpoint, polls Forgejo until ctx is cancelled, and
// waits for active one-job runners to clean up before returning.
func (c *Controller) Run(ctx context.Context) error {
	base, err := c.images.PrepareBase(ctx)
	if err != nil {
		return fmt.Errorf("prepare Forgejo runner image: %w", err)
	}
	c.logger.Info("Forgejo runner controller ready", "checkpoint", base.Name, "version", base.Version)

	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if time.Since(base.RefreshedAt) >= c.cfg.SnapshotRefresh {
			refreshed, refreshErr := c.images.PrepareBase(ctx)
			if refreshErr != nil {
				c.logger.Error("refresh Forgejo runner image", "error", refreshErr)
			} else {
				base = refreshed
				c.mu.Lock()
				c.jobImages = make(map[string]RunnerImage)
				c.mu.Unlock()
			}
		}
		if err := c.poll(ctx, base); err != nil && ctx.Err() == nil {
			c.logger.Error("poll Forgejo jobs", "error", err)
		}
		select {
		case <-ctx.Done():
			c.wg.Wait()
			return nil
		case <-ticker.C:
		}
	}
}

func (c *Controller) poll(ctx context.Context, base RunnerImage) error {
	jobs, err := c.api.WaitingJobs(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	capacity := max(0, c.cfg.MaxConcurrentSandboxes-c.counts.Total())
	launchLimit := min(capacity, c.cfg.MaxLaunchPerPoll)
	launch := make([]Job, 0, launchLimit)
	for _, job := range jobs {
		if len(launch) >= launchLimit {
			break
		}
		if !c.activeJobIDs[job.Key()] {
			c.activeJobIDs[job.Key()] = true
			c.counts.Provisioning++
			launch = append(launch, job)
		}
	}
	active := c.counts.Total()
	c.mu.Unlock()
	c.logger.Info("Forgejo poll complete", "waitingJobs", len(jobs), "activeSandboxes", active, "launching", len(launch))

	for _, job := range launch {
		c.mu.Lock()
		c.sequence++
		sequence := c.sequence
		key := snapshotKey(job)
		image := c.jobImages[key]
		if image.Name == "" {
			image = base
		}
		c.mu.Unlock()
		c.wg.Add(1)
		go func(job Job, sequence uint64, snapshotKey string, image RunnerImage) {
			defer c.wg.Done()
			defer c.release(job.Key())
			if hydrated, ok := c.runOne(ctx, job, sequence, snapshotKey, image); ok {
				c.mu.Lock()
				c.jobImages[snapshotKey] = hydrated
				c.mu.Unlock()
			}
		}(job, sequence, key, image)
	}
	return nil
}

func (c *Controller) runOne(ctx context.Context, job Job, sequence uint64, snapshotKey string, image RunnerImage) (RunnerImage, bool) {
	runnerName := fmt.Sprintf("railway-forgejo-%d-%d", time.Now().Unix(), sequence)
	started := time.Now()
	tailscaleKey, err := c.keys.AuthKey(ctx)
	if err != nil {
		c.logger.Error("mint Tailscale auth key", "runner", runnerName, "error", err)
		return RunnerImage{}, false
	}
	registrationToken, err := c.api.RegistrationToken(ctx)
	if err != nil {
		c.logger.Error("mint Forgejo registration token", "runner", runnerName, "error", err)
		return RunnerImage{}, false
	}
	sandbox, err := c.railway.CreateSandbox(ctx, railway.CreateOptions{
		CheckpointName:     image.Name,
		IdleTimeoutMinutes: c.cfg.SandboxIdleTimeoutMinutes,
		NetworkIsolation:   c.network,
	})
	if err != nil {
		c.logger.Error("create Forgejo runner sandbox", "runner", runnerName, "checkpoint", image.Name, "error", err)
		return RunnerImage{}, false
	}
	c.markRunning()
	defer func() {
		if err := c.railway.DestroySandbox(context.WithoutCancel(ctx), sandbox.ID); err != nil {
			c.logger.Error("destroy Forgejo runner sandbox", "runner", runnerName, "sandbox", sandbox.ID, "error", err)
		}
	}()

	externalID := strconv.FormatInt(job.ID, 10)
	c.sink.ProviderJobStarted("forgejo", externalID, job.Name, strconv.FormatInt(job.RepoID, 10), runnerName, sandbox.ID)
	runCtx, cancel := context.WithTimeout(ctx, c.cfg.RunnerTimeout)
	result, execErr := c.railway.Exec(runCtx, sandbox.ID, "/usr/local/bin/start-forgejo-runner", railway.ExecOptions{Env: map[string]string{
		"TS_AUTHKEY":                        tailscaleKey,
		"FORGEJO_URL":                       c.cfg.URL,
		"FORGEJO_RUNNER_REGISTRATION_TOKEN": registrationToken,
		"FORGEJO_RUNNER_LABELS":             c.cfg.RunnerLabels,
		"FORGEJO_RUNNER_EPHEMERAL":          "true",
		"RUNNER_NAME":                       runnerName,
		"PATH":                              "/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin:/usr/local/sbin",
		"SHELL":                             "/bin/bash",
	}})
	cancel()
	if execErr != nil {
		outcome := "failed"
		if runCtx.Err() == context.DeadlineExceeded {
			outcome = "timed_out"
		}
		c.sink.ProviderJobCompleted("forgejo", externalID, runnerName, sandbox.ID, outcome)
		c.logger.Error("Forgejo runner exec", "runner", runnerName, "sandbox", sandbox.ID, "error", execErr)
		return RunnerImage{}, false
	}
	c.logger.Info("Forgejo runner finished", "runner", runnerName, "sandbox", sandbox.ID, "exitCode", result.ExitCode)
	if result.ExitCode != 0 {
		c.sink.ProviderJobCompleted("forgejo", externalID, runnerName, sandbox.ID, "failed")
		return RunnerImage{}, false
	}

	completion, err := c.api.Completion(context.WithoutCancel(ctx), job, started)
	if err != nil {
		c.logger.Warn("match Forgejo job completion", "jobId", job.ID, "error", err)
		c.sink.ProviderJobCompleted("forgejo", externalID, runnerName, sandbox.ID, "unknown")
		return RunnerImage{}, false
	}
	c.sink.ProviderJobCompleted("forgejo", externalID, runnerName, sandbox.ID, completion.Outcome)
	if !completion.Hydrate {
		return RunnerImage{}, false
	}
	hydrateCtx, hydrateCancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.SnapshotBuildTimeout)
	defer hydrateCancel()
	hydrated, err := c.images.Hydrate(hydrateCtx, sandbox.ID, image, snapshotKey)
	if err != nil {
		c.logger.Error("hydrate Forgejo runner checkpoint", "jobId", job.ID, "sandbox", sandbox.ID, "error", err)
		return RunnerImage{}, false
	}
	c.logger.Info("hydrated Forgejo runner checkpoint", "jobId", job.ID, "checkpoint", hydrated.Name)
	return hydrated, true
}

func (c *Controller) markRunning() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts.Provisioning--
	c.counts.Busy++
}

func (c *Controller) release(jobKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.activeJobIDs, jobKey)
	if c.counts.Busy > 0 {
		c.counts.Busy--
	} else if c.counts.Provisioning > 0 {
		c.counts.Provisioning--
	}
}

func snapshotKey(job Job) string {
	return sanitizeCheckpointPart(fmt.Sprintf("repo-%d-%s", job.RepoID, firstNonEmpty(job.Name, strconv.FormatInt(job.ID, 10))))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" && value != "0" {
			return value
		}
	}
	return "job"
}

func (c *Controller) Snapshot() scaler.Counts {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts
}
func (c *Controller) MinRunners() int { return 0 }
func (c *Controller) MaxRunners() int { return c.cfg.MaxConcurrentSandboxes }
