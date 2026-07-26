package forgejo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/railway"
)

var checkpointPartPattern = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

var basePackages = []string{
	"bash", "ca-certificates", "curl", "docker.io", "git", "iproute2", "jq",
	"make", "nodejs", "procps", "xz-utils",
}

const forgejoRunnerStartScript = `#!/usr/bin/env bash
set -Eeuo pipefail
export PATH="${PATH:-/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin:/usr/local/sbin}"
export SHELL="${SHELL:-/bin/bash}"
: "${TS_AUTHKEY:?TS_AUTHKEY must be set}"
: "${FORGEJO_URL:?FORGEJO_URL must be set}"
: "${FORGEJO_RUNNER_REGISTRATION_TOKEN:?FORGEJO_RUNNER_REGISTRATION_TOKEN must be set}"
: "${FORGEJO_RUNNER_LABELS:?FORGEJO_RUNNER_LABELS must be set}"
RUNNER_NAME="${RUNNER_NAME:-railway-forgejo-$(hostname | cut -c1-16)}"

cleanup() {
  tailscale logout >/dev/null 2>&1 || true
  [[ -n "${TAILSCALED_PID:-}" ]] && kill "$TAILSCALED_PID" 2>/dev/null || true
}
trap cleanup EXIT

mkdir -p /var/run/tailscale /workspace
chmod 0777 /workspace
if command -v dockerd >/dev/null 2>&1 && ! docker info >/dev/null 2>&1; then
  dockerd >/tmp/dockerd.log 2>&1 &
  for _ in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
fi

tailscaled --state=mem: >/tmp/tailscaled.log 2>&1 &
TAILSCALED_PID=$!
for _ in $(seq 1 30); do tailscale status >/dev/null 2>&1 && break; sleep 1; done
tailscale up --auth-key="$TS_AUTHKEY" --hostname="$RUNNER_NAME" --accept-dns=true --accept-routes=true
unset TS_AUTHKEY

for _ in $(seq 1 30); do
  curl -fsS --max-time 5 "$FORGEJO_URL/api/healthz" >/dev/null 2>&1 && break
  sleep 1
done

ephemeral_args=()
[[ "${FORGEJO_RUNNER_EPHEMERAL:-true}" == "true" ]] && ephemeral_args+=(--ephemeral)
forgejo-runner register --instance "$FORGEJO_URL" --token "$FORGEJO_RUNNER_REGISTRATION_TOKEN" --name "$RUNNER_NAME" --labels "$FORGEJO_RUNNER_LABELS" "${ephemeral_args[@]}" --no-interactive
unset FORGEJO_RUNNER_REGISTRATION_TOKEN
forgejo-runner one-job
`

const scrubScript = `set -eux
docker ps -aq | xargs -r docker rm -f || true
rm -f /root/.docker/config.json /home/*/.docker/config.json || true
rm -f /.runner /root/.runner /workspace/.runner || true
rm -rf /workspace/* /workspace/.[!.]* /workspace/..?* || true
rm -rf /root/.cache/act /home/*/.cache/act /var/forgejo-runner-home || true
rm -rf /tmp/* /tmp/.[!.]* /tmp/..?* || true
rm -rf /var/run/tailscale /var/lib/tailscale /tmp/tailscaled.log || true
find /root /home /workspace -type f \( -name '.credentials' -o -name '.credentials_rsaparams' -o -name '.runner' -o -name 'config.json' \) -delete 2>/dev/null || true
sync
`

type SandboxService interface {
	CreateSandbox(context.Context, railway.CreateOptions) (*railway.Sandbox, error)
	DestroySandbox(context.Context, string) error
	Exec(context.Context, string, string, railway.ExecOptions) (*railway.ExecResult, error)
	CreateCheckpoint(context.Context, string, string) (*railway.Checkpoint, error)
	ListCheckpoints(context.Context) ([]railway.Checkpoint, error)
	DeleteCheckpoint(context.Context, string) error
}

type RunnerImage struct {
	Name        string
	ID          string
	Version     string
	RefreshedAt time.Time
	Kind        string
}

type ImageManager struct {
	cfg     Config
	railway SandboxService
	network railway.NetworkIsolation
	http    *http.Client
	logger  *slog.Logger
}

func NewImageManager(cfg Config, service SandboxService, network railway.NetworkIsolation, httpClient *http.Client, logger *slog.Logger) *ImageManager {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &ImageManager{cfg: cfg, railway: service, network: network, http: httpClient, logger: logger}
}

