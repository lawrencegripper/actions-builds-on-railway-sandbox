// Package config holds CLI/runtime configuration and constructs the scaleset
// client, Railway client, and runner template.
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/actions/scaleset"

	"github.com/lawrencegripper/railway-sandbox-github-actions-runner/internal/railway"
)

const (
	defaultRunnerWorkdir = "/actions-runner"

	latestRunnerReleaseURL = "https://api.github.com/repos/actions/runner/releases/latest"

	// These values are interpolated into shell build steps, so constrain them to
	// safe shapes (defense-in-depth against injection even though only the
	// operator sets them).
	runnerVersionPattern = `^[0-9]+\.[0-9]+\.[0-9]+$`
	runnerWorkdirPattern = `^/[A-Za-z0-9._/-]+$`

	// RunnerStartCommand is the start script baked into the template image. It
	// fixes the sandbox's minimal PATH, starts dockerd, then runs the ephemeral
	// runner.
	RunnerStartCommand = "/usr/local/bin/start-runner"
)

// baseToolchain is installed into every runner image: generic tools most
// workflows expect, including docker for container/service jobs. The actions
// runner's own .NET deps are handled separately by installdependencies.sh.
var (
	runnerVersionRe = regexp.MustCompile(runnerVersionPattern)
	runnerWorkdirRe = regexp.MustCompile(runnerWorkdirPattern)
)

var baseToolchain = []string{
	"bash",
	"ca-certificates",
	"curl",
	"docker.io",
	"git",
	"jq",
	"make",
	"nodejs",
	"sudo",
	"tar",
	"unzip",
	"xz-utils",
}

type Config struct {
	// GitHub / scaleset
	RegistrationURL string
	ScaleSetName    string
	Labels          []string
	RunnerGroup     string
	GitHubApp       scaleset.GitHubAppAuth
	Token           string
	MinRunners      int
	MaxRunners      int

	// Railway
	RailwayToken            string // account/workspace token (Bearer)
	RailwayProjectToken     string // environment-scoped project token (preferred for deploys)
	RailwayEnvironmentID    string
	RailwayEndpoint         string
	IdleTimeoutMinutes      int
	EnablePrivateNetworking bool // attach sandboxes to the Railway private network

	// Runner image
	RunnerVersion string // actions runner version; empty resolves the latest release
	RunnerWorkdir string
	ExtraPackages []string // additional apt packages baked into the image
	ExtraSetup    string   // extra shell run as the last image build step

	LogLevel  string
	LogFormat string
}

func (c *Config) Defaults() {
	if c.RunnerGroup == "" {
		c.RunnerGroup = scaleset.DefaultRunnerGroup
	}
	if c.RunnerWorkdir == "" {
		c.RunnerWorkdir = defaultRunnerWorkdir
	}
}

// ApplyEnv fills config from environment variables for any flag the caller
// reports as not explicitly set (via changed), so the service can be driven
// entirely by Railway variables. Explicit flags win over env. Pass a predicate
// that always returns false to read purely from env (e.g. contexts with no flags).
func (c *Config) ApplyEnv(changed func(flag string) bool) error {
	str := func(flag, env string, dst *string) {
		if !changed(flag) {
			if v, ok := os.LookupEnv(env); ok {
				*dst = v
			}
		}
	}
	str("url", "GITHUB_URL", &c.RegistrationURL)
	str("name", "SCALE_SET_NAME", &c.ScaleSetName)
	str("runner-group", "RUNNER_GROUP", &c.RunnerGroup)
	str("token", "GITHUB_TOKEN", &c.Token)
	str("app-client-id", "GITHUB_APP_CLIENT_ID", &c.GitHubApp.ClientID)
	str("app-private-key", "GITHUB_APP_PRIVATE_KEY", &c.GitHubApp.PrivateKey)
	str("runner-version", "RUNNER_VERSION", &c.RunnerVersion)
	str("extra-setup", "EXTRA_SETUP", &c.ExtraSetup)
	str("log-level", "LOG_LEVEL", &c.LogLevel)
	str("log-format", "LOG_FORMAT", &c.LogFormat)
	str("railway-token", "RAILWAY_API_TOKEN", &c.RailwayToken)
	str("railway-project-token", "RAILWAY_PROJECT_TOKEN", &c.RailwayProjectToken)
	str("railway-environment-id", "RAILWAY_ENVIRONMENT_ID", &c.RailwayEnvironmentID)
	str("railway-endpoint", "RAILWAY_GRAPHQL_ENDPOINT", &c.RailwayEndpoint)

	if !changed("labels") {
		if v, ok := os.LookupEnv("RUNNER_LABELS"); ok && v != "" {
			c.Labels = splitList(v)
		}
	}
	if !changed("extra-packages") {
		if v, ok := os.LookupEnv("EXTRA_PACKAGES"); ok && v != "" {
			c.ExtraPackages = splitList(v)
		}
	}
	if !changed("enable-private-networking") {
		if v, ok := os.LookupEnv("ENABLE_PRIVATE_NETWORKING"); ok {
			c.EnablePrivateNetworking = truthy(v)
		}
	}
	if !changed("app-installation-id") {
		if v, ok := os.LookupEnv("GITHUB_APP_INSTALLATION_ID"); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid GITHUB_APP_INSTALLATION_ID=%q: %w", v, err)
			}
			c.GitHubApp.InstallationID = n
		}
	}
	if err := intEnv(changed, "min-runners", "MIN_RUNNERS", &c.MinRunners); err != nil {
		return err
	}
	if err := intEnv(changed, "max-runners", "MAX_RUNNERS", &c.MaxRunners); err != nil {
		return err
	}
	return intEnv(changed, "idle-timeout-minutes", "IDLE_TIMEOUT_MINUTES", &c.IdleTimeoutMinutes)
}

