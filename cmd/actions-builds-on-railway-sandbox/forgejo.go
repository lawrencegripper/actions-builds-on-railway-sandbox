package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/config"
	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/dashboard"
	forgejoprovider "github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/forgejo"
)

func forgejoCmd(common *config.Config) *cobra.Command {
	cfg := &forgejoprovider.Config{SnapshotDefaultBranchJobs: true}
	cmd := &cobra.Command{
		Use:   "forgejo",
		Short: "Run Forgejo Actions via one-job ephemeral runners",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := common.ApplyRailwayEnv(cmd.Flags().Changed); err != nil {
				return err
			}
			if err := cfg.ApplyEnv(cmd.Flags().Changed); err != nil {
				return err
			}
			if err := common.ValidateRailway(); err != nil {
				return err
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return runForgejo(ctx, common, *cfg)
		},
	}
	f := cmd.Flags()
	f.StringVar(&cfg.URL, "forgejo-url", "", "Forgejo base URL (or FORGEJO_URL)")
	f.StringVar(&cfg.APIToken, "forgejo-token", "", "Forgejo admin API token (or FORGEJO_API_TOKEN)")
	f.StringVar(&cfg.JobsURL, "forgejo-jobs-url", "", "Forgejo runner jobs endpoint override")
	f.StringVar(&cfg.RegistrationTokenURL, "forgejo-registration-token-url", "", "Forgejo runner registration-token endpoint override")
	f.StringVar(&cfg.RunnerLabels, "runner-labels", "", "Forgejo runner labels (or FORGEJO_RUNNER_LABELS)")
	f.StringVar(&cfg.JobLabelFilter, "job-label-filter", "", "queued-job label filter (or FORGEJO_JOB_LABEL_FILTER)")
	f.StringVar(&cfg.RunnerVersion, "runner-version", "", "Forgejo runner version (default: latest)")
	f.StringSliceVar(&cfg.ExtraPackages, "extra-packages", nil, "extra apt packages baked into the base checkpoint")
	f.StringVar(&cfg.ExtraSetup, "extra-setup", "", "extra shell setup baked into the base checkpoint")
	f.StringSliceVar(&cfg.PreloadDockerImages, "preload-docker-images", nil, "public Docker images to cache in the base checkpoint")
	f.StringVar(&cfg.DockerWarmupSetup, "docker-warmup-setup", "", "extra shell setup run with dockerd during checkpoint creation")
	f.DurationVar(&cfg.PollInterval, "poll-interval", 5*time.Second, "Forgejo queued-job poll interval")
	f.DurationVar(&cfg.SnapshotRefresh, "snapshot-refresh", 24*time.Hour, "base checkpoint refresh interval")
	f.StringVar(&cfg.SnapshotNamePrefix, "snapshot-name-prefix", "forgejo-runner", "Railway checkpoint name prefix")
	f.DurationVar(&cfg.SnapshotBuildTimeout, "snapshot-build-timeout", 30*time.Minute, "timeout for checkpoint build/scrub steps")
	f.IntVar(&cfg.RetainedSnapshots, "retained-snapshots", 3, "checkpoints retained per family")
	f.BoolVar(&cfg.SnapshotDefaultBranchJobs, "snapshot-default-branch-jobs", true, "hydrate cache after successful default-branch jobs")
	f.IntVar(&cfg.MaxConcurrentSandboxes, "max-runners", 2, "maximum concurrent Forgejo runner sandboxes")
	f.IntVar(&cfg.MaxLaunchPerPoll, "max-launch-per-poll", 2, "maximum runners launched in one poll")
	f.IntVar(&cfg.SandboxIdleTimeoutMinutes, "sandbox-idle-timeout-minutes", 10, "Railway sandbox idle timeout")
	f.DurationVar(&cfg.RunnerTimeout, "runner-timeout", 2*time.Hour, "one-job runner execution timeout")
	f.StringVar(&cfg.TailscaleTailnet, "tailscale-tailnet", "", "Tailscale tailnet (or TAILSCALE_TAILNET)")
	f.StringVar(&cfg.TailscaleAPIToken, "tailscale-api-token", "", "Tailscale API token alternative to OAuth")
	f.StringVar(&cfg.TailscaleOAuthClientID, "tailscale-oauth-client-id", "", "Tailscale OAuth client id")
	f.StringVar(&cfg.TailscaleOAuthSecret, "tailscale-oauth-secret", "", "Tailscale OAuth client secret")
	f.StringVar(&cfg.TailscaleTag, "tailscale-tag", "tag:railway-forgejo-runner", "tag assigned to ephemeral runner nodes")
	f.DurationVar(&cfg.TailscaleKeyExpiry, "tailscale-key-expiry", 10*time.Minute, "ephemeral Tailscale key lifetime")
	return cmd
}

func runForgejo(ctx context.Context, common *config.Config, cfg forgejoprovider.Config) error {
	logger := common.Logger()
	railwayClient, err := common.RailwayClient(logger.WithGroup("railway"))
	if err != nil {
		return fmt.Errorf("create Railway client: %w", err)
	}
	store, err := dashboard.Open(dashboard.DBPath(), logger.WithGroup("dashboard"))
	if err != nil {
		return fmt.Errorf("open dashboard store: %w", err)
	}
	defer store.Close()

	api := forgejoprovider.NewAPI(cfg, nil, logger.WithGroup("forgejo"))
	keys := forgejoprovider.NewTailscaleKeys(cfg, nil)
	images := forgejoprovider.NewImageManager(cfg, railwayClient, common.NetworkIsolation(), nil, logger.WithGroup("image"))
	controller := forgejoprovider.NewController(cfg, api, keys, images, railwayClient, common.NetworkIsolation(), store, logger.WithGroup("controller"))

	dashSrv := dashboard.NewServer(store, controller, "Forgejo · "+cfg.RunnerLabels, logger.WithGroup("dashboard"))
	httpSrv := &http.Server{Addr: ":" + listenPort(), Handler: dashSrv.Routes()}
	go func() {
		logger.Info("dashboard listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("dashboard server", "error", err)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	return controller.Run(ctx)
}