func (m *ImageManager) ResolveVersion(ctx context.Context) (string, error) {
	if m.cfg.RunnerVersion != "latest" {
		version := strings.TrimPrefix(m.cfg.RunnerVersion, "v")
		if !versionPattern.MatchString(version) {
			return "", fmt.Errorf("invalid Forgejo runner version %q", version)
		}
		return version, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.cfg.ForgejoReleaseURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "actions-builds-on-railway-sandbox")
	resp, err := m.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("resolve latest Forgejo runner: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("resolve latest Forgejo runner: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}
	version := strings.TrimPrefix(release.TagName, "v")
	if !versionPattern.MatchString(version) {
		return "", fmt.Errorf("latest Forgejo runner returned invalid version %q", release.TagName)
	}
	return version, nil
}

func (m *ImageManager) PrepareBase(ctx context.Context) (RunnerImage, error) {
	version, err := m.ResolveVersion(ctx)
	if err != nil {
		return RunnerImage{}, err
	}
	name := m.checkpointName(version, "base")
	m.logger.Info("building Forgejo base checkpoint", "name", name, "version", version)
	builder, err := m.railway.CreateSandbox(ctx, railway.CreateOptions{
		IdleTimeoutMinutes: max(m.cfg.SandboxIdleTimeoutMinutes, 30),
		NetworkIsolation:   m.network,
	})
	if err != nil {
		return RunnerImage{}, err
	}
	defer func() {
		if destroyErr := m.railway.DestroySandbox(context.WithoutCancel(ctx), builder.ID); destroyErr != nil {
			m.logger.Error("destroy Forgejo checkpoint builder", "sandbox", builder.ID, "error", destroyErr)
		}
	}()

	packages := uniqueStrings(append(append([]string{}, basePackages...), m.cfg.ExtraPackages...))
	steps := []struct{ name, command string }{
		{"install packages", aptInstallScript(packages)},
		{"install Tailscale", "set -eux\ncurl -fsSL https://tailscale.com/install.sh | sh"},
		{"install Forgejo runner", installRunnerScript(version)},
	}
	if len(m.cfg.PreloadDockerImages) > 0 || m.cfg.DockerWarmupSetup != "" {
		steps = append(steps, struct{ name, command string }{"warm Docker cache", dockerWarmupScript(m.cfg.PreloadDockerImages, m.cfg.DockerWarmupSetup)})
	}
	if m.cfg.ExtraSetup != "" {
		steps = append(steps, struct{ name, command string }{"extra setup", m.cfg.ExtraSetup})
	}
	steps = append(steps, struct{ name, command string }{"write runner start script", writeFileScript("/usr/local/bin/start-forgejo-runner", forgejoRunnerStartScript)})
	for _, step := range steps {
		if err := m.execChecked(ctx, builder.ID, step.name, step.command); err != nil {
			return RunnerImage{}, err
		}
	}
	checkpoint, err := m.railway.CreateCheckpoint(ctx, builder.ID, name)
	if err != nil {
		return RunnerImage{}, fmt.Errorf("capture base checkpoint: %w", err)
	}
	image := RunnerImage{Name: name, ID: checkpoint.ID, Version: version, RefreshedAt: time.Now(), Kind: "base"}
	if err := m.cleanup(ctx, m.familyPrefix(version, "base"), name); err != nil {
		m.logger.Warn("clean old base checkpoints", "error", err)
	}
	return image, nil
}

func (m *ImageManager) Hydrate(ctx context.Context, sandboxID string, base RunnerImage, snapshotKey string) (RunnerImage, error) {
	kind := "hydrated-" + sanitizeCheckpointPart(snapshotKey)
	name := m.checkpointName(base.Version, kind)
	if err := m.execChecked(ctx, sandboxID, "scrub completed runner", scrubScript); err != nil {
		return RunnerImage{}, err
	}
	checkpoint, err := m.railway.CreateCheckpoint(ctx, sandboxID, name)
	if err != nil {
		return RunnerImage{}, fmt.Errorf("capture hydrated checkpoint: %w", err)
	}
	image := RunnerImage{Name: name, ID: checkpoint.ID, Version: base.Version, RefreshedAt: time.Now(), Kind: kind}
	if err := m.cleanup(ctx, m.familyPrefix(base.Version, kind), name); err != nil {
		m.logger.Warn("clean old hydrated checkpoints", "family", kind, "error", err)
	}
	return image, nil
}

