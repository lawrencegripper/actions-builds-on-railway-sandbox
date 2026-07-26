// Command actions-builds-on-railway-sandbox runs GitHub Actions jobs as ephemeral runners
// inside Railway sandboxes, driven by the GitHub Actions runner scale set API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/config"
	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/dashboard"
	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/railway"
	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/scaler"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	cfg := &config.Config{}
	cmd := &cobra.Command{
		Use:   "actions-builds-on-railway-sandbox",
		Short: "Run GitHub or Forgejo Actions builds in ephemeral Railway sandboxes",
	}

	p := cmd.PersistentFlags()
	p.StringVar(&cfg.RailwayToken, "railway-token", "", "Railway account/workspace token (or RAILWAY_API_TOKEN)")
	p.StringVar(&cfg.RailwayProjectToken, "railway-project-token", "", "Railway project token, environment-scoped (or RAILWAY_PROJECT_TOKEN)")
	p.StringVar(&cfg.RailwayEnvironmentID, "railway-environment-id", "", "Railway environment id (or RAILWAY_ENVIRONMENT_ID)")
	p.StringVar(&cfg.RailwayEndpoint, "railway-endpoint", "", "Railway GraphQL endpoint (or RAILWAY_GRAPHQL_ENDPOINT)")
	p.BoolVar(&cfg.EnablePrivateNetworking, "enable-private-networking", false, "give sandboxes access to the Railway private network")
	p.StringVar(&cfg.LogLevel, "log-level", "info", "log level: debug, info, warn, error")
	p.StringVar(&cfg.LogFormat, "log-format", "text", "log format: text or json")

	cmd.AddCommand(githubCmd(cfg), forgejoCmd(cfg), smokeCmd(cfg))
	return cmd
}

func githubCmd(cfg *config.Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "github",
		Short: "Run GitHub Actions via the runner scale-set protocol",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cfg.ApplyEnv(cmd.Flags().Changed); err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return runGitHub(ctx, cfg)
		},
	}
	f := cmd.Flags()
	f.StringVar(&cfg.RegistrationURL, "url", "", "GitHub repo/org/enterprise URL (or GITHUB_URL)")
	f.StringVar(&cfg.ScaleSetName, "name", "", "scale-set name and default runs-on label (or GITHUB_SCALE_SET_NAME)")
	f.StringSliceVar(&cfg.Labels, "labels", nil, "labels for workflow targeting (defaults to --name)")
	f.StringVar(&cfg.RunnerGroup, "runner-group", scaleset.DefaultRunnerGroup, "runner group name")
	f.IntVar(&cfg.MinRunners, "min-runners", 0, "minimum pre-provisioned runners")
	f.IntVar(&cfg.MaxRunners, "max-runners", 5, "maximum concurrent runners")
	f.StringVar(&cfg.GitHubApp.ClientID, "app-client-id", "", "GitHub App client id")
	f.Int64Var(&cfg.GitHubApp.InstallationID, "app-installation-id", 0, "GitHub App installation id")
	f.StringVar(&cfg.GitHubApp.PrivateKey, "app-private-key", "", "GitHub App private key (PEM)")
	f.StringVar(&cfg.Token, "token", "", "GitHub PAT (alternative to a GitHub App)")
	f.IntVar(&cfg.IdleTimeoutMinutes, "idle-timeout-minutes", 30, "sandbox idle timeout")
	f.StringVar(&cfg.RunnerVersion, "runner-version", "", "GitHub Actions runner version (default: latest)")
	f.StringSliceVar(&cfg.ExtraPackages, "extra-packages", nil, "extra apt packages to bake into the runner image")
	f.StringVar(&cfg.ExtraSetup, "extra-setup", "", "extra shell run as the final image build step")
	return cmd
}

