package forgejo

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/railway"
)

func TestLiveBootstrap(t *testing.T) {
	if os.Getenv("FORGEJO_LIVE_BOOTSTRAP_TEST") != "true" {
		t.Skip("requires explicit FORGEJO_LIVE_BOOTSTRAP_TEST=true and Railway credentials")
	}
	client, err := railway.NewClient(railway.Config{
		Token: os.Getenv("RAILWAY_API_TOKEN"), EnvironmentID: os.Getenv("RAILWAY_ENVIRONMENT_ID"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	sandbox, err := client.CreateSandbox(ctx, railway.CreateOptions{
		IdleTimeoutMinutes: 5, NetworkIsolation: railway.NetworkIsolated,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("created isolated probe sandbox %s", sandbox.ID)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		if err := client.DestroySandbox(cleanup, sandbox.ID); err != nil {
			t.Errorf("destroy sandbox %s: %v", sandbox.ID, err)
			return
		}
		for {
			state, err := client.GetSandbox(cleanup, sandbox.ID)
			if err != nil {
				t.Errorf("verify sandbox %s destruction: %v", sandbox.ID, err)
				return
			}
			if state == nil || state.Status == railway.StatusDestroyed {
				t.Logf("destroyed probe sandbox %s", sandbox.ID)
				return
			}
			select {
			case <-cleanup.Done():
				t.Errorf("sandbox %s destruction not confirmed: %v", sandbox.ID, cleanup.Err())
				return
			case <-time.After(time.Second):
			}
		}
	})

	setup := "set -eu\nmkdir -p /workspace/probe-bin\n"
	for _, file := range []struct{ path, content string }{
		{"start-forgejo-runner", forgejoRunnerStartScript},
		{"curl", "#!/bin/sh\nexit 0\n"},
		{"tailscale", "#!/bin/sh\nexit 0\n"},
		{"tailscaled", "#!/bin/sh\nexec sleep 120\n"},
		{"dockerd", "#!/bin/sh\necho $$ > /workspace/dockerd.pid\ntouch /workspace/docker-ready\nexec sleep 120\n"},
		{"docker", "#!/bin/sh\ntest -f /workspace/docker-ready\n"},
		{"forgejo-runner", liveRunnerProbe},
	} {
		setup += writeFileScript("/workspace/probe-bin/"+file.path, file.content) + "\n"
	}
	var setupOutput strings.Builder
	result, err := client.Exec(ctx, sandbox.ID, setup, railway.ExecOptions{
		OnStdout: func(data []byte) { setupOutput.Write(data) },
		OnStderr: func(data []byte) { setupOutput.Write(data) },
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("prepare harmless bootstrap stubs: result=%+v error=%v output=%s", result, err, setupOutput.String())
	}
	for _, restricted := range []bool{false, true} {
		t.Run(fmt.Sprintf("restricted=%t", restricted), func(t *testing.T) {
			key := fmt.Sprintf("dummy-key-%x", randomProbeBytes(t))
			token := fmt.Sprintf("dummy-token-%x", randomProbeBytes(t))
			env := map[string]string{
				"PATH":        "/workspace/probe-bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
				"FORGEJO_URL": "https://unused.invalid", "FORGEJO_RUNNER_LABELS": "probe:host",
				"FORGEJO_RUNNER_RESTRICTED": fmt.Sprint(restricted), "FORGEJO_JOB_HANDLE": "probe-handle",
				"PROBE_KEY_HASH":   fmt.Sprintf("%x", sha256.Sum256([]byte(key))),
				"PROBE_TOKEN_HASH": fmt.Sprintf("%x", sha256.Sum256([]byte(token))),
			}
			var output strings.Builder
			run, stop := context.WithTimeout(ctx, 45*time.Second)
			defer stop()
			result, err := client.Exec(run, sandbox.ID, "exec /workspace/probe-bin/start-forgejo-runner", railway.ExecOptions{
				Env: env, Stdin: []byte(key + "\x00" + token + "\x00"),
				OnStdout: func(data []byte) { output.Write(data) },
				OnStderr: func(data []byte) { output.Write(data) },
			})
			if err != nil || result.ExitCode != 0 || !strings.Contains(output.String(), "probe-ok") {
				t.Fatalf("bootstrap probe: result=%+v error=%v output=%s", result, err, output.String())
			}
			t.Logf("native stdin and clean process ancestry: %s", strings.TrimSpace(output.String()))
		})
	}
}

func randomProbeBytes(t *testing.T) []byte {
	t.Helper()
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	return value
}

const liveRunnerProbe = `#!/bin/bash
set -euo pipefail
case "$1" in
  --version) echo 'forgejo-runner version v13.2.1'; exit ;;
  register) exit ;;
  one-job)
    if [[ "$FORGEJO_RUNNER_RESTRICTED" == true ]]; then
      [[ "$#" == 3 && "$2" == --handle && "$3" == probe-handle ]]
    else
      [[ "$#" == 1 ]]
    fi ;;
  *) exit 1 ;;
esac
scan() {
  local pid="$1" entry value digest kind
  for kind in environ cmdline; do
    while IFS= read -r -d '' entry; do
      value="${entry#*=}"
      digest=$(printf '%s' "$value" | sha256sum)
      digest="${digest%% *}"
      [[ "$digest" != "$PROBE_KEY_HASH" && "$digest" != "$PROBE_TOKEN_HASH" ]] || {
        echo "credential exposure: pid=$pid source=$kind" >&2; exit 1;
      }
    done < "/proc/$pid/$kind"
  done
}
pid=$$
while (( pid > 0 )); do
  scan "$pid"
  pid=$(awk '/^PPid:/ {print $2}' "/proc/$pid/status")
done
scan "$TAILSCALED_PID"
read -r docker_pid < /workspace/dockerd.pid
scan "$docker_pid"
[[ "$(readlink /proc/$PPID/fd/0)" == /dev/null ]]
echo probe-ok
`
