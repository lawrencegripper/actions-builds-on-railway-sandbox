# railway-sandbox-actions

Run GitHub Actions jobs as ephemeral [Railway sandboxes](https://docs.railway.com), driven by the GitHub Actions [runner scale set](https://github.com/actions/scaleset) API.

Each job gets its own fresh sandbox: the service registers a scale set with GitHub, listens for scaling signals, provisions a sandbox per assigned job, runs the actions runner inside it (one job, then exit), and destroys the sandbox on completion. No Kubernetes, no `actions-runner-controller`.

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

- The [`actions/scaleset`](https://github.com/actions/scaleset) Go client handles all GitHub-side protocol: GitHub App / PAT auth and token refresh, scale-set CRUD, the long-poll message session, and JIT runner configs.
- `internal/railway` is a minimal Go port of the official [TypeScript SDK](https://www.npmjs.com/package/railway): sandbox create/get/destroy, template builds, shell-token minting, and the `/ws/exec` WebSocket bridge.
- `internal/scaler` implements `listener.Scaler`: it maps `statistics.TotalAssignedJobs` to a target runner count and provisions/destroys sandboxes to match.

The runner is launched with `ACTIONS_RUNNER_INPUT_JITCONFIG` in its environment, so `run.sh` registers just-in-time, runs exactly one job, and exits — the same mechanism the upstream `dockerscaleset` example uses.

## Build

```bash
go build ./cmd/railway-sandbox-actions
```

## Configure

Railway credentials (flags or environment):

| Flag | Env | Notes |
| --- | --- | --- |
| `--railway-token` | `RAILWAY_API_TOKEN` | required |
| `--railway-environment-id` | `RAILWAY_ENVIRONMENT_ID` | required |
| `--railway-endpoint` | `RAILWAY_GRAPHQL_ENDPOINT` | defaults to prod |

GitHub credentials — a GitHub App (recommended) or a PAT:

- App: `--app-client-id`, `--app-installation-id`, `--app-private-key`
- PAT: `--token`

See the [scale set auth docs](https://docs.github.com/en/actions/tutorials/use-actions-runner-controller/authenticate-to-the-api) for the required permissions.

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

### Key flags

| Flag | Default | Purpose |
| --- | --- | --- |
| `--url` | _(required)_ | repo/org/enterprise URL to register the scale set against |
| `--name` | _(required)_ | scale set name, also the default `runs-on` label |
| `--labels` | `--name` | extra targeting labels |
| `--min-runners` | `0` | pre-provisioned (warm) runners |
| `--max-runners` | `5` | concurrent runner cap |
| `--runner-version` | latest | actions runner release (defaults to the latest) |
| `--idle-timeout-minutes` | `30` | sandbox idle timeout |
| `--enable-private-networking` | `false` | give sandboxes access to the Railway private network |

## Deploy on Railway

The repo ships a `Dockerfile` (multi-stage, static binary on distroless) and a `railway.json`. The service is a worker — no inbound port, only outbound HTTPS to GitHub and the Railway API — and is configured entirely through **environment variables**, so it works as a Railway template.

```bash
railway up        # from the repo, or point a template/service at this repo
```

### Template variables

| Variable | Required | Notes |
| --- | --- | --- |
| `RAILWAY_PROJECT_TOKEN` | yes\* | **Project token** (Project Settings → Tokens), scoped to one environment — the recommended credential. The service uses it (via the `Project-Access-Token` header) to create/destroy sandboxes. |
| `RAILWAY_API_TOKEN` | yes\* | Alternative to the project token: an account/workspace token (Bearer auth). |
| `RAILWAY_ENVIRONMENT_ID` | no | **Auto-injected** by Railway at runtime — leave unset on Railway. |
| `GITHUB_URL` | yes | repo/org/enterprise URL to register against |
| `SCALE_SET_NAME` | yes | scale set name / default `runs-on` label |
| `GITHUB_TOKEN` | yes\*\* | GitHub PAT |
| `GITHUB_APP_CLIENT_ID` / `GITHUB_APP_INSTALLATION_ID` / `GITHUB_APP_PRIVATE_KEY` | yes\*\* | GitHub App (preferred over a PAT) |
| `RUNNER_LABELS` | no | comma-separated, defaults to `SCALE_SET_NAME` |
| `MIN_RUNNERS` / `MAX_RUNNERS` | no | `0` / `5` |
| `IDLE_TIMEOUT_MINUTES` | no | `30` |
| `RUNNER_VERSION` | no | actions runner version; defaults to the latest release |
| `EXTRA_PACKAGES` | no | extra apt packages baked into the image (comma/space separated) |
| `EXTRA_SETUP` | no | extra shell run as the final image build step |
| `ENABLE_PRIVATE_NETWORKING` | no | give sandboxes access to the [Railway private network](https://docs.railway.com/networking/private-networking) |
| `LOG_LEVEL` / `LOG_FORMAT` | no | `info` / `text` (use `json` on Railway) |

\* Provide exactly one Railway token. A **project token cannot be minted by the template itself** — Railway has no token-minting template variable, and no API token is auto-injected — so create it once and add it as a template input variable. The environment id, however, *is* auto-injected.

\*\* Provide either a PAT (`GITHUB_TOKEN`) or the three GitHub App variables. Both are passed in as template input variables (mark them secret).

See `.env.example` for the full list.

### Publishing as a template

You do **not** need to publish a Docker image. A Railway template service can deploy from **either a GitHub source repo or a Docker image** — source is simpler here, since Railway builds this repo's `Dockerfile` in each deployer's project on deploy.

1. Push this repo to a **public GitHub repo**.
2. Railway → Workspace → **Create Template** (template composer) → **Add New** service → source = **GitHub repo**, pointed at your repo. It picks up `railway.json` (Dockerfile builder); no start command (worker).
3. On the **Variables** tab, add the inputs from the table above. Mark `RAILWAY_PROJECT_TOKEN`, `GITHUB_TOKEN` / `GITHUB_APP_PRIVATE_KEY` as secrets; give `MAX_RUNNERS` etc. defaults. Do **not** add `RAILWAY_ENVIRONMENT_ID` — it's auto-injected. (Template variable functions like `secret(length)` / `randomInt(min,max)` exist for generated values, but the tokens here are user-supplied.)
4. Click **Publish**, fill the form. You get a template URL and a "Deploy on Railway" button:

   ```md
   [![Deploy on Railway](https://railway.com/button.svg)](https://railway.com/template/<YOUR_TEMPLATE_CODE>)
   ```

Optional image-based path: if you'd rather not rebuild per deploy (or want to keep the source closed), push to GHCR/Docker Hub and use the **Docker Image** source instead — see Railway's "Private Docker images" docs.

## Smoke test (Railway transport only, no GitHub)

Verifies sandbox create → exec → destroy against the live API:

```bash
export RAILWAY_API_TOKEN=... RAILWAY_ENVIRONMENT_ID=...
go run ./cmd/railway-sandbox-actions smoke --command "uname -a"
```

## Tests

```bash
go test ./...
```

Unit/integration tests cover the GraphQL client (against an in-process server), the `/ws/exec` protocol (against a real WebSocket server speaking the wire format), and the scaler state machine (reserve → idle → busy → destroy, max-runner cap, no over-provisioning) — all offline.

## Runner image (prebuilt template)

Each runner boots from a **prebuilt sandbox template**, not a bare base image. On startup the service builds the template once (`BuildTemplateUntilReady`); Railway content-addresses the build, so every `Sandbox.create` forks the cached image instead of rebuilding — fast, identical runners.

The image (`config.RunnerTemplateInstructions`, Debian/Ubuntu base) bakes in:

- a base toolchain: `bash`, `git`, `curl`, `jq`, `make`, `nodejs`, `docker.io`, `unzip`, `xz-utils`, `sudo`, … (plus the runner's own .NET deps via `installdependencies.sh`);
- the actions runner (linux-x64, `RUNNER_VERSION` or the latest release), extracted to `--runner-workdir` (`/actions-runner`);
- a start script at `/usr/local/bin/start-runner` that normalizes the sandbox's PATH, starts `dockerd` (for `container:`/service jobs and image builds), then runs the ephemeral runner.

Customize the image without forking:

- **`EXTRA_PACKAGES`** — extra apt packages baked in (e.g. `python3,ffmpeg`).
- **`EXTRA_SETUP`** — an arbitrary shell step run last (e.g. `pip install uv`).

Because the template is content-addressed, changing any of these (or the runner version) produces a new cached image automatically — no manual rebuild.

## Security notes

- **Untrusted job code runs inside the sandbox as root, with dockerd.** The sandbox is the isolation boundary; it is created per job and destroyed on completion. `--enable-private-networking` widens the blast radius — with it on, a malicious workflow can reach your environment's private services, so enable it only for trusted repos.
- **Pin `RUNNER_VERSION` in production.** Left unset it tracks the latest `actions/runner` release; pinning gives reproducible, reviewable builds. The runner tarball is fetched over HTTPS from github.com; there is no separate checksum step, so pinning is your supply-chain control.
- **`EXTRA_SETUP`/`EXTRA_PACKAGES` are shell run at image-build time** — treat them as trusted operator input. `RUNNER_VERSION` and `--runner-workdir` are validated (semver / absolute-path) before they reach a shell step.
- **Secrets** (Railway tokens, GitHub PAT/App key, the exec JWT, the JIT config) travel in HTTP headers, WebSocket subprotocols, or the exec env frame — never in a command line — and job stdout/stderr is not streamed into the orchestrator's logs.
