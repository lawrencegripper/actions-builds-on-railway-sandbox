package forgejo

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// Config contains Forgejo-specific controller, runner image, and Tailscale
// settings. Railway connection settings are shared in internal/config.
type Config struct {
	RestrictedPolicy     *RestrictedPolicy
	URL                  string
	APIToken             string
	JobsURL              string
	RegistrationTokenURL string
	RunnerLabels         string
	JobLabelFilter       string
	RunnerVersion        string
	ExtraPackages        []string
	ExtraSetup           string
	PreloadDockerImages  []string
	DockerWarmupSetup    string

	PollInterval              time.Duration
	SnapshotRefresh           time.Duration
	SnapshotNamePrefix        string
	SnapshotBuildTimeout      time.Duration
	RetainedSnapshots         int
	SnapshotDefaultBranchJobs bool
	MaxConcurrentSandboxes    int
	MaxLaunchPerPoll          int
	SandboxIdleTimeoutMinutes int
	RunnerTimeout             time.Duration

	TailscaleTailnet       string
	TailscaleAPIToken      string
	TailscaleOAuthClientID string
	TailscaleOAuthSecret   string
	TailscaleTag           string
	TailscaleKeyExpiry     time.Duration
	TailscaleAPIBaseURL    string
	TailscaleOAuthURL      string
	ForgejoReleaseURL      string
}

func (c *Config) Defaults() {
	c.URL = strings.TrimRight(c.URL, "/")
	if c.JobsURL == "" {
		c.JobsURL = c.URL + "/api/v1/admin/actions/runners/jobs"
	}
	if c.RegistrationTokenURL == "" {
		c.RegistrationTokenURL = c.URL + "/api/v1/admin/actions/runners/registration-token"
	}
	if c.RunnerLabels == "" {
		c.RunnerLabels = "railway-ubuntu:host"
	}
	if c.JobLabelFilter == "" {
		labels := strings.Split(c.RunnerLabels, ",")
		for i, label := range labels {
			labels[i] = strings.SplitN(label, ":", 2)[0]
		}
		c.JobLabelFilter = strings.Join(labels, ",")
	}
	if c.RunnerVersion == "" {
		c.RunnerVersion = "latest"
	}
	if c.PollInterval == 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.SnapshotRefresh == 0 {
		c.SnapshotRefresh = 24 * time.Hour
	}
	if c.SnapshotNamePrefix == "" {
		c.SnapshotNamePrefix = "forgejo-runner"
	}
	if c.SnapshotBuildTimeout == 0 {
		c.SnapshotBuildTimeout = 30 * time.Minute
	}
	if c.RetainedSnapshots == 0 {
		c.RetainedSnapshots = 3
	}
	if c.MaxConcurrentSandboxes == 0 {
		c.MaxConcurrentSandboxes = 2
	}
	if c.MaxLaunchPerPoll == 0 {
		c.MaxLaunchPerPoll = 2
	}
	if c.SandboxIdleTimeoutMinutes == 0 {
		c.SandboxIdleTimeoutMinutes = 10
	}
	if c.RunnerTimeout == 0 {
		c.RunnerTimeout = 2 * time.Hour
	}
	if c.TailscaleTag == "" {
		c.TailscaleTag = "tag:railway-forgejo-runner"
	}
	if c.TailscaleKeyExpiry == 0 {
		c.TailscaleKeyExpiry = 10 * time.Minute
	}
	if c.TailscaleAPIBaseURL == "" {
		c.TailscaleAPIBaseURL = "https://api.tailscale.com/api/v2"
	}
	if c.TailscaleOAuthURL == "" {
		c.TailscaleOAuthURL = "https://api.tailscale.com/api/v2/oauth/token"
	}
	if c.ForgejoReleaseURL == "" {
		c.ForgejoReleaseURL = "https://code.forgejo.org/api/v1/repos/forgejo/runner/releases/latest"
	}
}