func (m *ImageManager) execChecked(parent context.Context, sandboxID, step, command string) error {
	ctx, cancel := context.WithTimeout(parent, m.cfg.SnapshotBuildTimeout)
	defer cancel()
	var output strings.Builder
	appendOutput := func(chunk []byte) {
		if output.Len() < 16*1024 {
			remaining := 16*1024 - output.Len()
			if len(chunk) > remaining {
				chunk = chunk[:remaining]
			}
			output.Write(chunk)
		}
	}
	m.logger.Info("running checkpoint build step", "step", step, "sandbox", sandboxID)
	result, err := m.railway.Exec(ctx, sandboxID, command, railway.ExecOptions{OnStdout: appendOutput, OnStderr: appendOutput})
	if err != nil {
		return fmt.Errorf("checkpoint step %q: %w; output: %s", step, err, output.String())
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("checkpoint step %q exited %d: %s", step, result.ExitCode, output.String())
	}
	return nil
}

func (m *ImageManager) cleanup(ctx context.Context, familyPrefix, keepName string) error {
	checkpoints, err := m.railway.ListCheckpoints(ctx)
	if err != nil {
		return err
	}
	var owned []railway.Checkpoint
	for _, checkpoint := range checkpoints {
		if strings.HasPrefix(checkpoint.Key, familyPrefix) {
			owned = append(owned, checkpoint)
		}
	}
	sort.SliceStable(owned, func(i, j int) bool {
		return checkpointTime(owned[i]).After(checkpointTime(owned[j]))
	})
	retained := map[string]bool{keepName: true}
	for _, checkpoint := range owned {
		if len(retained) >= max(1, m.cfg.RetainedSnapshots) {
			break
		}
		retained[checkpoint.Key] = true
	}
	for _, checkpoint := range owned {
		if retained[checkpoint.Key] {
			continue
		}
		if err := m.railway.DeleteCheckpoint(ctx, checkpoint.ID); err != nil {
			return err
		}
		m.logger.Info("deleted old runner checkpoint", "name", checkpoint.Key, "id", checkpoint.ID)
	}
	return nil
}

func (m *ImageManager) prefix() string { return sanitizeCheckpointPart(m.cfg.SnapshotNamePrefix) }
func (m *ImageManager) familyPrefix(version, kind string) string {
	return fmt.Sprintf("%s-%s-%s-", m.prefix(), sanitizeCheckpointPart(version), sanitizeCheckpointPart(kind))
}
func (m *ImageManager) checkpointName(version, kind string) string {
	now := time.Now().UTC()
	timestamp := now.Format("2006-01-02T15-04-05") + fmt.Sprintf("-%03dZ", now.Nanosecond()/int(time.Millisecond))
	return m.familyPrefix(version, kind) + timestamp
}

func checkpointTime(checkpoint railway.Checkpoint) time.Time {
	parsed, _ := time.Parse(time.RFC3339, checkpoint.CreatedAt)
	return parsed
}

func sanitizeCheckpointPart(value string) string {
	value = strings.Trim(checkpointPartPattern.ReplaceAllString(value, "-"), "-")
	if value == "" {
		return "snapshot"
	}
	return value
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'" }

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func aptInstallScript(packages []string) string {
	quoted := make([]string, len(packages))
	for i, item := range packages {
		quoted[i] = shellQuote(item)
	}
	return "set -eux\nexport DEBIAN_FRONTEND=noninteractive\napt-get update\napt-get install -y --no-install-recommends " + strings.Join(quoted, " ") + "\nrm -rf /var/lib/apt/lists/*"
}

func installRunnerScript(version string) string {
	return fmt.Sprintf(`set -eux
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "Unsupported arch $ARCH" >&2; exit 1 ;;
esac
curl -fsSL -o /usr/local/bin/forgejo-runner "https://code.forgejo.org/forgejo/runner/releases/download/v%s/forgejo-runner-%s-linux-$ARCH"
chmod +x /usr/local/bin/forgejo-runner
forgejo-runner --version`, version, version)
}

func dockerWarmupScript(images []string, setup string) string {
	lines := []string{"set -eux", "if ! docker info >/dev/null 2>&1; then", "  dockerd >/tmp/dockerd.log 2>&1 &", "  for _ in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done", "fi"}
	if len(images) > 0 {
		quoted := make([]string, len(images))
		for i, image := range images {
			quoted[i] = shellQuote(image)
		}
		lines = append(lines, "for image in "+strings.Join(quoted, " ")+"; do", "  timeout 10m docker pull \"$image\"", "done")
	}
	if setup != "" {
		lines = append(lines, setup)
	}
	lines = append(lines, "docker image ls")
	return strings.Join(lines, "\n")
}

func writeFileScript(path, content string) string {
	return "cat > " + shellQuote(path) + " <<'ACTIONS_BUILDS_EOF'\n" + content + "ACTIONS_BUILDS_EOF\nchmod +x " + shellQuote(path)
}