func runGitHub(ctx context.Context, cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	logger := cfg.Logger()

	ssClient, err := cfg.ScalesetClient()
	if err != nil {
		return fmt.Errorf("create scaleset client: %w", err)
	}

	runnerGroupID := 1
	if cfg.RunnerGroup != scaleset.DefaultRunnerGroup {
		rg, err := ssClient.GetRunnerGroupByName(ctx, cfg.RunnerGroup)
		if err != nil {
			return fmt.Errorf("get runner group %q: %w", cfg.RunnerGroup, err)
		}
		runnerGroupID = rg.ID
	}

	scaleSet, err := ssClient.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
		Name:          cfg.ScaleSetName,
		RunnerGroupID: runnerGroupID,
		Labels:        cfg.BuildLabels(),
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	})
	if err != nil {
		return fmt.Errorf("create runner scale set: %w", err)
	}
	logger.Info("runner scale set created", "id", scaleSet.ID, "name", scaleSet.Name)
	defer func() {
		if err := ssClient.DeleteRunnerScaleSet(context.WithoutCancel(ctx), scaleSet.ID); err != nil {
			logger.Error("delete runner scale set failed", "id", scaleSet.ID, "error", err.Error())
		}
	}()

	railwayClient, err := cfg.RailwayClient(logger.WithGroup("railway"))
	if err != nil {
		return fmt.Errorf("create railway client: %w", err)
	}

	if err := cfg.ResolveRunnerVersion(ctx); err != nil {
		return err
	}
	store, err := dashboard.Open(dashboard.DBPath(), logger.WithGroup("dashboard"))
	if err != nil {
		return fmt.Errorf("open dashboard store: %w", err)
	}
	defer store.Close()

	instructions := cfg.RunnerTemplateInstructions()
	// Runners boot from a prebuilt, content-addressed template image. Ensure it
	// exists before listening; Railway reuses the cached build if it already does.
	logger.Info("building runner template", "runnerVersion", cfg.RunnerVersion)
	started := time.Now()
	res, buildErr := railwayClient.BuildTemplateUntilReady(ctx, instructions)
	recordTemplateBuild(store, logger, cfg, res, buildErr, started, instructions)
	if buildErr != nil {
		return fmt.Errorf("build runner template: %w", buildErr)
	}

	sc := scaler.New(scaler.Config{
		ScaleSetID:           scaleSet.ID,
		MinRunners:           cfg.MinRunners,
		MaxRunners:           cfg.MaxRunners,
		TemplateInstructions: instructions,
		StartCommand:         config.RunnerStartCommand,
		IdleTimeoutMinutes:   cfg.IdleTimeoutMinutes,
		NetworkIsolation:     cfg.NetworkIsolation(),
		Provisioner:          railwayClient,
		JitGenerator:         ssClient,
		Sink:                 store,
		Logger:               logger.WithGroup("scaler"),
	})
	defer sc.Shutdown(context.WithoutCancel(ctx))

	dashSrv := dashboard.NewServer(store, sc, cfg.ScaleSetName, logger.WithGroup("dashboard"))
	httpSrv := &http.Server{Addr: ":" + listenPort(), Handler: dashSrv.Routes()}
	go func() {
		logger.Info("dashboard listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("dashboard server", "error", err.Error())
		}
	}()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutCtx)
	}()

	owner := hostname()
	session, err := ssClient.MessageSessionClient(ctx, scaleSet.ID, owner)
	if err != nil {
		return fmt.Errorf("create message session: %w", err)
	}
	defer session.Close(context.WithoutCancel(ctx))

	l, err := listener.New(session, listener.Config{
		ScaleSetID: scaleSet.ID,
		MaxRunners: cfg.MaxRunners,
		Logger:     logger.WithGroup("listener"),
	})
	if err != nil {
		return fmt.Errorf("create listener: %w", err)
	}

	logger.Info("listening for jobs", "scaleSet", cfg.ScaleSetName, "maxRunners", cfg.MaxRunners)
	if err := l.Run(ctx, sc); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("listener run: %w", err)
	}
	return nil
}

// recordTemplateBuild persists a template build attempt for the dashboard.
// Railway does not expose per-line build logs (the SandboxTemplateBuild type is
// only id + status), so we store the resolved version, recipe, status, timing,
// and any error instead.
func recordTemplateBuild(store *dashboard.Store, logger *slog.Logger, cfg *config.Config, res railway.TemplateBuildResult, buildErr error, started time.Time, instructions []string) {
	status := string(res.Status)
	if status == "" {
		status = string(railway.BuildFailed)
	}
	b := dashboard.TemplateBuild{
		BuildID:       res.ID,
		RunnerVersion: cfg.RunnerVersion,
		Recipe:        strings.Join(instructions, "\n"),
		Status:        status,
		Started:       started,
		Finished:      time.Now(),
	}
	if buildErr != nil {
		b.Error = buildErr.Error()
	}
	if _, err := store.RecordTemplateBuild(b); err != nil {
		logger.Error("record template build", "error", err.Error())
	}
}

func listenPort() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return "8080"
}

func hostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return uuid.NewString()
}