func intEnv(changed func(string) bool, flag, env string, dst *int) error {
	if changed(flag) {
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

func (c *Config) Validate() error {
	c.Defaults()

	if _, err := url.ParseRequestURI(c.RegistrationURL); err != nil {
		return fmt.Errorf("invalid --url: %w (e.g. https://github.com/org/repo)", err)
	}
	appErr := c.GitHubApp.Validate()
	if c.Token == "" && appErr != nil {
		return fmt.Errorf("no GitHub credentials: provide a GitHub App (--app-client-id/--app-installation-id/--app-private-key) or --token")
	}
	if c.ScaleSetName == "" {
		return fmt.Errorf("--name (scale set name) is required")
	}
	if c.MaxRunners < 1 {
		return fmt.Errorf("--max-runners must be at least 1")
	}
	if c.MaxRunners < c.MinRunners {
		return fmt.Errorf("--max-runners cannot be less than --min-runners")
	}
	if c.RailwayToken == "" && c.RailwayProjectToken == "" {
		return fmt.Errorf("Railway token required: a project token (RAILWAY_PROJECT_TOKEN) or account token (RAILWAY_API_TOKEN)")
	}
	if c.RailwayEnvironmentID == "" {
		return fmt.Errorf("Railway environment id required (--railway-environment-id or RAILWAY_ENVIRONMENT_ID)")
	}
	if !runnerWorkdirRe.MatchString(c.RunnerWorkdir) {
		return fmt.Errorf("--runner-workdir must be an absolute path of [A-Za-z0-9._/-]: %q", c.RunnerWorkdir)
	}
	return nil
}

func (c *Config) Logger() *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if strings.ToLower(c.LogFormat) == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

func systemInfo(scaleSetID int) scaleset.SystemInfo {
	return scaleset.SystemInfo{
		System:     "railway-sandbox-actions",
		Subsystem:  "railway-sandbox-actions",
		CommitSHA:  "NA",
		Version:    "0.1.0",
		ScaleSetID: scaleSetID,
	}
}

func (c *Config) ScalesetClient() (*scaleset.Client, error) {
	if err := c.GitHubApp.Validate(); err == nil {
		return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: c.RegistrationURL,
			GitHubAppAuth:   c.GitHubApp,
			SystemInfo:      systemInfo(0),
		})
	}
	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     c.RegistrationURL,
		PersonalAccessToken: c.Token,
		SystemInfo:          systemInfo(0),
	})
}

func (c *Config) RailwayClient(logger *slog.Logger) (*railway.Client, error) {
	token, authType := c.RailwayToken, railway.AuthBearer
	if c.RailwayProjectToken != "" {
		token, authType = c.RailwayProjectToken, railway.AuthProjectToken
	}
	return railway.NewClient(railway.Config{
		Token:           token,
		AuthType:        authType,
		EnvironmentID:   c.RailwayEnvironmentID,
		GraphQLEndpoint: c.RailwayEndpoint,
		Logger:          logger,
	})
}

