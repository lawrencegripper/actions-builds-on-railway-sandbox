package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/railway"
)

const probeRegistrationToken = "test-bootstrap-registration-unique-value"
const probeTailscaleKey = "test-bootstrap-tailscale-unique-value"

type bootstrapAPI struct{ fakeForgejoAPI }

func (*bootstrapAPI) RegistrationToken(context.Context) (string, error) {
	return probeRegistrationToken, nil
}
func (*bootstrapAPI) AuthorizeJob(context.Context, Job) error { return nil }

type bootstrapKeys struct{}

func (bootstrapKeys) AuthKey(context.Context) (string, error) { return probeTailscaleKey, nil }

type bootstrapSandbox struct {
	*fakeSandboxService
	t      *testing.T
	dir    string
	binary string
}

func (s *bootstrapSandbox) Exec(ctx context.Context, _ string, command string, options railway.ExecOptions) (*railway.ExecResult, error) {
	s.t.Helper()
	// Keep a wrapper alive deliberately, rather than relying on shell exec optimizations.
	wrapper := `echo $$ > "$PROBE_DIR/wrapper.pid"; bash -c "$1"; status=$?; exit "$status"`
	command = strings.ReplaceAll(command, "/usr/local/bin/start-forgejo-runner", shellQuote(filepath.Join(s.dir, "start-forgejo-runner")))
	child := exec.CommandContext(ctx, "bash", "-c", wrapper, "wrapper", command)
	child.Env = []string{"PATH=" + s.dir + ":" + os.Getenv("PATH"), "PROBE_DIR=" + s.dir, "PROBE_BINARY=" + s.binary, "PROBE_STOP_PID=" + strconv.Itoa(os.Getpid()), "GORACE=atexit_sleep_ms=0"}
	for key, value := range options.Env {
		if key != "PATH" {
			child.Env = append(child.Env, key+"="+value)
		}
	}
	child.Stdin = bytes.NewReader(options.Stdin)
	output, err := child.CombinedOutput()
	if err != nil {
		s.t.Logf("bootstrap fixture error: %v: %s", err, output)
		return nil, fmt.Errorf("bootstrap fixture: %w: %s", err, output)
	}
	return &railway.ExecResult{}, nil
}

type processProbe struct {
	Role        string
	PID         int
	Environment []string
	CommandLine []string
}

func TestBootstrapCredentialsAbsentWhenJobBegins(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux /proc")
	}
	for _, restricted := range []bool{false, true} {
		t.Run(fmt.Sprintf("restricted=%t", restricted), func(t *testing.T) {
			dir := t.TempDir()
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "start-forgejo-runner"), []byte(forgejoRunnerStartScript), 0700); err != nil {
				t.Fatal(err)
			}
			stub := `#!/bin/bash
set -eu
name="${0##*/}"
case "$name" in
  dockerd|tailscaled) exec "$PROBE_BINARY" -test.run=TestBootstrapProcessHelper -- daemon "$name" ;;
  docker) test -f "$PROBE_DIR/dockerd.pid" ;;
  tailscale)
    if [[ "$1" == up ]]; then exec "$PROBE_BINARY" -test.run=TestBootstrapProcessHelper -- join "$@"; fi
    ;;
  forgejo-runner)
    if [[ "$1" == --version ]]; then echo 'forgejo-runner version v13.2.1'; exit 0; fi
    exec "$PROBE_BINARY" -test.run=TestBootstrapProcessHelper -- "$@"
    ;;
  seq) echo 1 ;;
esac
`
			for _, name := range []string{"dockerd", "tailscaled", "docker", "tailscale", "forgejo-runner", "mkdir", "chmod", "seq", "curl"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				for _, name := range []string{"dockerd", "tailscaled"} {
					raw, _ := os.ReadFile(filepath.Join(dir, name+".pid"))
					pid, _ := strconv.Atoi(string(raw))
					if pid > 0 {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				}
			})
			cfg := restrictedConfig(t, "https://forgejo.example")
			cfg.RunnerTimeout = 20 * time.Second
			if !restricted {
				cfg.RestrictedPolicy = nil
			}
			service := &bootstrapSandbox{fakeSandboxService: &fakeSandboxService{}, t: t, dir: dir, binary: binary}
			controller := NewController(cfg, &bootstrapAPI{}, bootstrapKeys{}, fakeImages{}, service, railway.NetworkIsolated, nil, nil)
			controller.runOne(context.Background(), Job{ID: 7, RepoID: 42, RunID: 123, Attempt: 1, Handle: testHandle, Name: "deploy-k3s", Status: "waiting"}, 1, "unused", RunnerImage{Name: "base", Kind: "base", Version: "13.2.1"})
			raw, err := os.ReadFile(filepath.Join(dir, "report.json"))
			if err != nil {
				t.Fatalf("job never reached probe: %v", err)
			}
			var probes []processProbe
			if err := json.Unmarshal(raw, &probes); err != nil {
				t.Fatal(err)
			}
			roles := map[string]bool{}
			for _, probe := range probes {
				roles[probe.Role] = true
				t.Logf("%s: environment=%v commandLine=%v", probe.Role, probe.Environment, probe.CommandLine)
				if len(probe.Environment) > 0 || len(probe.CommandLine) > 0 {
					t.Errorf("bootstrap credentials survive in %s: environment=%v commandLine=%v", probe.Role, probe.Environment, probe.CommandLine)
				}
			}
			for _, role := range []string{"runner", "supervisor", "wrapper", "dockerd", "tailscaled"} {
				if !roles[role] {
					t.Errorf("missing %s probe", role)
				}
			}
		})
	}
}