// ApplyEnv applies environment values unless the corresponding command-line
// flag was explicitly set.
func (c *Config) ApplyEnv(changed func(string) bool) error {
	if raw, ok := os.LookupEnv("FORGEJO_RESTRICTED_POLICY"); ok {
		policy, err := parseRestrictedPolicy(raw)
		if err != nil {
			return err
		}
		c.RestrictedPolicy = policy
	}
	str := func(flag, name string, dst *string) {
		if !changed(flag) {
			if value, ok := os.LookupEnv(name); ok {
				*dst = value
			}
		}
	}
	str("forgejo-url", "FORGEJO_URL", &c.URL)
	str("forgejo-token", "FORGEJO_API_TOKEN", &c.APIToken)
	str("forgejo-jobs-url", "FORGEJO_JOBS_URL", &c.JobsURL)
	str("forgejo-registration-token-url", "FORGEJO_REGISTRATION_TOKEN_URL", &c.RegistrationTokenURL)
	str("runner-labels", "FORGEJO_RUNNER_LABELS", &c.RunnerLabels)
	str("job-label-filter", "FORGEJO_JOB_LABEL_FILTER", &c.JobLabelFilter)
	str("runner-version", "FORGEJO_RUNNER_VERSION", &c.RunnerVersion)
	str("extra-setup", "EXTRA_SETUP", &c.ExtraSetup)
	str("docker-warmup-setup", "DOCKER_WARMUP_SETUP", &c.DockerWarmupSetup)
	str("snapshot-name-prefix", "RUNNER_SNAPSHOT_NAME_PREFIX", &c.SnapshotNamePrefix)
	str("tailscale-tailnet", "TAILSCALE_TAILNET", &c.TailscaleTailnet)
	str("tailscale-api-token", "TAILSCALE_API_TOKEN", &c.TailscaleAPIToken)
	str("tailscale-oauth-client-id", "TAILSCALE_OAUTH_CLIENT_ID", &c.TailscaleOAuthClientID)
	str("tailscale-oauth-secret", "TAILSCALE_OAUTH_CLIENT_SECRET", &c.TailscaleOAuthSecret)
	str("tailscale-tag", "TAILSCALE_TAG", &c.TailscaleTag)
	// Test/development endpoint overrides are intentionally environment-only.
	str("", "TAILSCALE_API_BASE_URL", &c.TailscaleAPIBaseURL)
	str("", "TAILSCALE_OAUTH_URL", &c.TailscaleOAuthURL)
	str("", "FORGEJO_RUNNER_RELEASE_URL", &c.ForgejoReleaseURL)

	if !changed("extra-packages") {
		if value, ok := os.LookupEnv("EXTRA_PACKAGES"); ok {
			c.ExtraPackages = splitList(value)
		}
	}
	if !changed("preload-docker-images") {
		if value, ok := os.LookupEnv("PRELOAD_DOCKER_IMAGES"); ok {
			c.PreloadDockerImages = splitList(value)
		}
	}
	if !changed("snapshot-default-branch-jobs") {
		if value, ok := os.LookupEnv("SNAPSHOT_AFTER_DEFAULT_BRANCH_BUILD"); ok {
			c.SnapshotDefaultBranchJobs = truthy(value)
		}
	}

	seconds := []struct {
		flag string
		env  string
		dst  *time.Duration
	}{
		{"poll-interval", "POLL_INTERVAL_SECONDS", &c.PollInterval},
		{"snapshot-build-timeout", "SNAPSHOT_BUILD_TIMEOUT_SECONDS", &c.SnapshotBuildTimeout},
		{"runner-timeout", "RUNNER_TIMEOUT_SECONDS", &c.RunnerTimeout},
		{"tailscale-key-expiry", "TAILSCALE_KEY_EXPIRY_SECONDS", &c.TailscaleKeyExpiry},
	}
	for _, item := range seconds {
		if changed(item.flag) {
			continue
		}
		if value, ok := os.LookupEnv(item.env); ok {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return fmt.Errorf("%s must be a positive integer", item.env)
			}
			*item.dst = time.Duration(n) * time.Second
		}
	}
	if !changed("snapshot-refresh") {
		if value, ok := os.LookupEnv("SNAPSHOT_REFRESH_HOURS"); ok {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return fmt.Errorf("SNAPSHOT_REFRESH_HOURS must be a positive integer")
			}
			c.SnapshotRefresh = time.Duration(n) * time.Hour
		}
	}

	ints := []struct {
		flag string
		env  string
		dst  *int
	}{
		{"retained-snapshots", "RETAINED_RUNNER_SNAPSHOTS", &c.RetainedSnapshots},
		{"max-runners", "MAX_CONCURRENT_SANDBOXES", &c.MaxConcurrentSandboxes},
		{"max-launch-per-poll", "MAX_LAUNCH_PER_POLL", &c.MaxLaunchPerPoll},
		{"sandbox-idle-timeout-minutes", "SANDBOX_IDLE_TIMEOUT_MINUTES", &c.SandboxIdleTimeoutMinutes},
	}
	for _, item := range ints {
		if changed(item.flag) {
			continue
		}
		if value, ok := os.LookupEnv(item.env); ok {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return fmt.Errorf("%s must be a positive integer", item.env)
			}
			*item.dst = n
		}
	}
	c.Defaults()
	return nil
}

func (c *Config) Validate() error {
	c.Defaults()
	if err := c.validateRestricted(); err != nil {
		return err
	}
	for name, raw := range map[string]string{
		"FORGEJO_URL":                    c.URL,
		"FORGEJO_JOBS_URL":               c.JobsURL,
		"FORGEJO_REGISTRATION_TOKEN_URL": c.RegistrationTokenURL,
		"TAILSCALE_API_BASE_URL":         c.TailscaleAPIBaseURL,
		"TAILSCALE_OAUTH_URL":            c.TailscaleOAuthURL,
	} {
		parsed, err := url.ParseRequestURI(raw)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("%s is not a valid absolute URL: %q", name, raw)
		}
	}
	if c.APIToken == "" {
		return fmt.Errorf("FORGEJO_API_TOKEN is required")
	}
	if c.RunnerLabels == "" || c.JobLabelFilter == "" {
		return fmt.Errorf("Forgejo runner labels and job label filter are required")
	}
	if c.RunnerVersion != "latest" && !versionPattern.MatchString(strings.TrimPrefix(c.RunnerVersion, "v")) {
		return fmt.Errorf("FORGEJO_RUNNER_VERSION must be latest or X.Y.Z")
	}
	if c.TailscaleTailnet == "" {
		return fmt.Errorf("TAILSCALE_TAILNET is required")
	}
	if c.TailscaleAPIToken == "" && (c.TailscaleOAuthClientID == "" || c.TailscaleOAuthSecret == "") {
		return fmt.Errorf("set TAILSCALE_API_TOKEN or both TAILSCALE_OAUTH_CLIENT_ID and TAILSCALE_OAUTH_CLIENT_SECRET")
	}
	if c.MaxLaunchPerPoll > c.MaxConcurrentSandboxes {
		return fmt.Errorf("MAX_LAUNCH_PER_POLL cannot exceed MAX_CONCURRENT_SANDBOXES")
	}
	return nil
}

func splitList(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
