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
	ResolveVersion(context.Context) (string, error)
	PrepareSandbox(context.Context, string, string) error
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
	generation   uint64
	snapshotMu   sync.Mutex
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

func (c *Controller) Run(ctx context.Context) error {
	if err := c.cfg.validateRestricted(); err != nil {
		return err
	}
	version, err := c.images.ResolveVersion(ctx)
	if err != nil {
		return fmt.Errorf("resolve Forgejo runner version: %w", err)
	}
	base := RunnerImage{Version: version, Kind: "base", RefreshedAt: time.Now()}
	c.logger.Info("Forgejo runner controller ready", "version", version, "coldFallback", true)
	type refreshResult struct {
		image RunnerImage
		err   error
	}
	prepared := make(chan refreshResult, 1)
	refreshing := false
	nextRefresh := time.Now()
	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()
	defer c.wg.Wait()
	for ctx.Err() == nil {
		if !refreshing && !time.Now().Before(nextRefresh) && c.snapshotMu.TryLock() {
			refreshing = true
			c.wg.Add(1)
			go func() {
				defer c.wg.Done()
				defer c.snapshotMu.Unlock()
				buildCtx, cancel := context.WithTimeout(ctx, c.cfg.SnapshotBuildTimeout)
				defer cancel()
				image, err := c.images.PrepareBase(buildCtx)
				select {
				case prepared <- refreshResult{image, err}:
				case <-ctx.Done():
				}
			}()
		}
		if err := c.poll(ctx, base); err != nil && ctx.Err() == nil {
			c.logger.Error("poll Forgejo jobs", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case result := <-prepared:
			refreshing = false
			if result.err != nil {
				c.logger.Warn("base snapshot unavailable; continuing with existing checkpoint or cold runners", "error", result.err)
				nextRefresh = time.Now().Add(min(5*time.Minute, c.cfg.SnapshotRefresh))
			} else {
				base = result.image
				c.mu.Lock()
				c.generation++
				base.generation = c.generation
				c.jobImages = make(map[string]RunnerImage)
				c.mu.Unlock()
				nextRefresh = time.Now().Add(c.cfg.SnapshotRefresh)
				c.logger.Info("base snapshot ready", "checkpoint", base.Name, "version", base.Version)
			}
		case <-ticker.C:
		}
	}
	return nil
}

func (c *Controller) poll(ctx context.Context, base RunnerImage) error {
	jobs, err := c.api.WaitingJobs(ctx)
	if err != nil {
		return err
	}
	if c.cfg.RestrictedPolicy != nil {
		allowed := make([]Job, 0, len(jobs))
		for _, job := range jobs {
			if err := c.authorize(ctx, job); err != nil {
				c.logger.Warn("reject restricted Forgejo job", "jobId", job.ID, "error", err)
				continue
			}
			allowed = append(allowed, job)
		}
		jobs = allowed
	}
	c.mu.Lock()
	capacity := max(0, c.cfg.MaxConcurrentSandboxes-c.counts.Total())
	launchLimit := min(capacity, c.cfg.MaxLaunchPerPoll)
	launch := make([]Job, 0, launchLimit)
	for _, job := range jobs {
		if len(launch) >= launchLimit {
			break
		}
		if _, active := c.activeJobIDs[job.Key()]; !active {
			c.activeJobIDs[job.Key()] = false
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
		if image.Name == "" || c.cfg.RestrictedPolicy != nil {
			image = base
		}
		c.mu.Unlock()
		c.wg.Add(1)
		go func(job Job, sequence uint64, snapshotKey string, image RunnerImage) {
			defer c.wg.Done()
			defer c.release(job.Key())
			c.runOne(ctx, job, sequence, snapshotKey, image)
		}(job, sequence, key, image)
	}
	return nil
}

func (c *Controller) runOne(ctx context.Context, job Job, sequence uint64, snapshotKey string, image RunnerImage) {
	runnerName := fmt.Sprintf("railway-forgejo-%d-%d", time.Now().Unix(), sequence)
	started := time.Now()
	if c.cfg.RestrictedPolicy != nil {
		if image.Kind != "base" || image.Version != "13.2.1" {
			c.logger.Error("restricted runner requires a clean pinned base", "jobId", job.ID)
			return
		}
		if err := c.authorize(ctx, job); err != nil {
			c.logger.Warn("reject restricted Forgejo job", "jobId", job.ID, "error", err)
			return
		}
	}
	options := railway.CreateOptions{
		CheckpointName:     image.Name,
		IdleTimeoutMinutes: c.cfg.SandboxIdleTimeoutMinutes,
		NetworkIsolation:   c.network,
	}
	sandbox, err := c.railway.CreateSandbox(ctx, options)
	if err != nil && image.Name != "" && ctx.Err() == nil {
		c.logger.Warn("restore checkpoint failed; starting cold runner", "checkpoint", image.Name, "error", err)
		options.CheckpointName = ""
		sandbox, err = c.railway.CreateSandbox(ctx, options)
	}
	if err != nil {
		c.logger.Error("create Forgejo runner sandbox", "runner", runnerName, "error", err)
		return
	}
	destroy := true
	defer func() {
		if destroy {
			c.destroySandbox(ctx, sandbox.ID)
		}
	}()
	if options.CheckpointName == "" {
		c.logger.Info("preparing cold runner", "jobId", job.ID, "sandbox", sandbox.ID)
		setupCtx, cancel := context.WithTimeout(ctx, c.cfg.SnapshotBuildTimeout)
		err = c.images.PrepareSandbox(setupCtx, sandbox.ID, image.Version)
		cancel()
		if err != nil {
			c.logger.Error("prepare cold runner", "jobId", job.ID, "error", err)
			return
		}
	}

	if c.cfg.RestrictedPolicy != nil {
		if err := c.authorize(ctx, job); err != nil {
			c.logger.Warn("restricted Forgejo authorization changed before launch", "jobId", job.ID, "error", err)
			return
		}
	}
	tailscaleKey, err := c.keys.AuthKey(ctx)
	if err != nil {
		c.logger.Error("mint Tailscale auth key", "runner", runnerName, "error", err)
		return
	}
	registrationToken, err := c.api.RegistrationToken(ctx)
	if err != nil {
		c.logger.Error("mint Forgejo registration token", "runner", runnerName, "error", err)
		return
	}
	c.markRunning(job.Key())

	externalID := strconv.FormatInt(job.ID, 10)
	c.sink.ProviderJobStarted("forgejo", externalID, job.Name, strconv.FormatInt(job.RepoID, 10), runnerName, sandbox.ID)
	runCtx, cancel := context.WithTimeout(ctx, c.cfg.RunnerTimeout)
	env := map[string]string{
		"FORGEJO_URL":              c.cfg.URL,
		"FORGEJO_RUNNER_LABELS":    c.cfg.RunnerLabels,
		"FORGEJO_RUNNER_EPHEMERAL": "true",
		"RUNNER_NAME":              runnerName,
		"PATH":                     "/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin:/usr/local/sbin",
		"SHELL":                    "/bin/bash",
	}
	if c.cfg.RestrictedPolicy != nil {
		env["FORGEJO_RUNNER_RESTRICTED"] = "true"
		env["FORGEJO_JOB_HANDLE"] = job.Handle
	}
	result, execErr := c.railway.Exec(runCtx, sandbox.ID, "exec /usr/local/bin/start-forgejo-runner", railway.ExecOptions{
		Env:   env,
		Stdin: []byte(tailscaleKey + "\x00" + registrationToken + "\x00"),
	})
	cancel()
	if execErr != nil {
		outcome := "failed"
		if runCtx.Err() == context.DeadlineExceeded {
			outcome = "timed_out"
		}
		c.sink.ProviderJobCompleted("forgejo", externalID, runnerName, sandbox.ID, outcome)
		c.logger.Error("Forgejo runner exec", "runner", runnerName, "sandbox", sandbox.ID, "error", execErr)
		return
	}
	c.logger.Info("Forgejo runner finished", "jobId", job.ID, "runner", runnerName, "sandbox", sandbox.ID, "exitCode", result.ExitCode, "reason", result.Reason)
	if result.ExitCode != 0 {
		c.sink.ProviderJobCompleted("forgejo", externalID, runnerName, sandbox.ID, "failed")
		return
	}

	completion, err := c.api.Completion(context.WithoutCancel(ctx), job, started)
	if err != nil {
		c.logger.Warn("match Forgejo job completion", "jobId", job.ID, "error", err)
		c.sink.ProviderJobCompleted("forgejo", externalID, runnerName, sandbox.ID, "unknown")
		return
	}
	c.sink.ProviderJobCompleted("forgejo", externalID, runnerName, sandbox.ID, completion.Outcome)
	if !completion.Hydrate || c.cfg.RestrictedPolicy != nil || ctx.Err() != nil || !c.snapshotMu.TryLock() {
		return
	}
	destroy = false
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer c.snapshotMu.Unlock()
		defer c.destroySandbox(ctx, sandbox.ID)
		hydrateCtx, cancel := context.WithTimeout(ctx, c.cfg.SnapshotBuildTimeout)
		defer cancel()
		hydrated, err := c.images.Hydrate(hydrateCtx, sandbox.ID, image, snapshotKey)
		if err != nil {
			c.logger.Warn("optional job snapshot failed", "jobId", job.ID, "sandbox", sandbox.ID, "error", err)
			return
		}
		c.mu.Lock()
		if image.generation == c.generation {
			hydrated.generation = image.generation
			c.jobImages[snapshotKey] = hydrated
		}
		c.mu.Unlock()
		c.logger.Info("hydrated Forgejo runner checkpoint", "jobId", job.ID, "checkpoint", hydrated.Name)
	}()
}

func (c *Controller) destroySandbox(ctx context.Context, id string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := c.railway.DestroySandbox(cleanupCtx, id); err != nil {
		c.logger.Error("destroy Forgejo runner sandbox", "sandbox", id, "error", err)
	}
}

func (c *Controller) authorize(ctx context.Context, job Job) error {
	if err := c.cfg.validateRestricted(); err != nil {
		return err
	}
	api, ok := c.api.(interface {
		AuthorizeJob(context.Context, Job) error
	})
	if !ok {
		return fmt.Errorf("Forgejo client does not support restricted job authorization")
	}
	return api.AuthorizeJob(ctx, job)
}

func (c *Controller) markRunning(jobKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if busy, active := c.activeJobIDs[jobKey]; active && !busy {
		c.activeJobIDs[jobKey] = true
		c.counts.Provisioning--
		c.counts.Busy++
	}
}

func (c *Controller) release(jobKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if busy, active := c.activeJobIDs[jobKey]; active {
		if busy {
			c.counts.Busy--
		} else {
			c.counts.Provisioning--
		}
		delete(c.activeJobIDs, jobKey)
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