func TestBootstrapProcessHelper(t *testing.T) {
	dir := os.Getenv("PROBE_DIR")
	if dir == "" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		t.Fatal("missing helper role")
	}
	args = args[1:]
	savePID := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name+".pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			t.Fatal(err)
		}
	}
	switch args[0] {
	case "daemon":
		savePID(args[1])
		time.Sleep(time.Minute)
		os.Exit(0)
	case "register", "join":
		expected := probeRegistrationToken
		if args[0] == "join" {
			expected = probeTailscaleKey
		}
		if !strings.Contains(strings.Join(args, " "), expected) {
			t.Fatal("bootstrap command did not receive its credential")
		}
		savePID(args[0])
		os.Exit(0)
	case "one-job":
		if os.Getenv("FORGEJO_RUNNER_RESTRICTED") == "true" && (len(args) != 3 || args[1] != "--handle" || args[2] != testHandle) {
			t.Fatal("restricted job not bound")
		}
	default:
		t.Fatalf("unknown helper role %q", args[0])
	}
	stdin, err := os.Readlink("/proc/self/fd/0")
	if err != nil || stdin != "/dev/null" {
		t.Fatalf("job inherited bootstrap stdin: %q (%v)", stdin, err)
	}
	var probes []processProbe
	readProbe := func(role string, pid int) {
		p := processProbe{Role: role, PID: pid}
		for _, item := range []struct {
			name string
			dst  *[]string
		}{{"environ", &p.Environment}, {"cmdline", &p.CommandLine}} {
			data, err := os.ReadFile(fmt.Sprintf("/proc/%d/%s", pid, item.name))
			if err != nil {
				t.Fatal(err)
			}
			if role == "ancestor" && item.name == "cmdline" && bytes.Contains(data, []byte("\x00--run-job\x00")) {
				p.Role = "supervisor"
			}
			for name, value := range map[string]string{"registration": probeRegistrationToken, "tailscale": probeTailscaleKey} {
				if bytes.Contains(data, []byte(value)) {
					*item.dst = append(*item.dst, name)
				}
			}
		}
		probes = append(probes, p)
	}
	readPID := func(name string) int {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			raw, err := os.ReadFile(filepath.Join(dir, name+".pid"))
			if err == nil {
				pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
				if err == nil {
					return pid
				}
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("missing %s pid", name)
		return 0
	}
	for _, role := range []string{"register", "join"} {
		pid := readPID(role)
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); !os.IsNotExist(err) {
			t.Fatalf("credential-bearing %s process still exists at job start", role)
		}
	}
	wrapper := readPID("wrapper")
	stop, _ := strconv.Atoi(os.Getenv("PROBE_STOP_PID"))
	for pid := os.Getpid(); pid > 0; {
		role := "ancestor"
		if pid == os.Getpid() {
			role = "runner"
		}
		if pid == wrapper {
			role = "wrapper"
		}
		readProbe(role, pid)
		if pid == stop {
			break
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			t.Fatal(err)
		}
		next := 0
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "PPid:") {
				next, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
			}
		}
		if next <= 0 {
			t.Fatal("probe did not reach test process")
		}
		pid = next
	}
	for _, name := range []string{"dockerd", "tailscaled"} {
		readProbe(name, readPID(name))
	}
	raw, err := json.Marshal(probes)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestBootstrapRejectsIncompleteCredentials(t *testing.T) {
	for _, input := range []string{"", "key", "key\x00", "key\x00token", "\x00token\x00", "key\x00\x00"} {
		t.Run(fmt.Sprintf("bytes=%d:%q", len(input), input), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "start-forgejo-runner")
			if err := os.WriteFile(path, []byte(forgejoRunnerStartScript), 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"forgejo-runner", "tailscale"} {
				stub := "#!/bin/bash\nprintf '%s\\n' \"$*\" >> \"$TEST_CALLS\"\n"
				if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0700); err != nil {
					t.Fatal(err)
				}
			}
			calls := filepath.Join(dir, "calls")
			command := exec.Command("bash", path)
			command.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "FORGEJO_URL=https://forgejo.example", "FORGEJO_RUNNER_LABELS=build:host", "RUNNER_NAME=test", "TEST_CALLS=" + calls}
			command.Stdin = strings.NewReader(input)
			if output, err := command.CombinedOutput(); err == nil {
				t.Fatalf("incomplete bootstrap accepted: %s", output)
			}
			raw, _ := os.ReadFile(calls)
			if strings.Contains(string(raw), "register") || strings.Contains(string(raw), "one-job") || strings.Contains(string(raw), "up ") {
				t.Fatal("incomplete credentials reached bootstrap/job execution")
			}
		})
	}
}
