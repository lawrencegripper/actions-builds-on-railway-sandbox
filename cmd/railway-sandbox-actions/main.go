// Command railway-sandbox-actions runs GitHub Actions jobs as ephemeral runners
// inside Railway sandboxes, driven by the GitHub Actions runner scale set API.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"unicode"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/lawrencegripper/railway-sandbox-actions/internal/config"
	"github.com/lawrencegripper/railway-sandbox-actions/internal/scaler"
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
		Use:   "railway-sandbox-actions",
		Short: "Run GitHub Actions runners as ephemeral Railway sandboxes via the scale set API",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := applyEnvOverrides(cmd, cfg); err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()
			return run(ctx, cfg)
		},
	}

	// Flags shared with subcommands (e.g. smoke) live on PersistentFlags.
	p := cmd.PersistentFlags()
	p.StringVar(&cfg.RailwayToken, "railway-token", "", "Railway account/workspace token (or RAILWAY_API_TOKEN)")
	p.StringVar(&cfg.RailwayProjectToken, "railway-project-token", "", "Railway project token, environment-scoped (or RAILWAY_PROJECT_TOKEN)")
	p.StringVar(&cfg.RailwayEnvironmentID, "railway-environment-id", "", "Railway environment id (or RAILWAY_ENVIRONMENT_ID, auto-injected on Railway)")
	p.StringVar(&cfg.RailwayEndpoint, "railway-endpoint", "", "Railway GraphQL endpoint (or RAILWAY_GRAPHQL_ENDPOINT)")
	p.BoolVar(&cfg.EnablePrivateNetworking, "enable-private-networking", false, "give sandboxes access to the Railway private network (https://docs.railway.com/networking/private-networking)")
	p.StringVar(&cfg.LogLevel, "log-level", "info", "log level: debug, info, warn, error")
	p.StringVar(&cfg.LogFormat, "log-format", "text", "log format: text or json")

	f := cmd.Flags()
	f.StringVar(&cfg.RegistrationURL, "url", "", "where to register the scale set (or GITHUB_URL), e.g. https://github.com/org/repo")
	f.StringVar(&cfg.ScaleSetName, "name", "", "scale set name, also the default runs-on label (or SCALE_SET_NAME)")
	f.StringSliceVar(&cfg.Labels, "labels", nil, "labels for workflow targeting (defaults to --name)")
	f.StringVar(&cfg.RunnerGroup, "runner-group", scaleset.DefaultRunnerGroup, "runner group name")
	f.IntVar(&cfg.MinRunners, "min-runners", 0, "minimum (pre-provisioned) runners")
	f.IntVar(&cfg.MaxRunners, "max-runners", 5, "maximum concurrent runners")

	f.StringVar(&cfg.GitHubApp.ClientID, "app-client-id", "", "GitHub App client id")
	f.Int64Var(&cfg.GitHubApp.InstallationID, "app-installation-id", 0, "GitHub App installation id")
	f.StringVar(&cfg.GitHubApp.PrivateKey, "app-private-key", "", "GitHub App private key (PEM)")
	f.StringVar(&cfg.Token, "token", "", "GitHub PAT (alternative to a GitHub App)")

	f.IntVar(&cfg.IdleTimeoutMinutes, "idle-timeout-minutes", 30, "sandbox idle timeout")
	f.StringVar(&cfg.RunnerVersion, "runner-version", "", "actions runner version (default: latest release)")
	f.StringSliceVar(&cfg.ExtraPackages, "extra-packages", nil, "extra apt packages to bake into the runner image")
	f.StringVar(&cfg.ExtraSetup, "extra-setup", "", "extra shell run as the final image build step")

	cmd.AddCommand(smokeCmd(cfg))
	return cmd
}

// applyEnvOverrides fills config from environment variables for any flag the
// user did not explicitly set, so the service can be driven entirely by Railway
// variables. Explicit flags win over env.
func applyEnvOverrides(cmd *cobra.Command, cfg *config.Config) error {
	f := cmd.Flags()
	str := func(flag, env string, dst *string) {
		if !f.Changed(flag) {
			if v, ok := os.LookupEnv(env); ok {
				*dst = v
			}
		}
	}
	intVar := func(flag, env string, dst *int) error {
		if f.Changed(flag) {
			return nil
		}
		v, ok := os.LookupEnv(env)
		if !ok {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid %s=%q: %w", env, v, err)
		}
		*dst = n
		return nil
	}

	str("url", "GITHUB_URL", &cfg.RegistrationURL)
	str("name", "SCALE_SET_NAME", &cfg.ScaleSetName)
	str("runner-group", "RUNNER_GROUP", &cfg.RunnerGroup)
	str("token", "GITHUB_TOKEN", &cfg.Token)
	str("app-client-id", "GITHUB_APP_CLIENT_ID", &cfg.GitHubApp.ClientID)
	str("app-private-key", "GITHUB_APP_PRIVATE_KEY", &cfg.GitHubApp.PrivateKey)
	str("runner-version", "RUNNER_VERSION", &cfg.RunnerVersion)
	str("extra-setup", "EXTRA_SETUP", &cfg.ExtraSetup)
	str("log-level", "LOG_LEVEL", &cfg.LogLevel)
	str("log-format", "LOG_FORMAT", &cfg.LogFormat)

	if !f.Changed("extra-packages") {
		if v, ok := os.LookupEnv("EXTRA_PACKAGES"); ok && v != "" {
			cfg.ExtraPackages = splitList(v)
		}
	}

	if !f.Changed("labels") {
		if v, ok := os.LookupEnv("RUNNER_LABELS"); ok && v != "" {
			cfg.Labels = splitList(v)
		}
	}
	if !f.Changed("app-installation-id") {
		if v, ok := os.LookupEnv("GITHUB_APP_INSTALLATION_ID"); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid GITHUB_APP_INSTALLATION_ID=%q: %w", v, err)
			}
			cfg.GitHubApp.InstallationID = n
		}
	}
	if !f.Changed("enable-private-networking") {
		if v, ok := os.LookupEnv("ENABLE_PRIVATE_NETWORKING"); ok {
			cfg.EnablePrivateNetworking = truthy(v)
		}
	}
	if err := intVar("min-runners", "MIN_RUNNERS", &cfg.MinRunners); err != nil {
		return err
	}
	if err := intVar("max-runners", "MAX_RUNNERS", &cfg.MaxRunners); err != nil {
		return err
	}
	if err := intVar("idle-timeout-minutes", "IDLE_TIMEOUT_MINUTES", &cfg.IdleTimeoutMinutes); err != nil {
		return err
	}
	return nil
}

func run(ctx context.Context, cfg *config.Config) error {
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
	instructions := cfg.RunnerTemplateInstructions()
	// Runners boot from a prebuilt, content-addressed template image. Ensure it
	// exists before listening; Railway reuses the cached build if it already does.
	logger.Info("building runner template", "runnerVersion", cfg.RunnerVersion)
	if err := railwayClient.BuildTemplateUntilReady(ctx, instructions); err != nil {
		return fmt.Errorf("build runner template: %w", err)
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
		Logger:               logger.WithGroup("scaler"),
	})
	defer sc.Shutdown(context.WithoutCancel(ctx))

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

// splitList parses a comma/whitespace-separated env value, dropping empties.
func splitList(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func hostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return uuid.NewString()
}