func (c *Config) NetworkIsolation() railway.NetworkIsolation {
	if c.EnablePrivateNetworking {
		return railway.NetworkPrivate
	}
	return railway.NetworkIsolated
}

func (c *Config) BuildLabels() []scaleset.Label {
	if len(c.Labels) > 0 {
		labels := make([]scaleset.Label, 0, len(c.Labels))
		for _, name := range c.Labels {
			if n := strings.TrimSpace(name); n != "" {
				labels = append(labels, scaleset.Label{Name: n})
			}
		}
		return labels
	}
	return []scaleset.Label{{Name: c.ScaleSetName}}
}

// ResolveRunnerVersion normalizes RunnerVersion, fetching the latest actions
// runner release when it is unset. The GitHub PAT (when provided) authenticates
// the release lookup so it is not subject to the low unauthenticated rate limit.
func (c *Config) ResolveRunnerVersion(ctx context.Context) error {
	if c.RunnerVersion != "" {
		c.RunnerVersion = strings.TrimPrefix(c.RunnerVersion, "v")
		return c.validateRunnerVersion()
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestRunnerReleaseURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("resolve latest runner release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("resolve latest runner release: GitHub returned %s", resp.Status)
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return fmt.Errorf("decode latest runner release: %w", err)
	}
	if release.TagName == "" {
		return fmt.Errorf("latest runner release has no tag_name")
	}
	c.RunnerVersion = strings.TrimPrefix(release.TagName, "v")
	return c.validateRunnerVersion()
}

func (c *Config) validateRunnerVersion() error {
	if !runnerVersionRe.MatchString(c.RunnerVersion) {
		return fmt.Errorf("runner version must look like X.Y.Z: %q", c.RunnerVersion)
	}
	return nil
}

// RunnerTemplateInstructions builds the runner image, content-addressed and
// cached by Railway so identical instructions are reused across runners: install
// the base toolchain (+ extras), the actions runner and its deps, then bake in
// the start script. Each entry is a separate build step (its own shell).
func (c *Config) RunnerTemplateInstructions() []string {
	tarball := fmt.Sprintf("actions-runner-linux-x64-%s.tar.gz", c.RunnerVersion)
	downloadURL := fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/%s", c.RunnerVersion, tarball)
	packages := append(append([]string{}, baseToolchain...), c.ExtraPackages...)

	steps := []string{
		"apt-get update",
		"DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends " + strings.Join(packages, " "),
		fmt.Sprintf("mkdir -p %s", c.RunnerWorkdir),
		fmt.Sprintf("curl -fsSL -o /tmp/runner.tar.gz %s", downloadURL),
		fmt.Sprintf("tar xzf /tmp/runner.tar.gz -C %s", c.RunnerWorkdir),
		"rm -f /tmp/runner.tar.gz",
		fmt.Sprintf("%s/bin/installdependencies.sh", c.RunnerWorkdir),
		c.writeStartScriptStep(),
	}
	if c.ExtraSetup != "" {
		steps = append(steps, c.ExtraSetup)
	}
	return steps
}

// writeStartScriptStep emits the build step that bakes RunnerStartCommand into
// the image. The script normalizes PATH (sandboxes can boot with a minimal env),
// starts dockerd for container/service jobs, then execs the ephemeral runner —
// run.sh reads the JIT config and RUNNER_ALLOW_RUNASROOT from the exec environment.
func (c *Config) writeStartScriptStep() string {
	script := fmt.Sprintf(`#!/usr/bin/env bash
set -Eeuo pipefail
export PATH="${PATH:-/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin:/usr/local/sbin}"
export SHELL="${SHELL:-/bin/bash}"
: "${ACTIONS_RUNNER_INPUT_JITCONFIG:?ACTIONS_RUNNER_INPUT_JITCONFIG must be set}"

if command -v dockerd >/dev/null 2>&1; then
  dockerd >/tmp/dockerd.log 2>&1 &
  for _ in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
fi

cd %s
exec ./run.sh
`, c.RunnerWorkdir)
	return fmt.Sprintf("cat > %s <<'EOF'\n%sEOF\nchmod +x %s", RunnerStartCommand, script, RunnerStartCommand)
}
