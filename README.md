# railway-sandbox-actions

Run GitHub Actions jobs as ephemeral [Railway sandboxes](https://docs.railway.com), driven by the GitHub Actions [runner scale set](https://github.com/actions/scaleset) API.

Each job gets its own fresh sandbox. The service registers a scale set with GitHub, listens for scaling signals, provisions a sandbox per assigned job, runs the actions runner inside it for one job, and destroys the sandbox when the job finishes. No Kubernetes, no `actions-runner-controller`.

Railway [bills by usage](https://docs.railway.com/reference/pricing/understanding-your-bill): the CPU and memory a workload actually consumes, not capacity you reserve. A sandbox only exists for the length of a job, so there is no idle cost between jobs. A job that spends its time waiting, blocked on I/O or sitting on a `sleep`, costs next to nothing while it waits, where an always-on runner pool bills whether or not it is working. Sandboxes run on Railway's fast CPUs, which helps build-heavy jobs.

## How it works

```
GitHub Actions  ──scale set API──▶  this service  ──GraphQL──▶  Railway
   (job queued)                     (listener)                  (sandbox create)
                                         │
                                         ├─ GenerateJitRunnerConfig (per job)
                                         ├─ CreateSandbox (runner template)
                                         ├─ exec ./run.sh  (ws /ws/exec, JIT config in env)
                                         └─ DestroySandbox (on JobCompleted)
```

- The [`actions/scaleset`](https://github.com/actions/scaleset) Go client handles the GitHub-side protocol: GitHub App / PAT auth and token refresh, scale-set CRUD, the long-poll message session, and JIT runner configs.
- `internal/railway` is a Go port of the official [TypeScript SDK](https://www.npmjs.com/package/railway): sandbox create/get/destroy, template builds, shell-token minting, and the `/ws/exec` WebSocket bridge.
- `internal/scaler` implements `listener.Scaler`. It maps `statistics.TotalAssignedJobs` to a target runner count and provisions or destroys sandboxes to match.

The runner starts with `ACTIONS_RUNNER_INPUT_JITCONFIG` in its environment, so `run.sh` registers just-in-time, runs one job, and exits.

## Build

```bash
go build ./cmd/railway-sandbox-actions
```

## Run

```bash
railway-sandbox-actions \
  --url https://github.com/my-org/my-repo \
  --name railway-runners \
  --max-runners 5 \
  --token "$GITHUB_PAT"
```

Then target it from a workflow:

```yaml
jobs:
  build:
    runs-on: railway-runners   # the --name value (or a --labels entry)
```

All flags and environment variables are in [docs/configuration.md](docs/configuration.md).

## Deploy on Railway

The repo ships a `Dockerfile` (multi-stage, static binary on distroless) and a `railway.json`, and is configured entirely through environment variables. It serves a small status dashboard on `PORT` (see below), so attach a volume to persist the dashboard history across deploys.

```bash
railway up
```

Then set the service's variables. At a minimum: a Railway token, `GITHUB_URL`, `SCALE_SET_NAME`, and a GitHub credential (`RAILWAY_ENVIRONMENT_ID` is auto-injected). See [docs/configuration.md](docs/configuration.md) for the full list, and `.env.example` for a starting point.

`.railway/railway.ts` defines the whole project (service, a volume for the dashboard, and the input variables) as Railway Infrastructure-as-Code. Apply it with `bun install && railway config apply`.

## Dashboard

The service serves a read-only status page on `PORT` (default `8080` locally): live runner counts, jobs over the last 7 days, durations, recent jobs (each linked to its Railway sandbox when `RAILWAY_PROJECT_ID` and `RAILWAY_ENVIRONMENT_ID` are present), and the recent template builds with a per-build detail page at `/builds/<id>` for debugging failures. It records history to SQLite at `$RAILWAY_VOLUME_MOUNT_PATH/dashboard.db` when a Railway volume is attached, falling back to `./data/dashboard.db` for local runs. There is no auth, so put it behind a private network or add one if the domain is public.

Railway's API does not expose per-line template build logs (the `SandboxTemplateBuild` resource is only a content-addressed id plus a status), so the build detail page shows the resolved runner version, the recipe, status, timing, and any error instead.

## Smoke test (Railway transport only, no GitHub)

Checks sandbox create, exec, and destroy against the live API:

```bash
export RAILWAY_API_TOKEN=... RAILWAY_ENVIRONMENT_ID=...
go run ./cmd/railway-sandbox-actions smoke --command "uname -a"
```

## Tests

```bash
go test ./...
```

The tests cover the GraphQL client (against an in-process server), the `/ws/exec` protocol (against a real WebSocket server), and the scaler state machine (reserve, idle, busy, destroy; the max-runner cap; no over-provisioning). They run offline.

## Runner image (prebuilt template)

Each runner boots from a prebuilt sandbox template rather than a bare base image. On startup the service builds the template once (`BuildTemplateUntilReady`). Railway content-addresses the build, so every `Sandbox.create` forks the cached image instead of rebuilding.

The image (`config.RunnerTemplateInstructions`, Debian/Ubuntu base) contains:

- a base toolchain: `bash`, `git`, `curl`, `jq`, `make`, `nodejs`, `docker.io`, `unzip`, `xz-utils`, `sudo`, plus the runner's own .NET dependencies (via `installdependencies.sh`);
- the actions runner (linux-x64, `RUNNER_VERSION` or the latest release), extracted to `/actions-runner`;
- a start script at `/usr/local/bin/start-runner` that fixes the sandbox's PATH, starts `dockerd` (for `container:`/service jobs and image builds), then runs the runner.

Customise the image without forking:

- `EXTRA_PACKAGES`: extra apt packages, for example `python3,ffmpeg`.
- `EXTRA_SETUP`: a shell step run last, for example `pip install uv`.

The build is content-addressed, so changing any of these (or the runner version) produces a new cached image on the next start.

## Security notes

- Untrusted job code runs inside the sandbox as root, with dockerd. The sandbox is the isolation boundary; it is created per job and destroyed on completion. `ENABLE_PRIVATE_NETWORKING` widens the blast radius: with it on, a malicious workflow can reach your environment's private services, so enable it only for trusted repos.
- Pin `RUNNER_VERSION` in production. Left unset it tracks the latest `actions/runner` release; pinning gives reproducible builds. The tarball is fetched over HTTPS from github.com with no separate checksum step, so pinning is your supply-chain control.
- `EXTRA_SETUP` and `EXTRA_PACKAGES` run as shell at image-build time, so treat them as trusted operator input. `RUNNER_VERSION` and `--runner-workdir` are validated (semver and absolute path) before they reach a shell step.
- Secrets (Railway tokens, GitHub PAT or App key, the exec JWT, the JIT config) travel in HTTP headers, WebSocket subprotocols, or the exec env frame, never on a command line. Job stdout/stderr is not written to the orchestrator's logs.
