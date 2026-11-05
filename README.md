# actions-builds-on-railway-sandbox

Run GitHub Actions or Forgejo Actions builds in fresh, ephemeral [Railway sandboxes](https://docs.railway.com/sandboxes). Each runner handles one job and its sandbox is then destroyed.

Railway bills sandbox CPU and memory by actual usage. There is no idle runner VM pool and no Kubernetes dependency such as `actions-runner-controller`.

## Providers

### GitHub

GitHub mode uses the official [`actions/scaleset`](https://github.com/actions/scaleset) Go client. The service creates a runner scale set, consumes desired-capacity and lifecycle messages, mints a JIT runner config, and starts one sandbox per runner.

```text
GitHub scale-set API ──▶ GitHub controller ──▶ Railway template sandbox
                                │                       │
                                └── JIT config ─────────┘
```

### Forgejo

Forgejo mode polls the Forgejo 15+ admin runner-job endpoint. For each waiting job it obtains a reusable runner registration token and creates an ephemeral Tailscale key, then starts a one-job runner from a Railway checkpoint.

```text
Forgejo admin API ──▶ Forgejo controller ──▶ Railway checkpoint sandbox
       ▲                       │                        │
       └── one-job runner over Tailscale ──────────────┘
```

Forgejo base checkpoints can preload public Docker images. After a successful default-branch job, the controller can scrub the runner state and save a checkpoint partitioned by repository and job name, preserving Docker layers/build cache for the next build.

## Build

```bash
go build ./cmd/actions-builds-on-railway-sandbox
```

## Run GitHub mode

```bash
actions-builds-on-railway-sandbox github \
  --url https://github.com/my-org/my-repo \
  --name railway-runners \
  --max-runners 5 \
  --token "$GITHUB_TOKEN"
```

Use the scale-set name or configured label in a workflow:

```yaml
jobs:
  build:
    runs-on: railway-runners
```

GitHub runner images use Railway's content-addressed template builds. The image includes the Actions runner, Docker, and a normal build toolchain.

## Run Forgejo mode

The Forgejo controller needs Railway, Forgejo admin API, and Tailscale OAuth credentials. Those API credentials remain in the controller. A reusable runner registration token and the sandbox's ephemeral Tailscale key travel through exec stdin, not its environment. Bootstrap finishes registration and replaces its credential-bearing shell before running a job. The Railway exec bridge must support exec-control v2 stdin; there is no environment fallback.

```bash
export RAILWAY_API_TOKEN=...
export RAILWAY_ENVIRONMENT_ID=...
export FORGEJO_URL=https://forgejo.example.com
export FORGEJO_API_TOKEN=...
export TAILSCALE_TAILNET=example.ts.net
export TAILSCALE_OAUTH_CLIENT_ID=...
export TAILSCALE_OAUTH_CLIENT_SECRET=...

actions-builds-on-railway-sandbox forgejo \
  --runner-labels railway-ubuntu:host \
  --job-label-filter railway-ubuntu \
  --max-runners 10
```

The Tailscale OAuth client must be able to create ephemeral, pre-authorized devices with `TAILSCALE_TAG`. Restrict that tag so runner sandboxes can reach only the required Forgejo endpoint.

For privileged jobs, use a separate [restricted pool](docs/restricted-pools.md). `FORGEJO_RESTRICTED_POLICY` checks an exact repository/full-ref/event/workflow/unique-display-name tuple before provisioning, then binds runner 13.2.1 to that attempt's handle. This requires Forgejo 17.0.1 and base-only snapshots. Labels alone are not authorization.

See [docs/configuration.md](docs/configuration.md) and [.env.example](.env.example) for every setting.

## Custom runner images

Both modes support:

- `EXTRA_PACKAGES`: comma/whitespace-separated apt packages.
- `EXTRA_SETUP`: trusted shell setup baked into the runner image.

Forgejo additionally supports:

- `PRELOAD_DOCKER_IMAGES`: public images pulled into the base checkpoint.
- `DOCKER_WARMUP_SETUP`: trusted shell setup run with dockerd available.
- periodic base checkpoint refresh and per-job hydrated checkpoints.

Changing a recipe causes a new template/checkpoint to be prepared. Pin `GITHUB_RUNNER_VERSION` or `FORGEJO_RUNNER_VERSION` for reproducible production images.

## Dashboard

Both modes expose a dashboard on `PORT` (default `8080`) with active runner counts, seven-day usage, recent provider/job/sandbox records, and GitHub template build details. SQLite is stored at `$RAILWAY_VOLUME_MOUNT_PATH/dashboard.db`, falling back to `./data/dashboard.db` locally.

The dashboard has no authentication. Keep it private or put it behind an authenticated proxy.

## Railway deployment

The Docker image defaults to the `github` subcommand. `.railway/railway.ts` defines a GitHub controller service and dashboard volume:

```bash
bun install
railway config plan
railway config apply
```

A Forgejo controller can use the same image outside Railway, for example from the homenet k3s deployment, with the container argument overridden to `forgejo`.

Every push to `main` and every `v*` tag runs lint, tests, and a static build before publishing the image to [`ghcr.io/lawrencegripper/actions-builds-on-railway-sandbox`](https://github.com/lawrencegripper/actions-builds-on-railway-sandbox/pkgs/container/actions-builds-on-railway-sandbox). Main publishes `latest` and `sha-*`; version tags also publish the tag.

## Smoke test

Test Railway create/exec/destroy without either Actions provider:

```bash
actions-builds-on-railway-sandbox smoke --command "uname -a"
actions-builds-on-railway-sandbox smoke --checkpoint
```

## Tests

```bash
make ci
```

Tests cover the Railway GraphQL and WebSocket contracts, GitHub scaler state machine, Forgejo API/Tailscale clients, checkpoint creation and scrubbing, duplicate-job suppression, dashboard persistence, and configuration.

## Security

- Every job executes as root with dockerd; the Railway sandbox is the isolation boundary.
- Controller API credentials never enter the sandbox. Forgejo bootstrap credentials travel via stdin; only transient join/registration processes receive them in arguments, before workflow execution.
- Job output is not copied into controller logs.
- Hydrated Forgejo checkpoints remove runner state, workspaces, Docker registry auth, Tailscale state, and temporary files before capture.
- Docker layers produced by a workflow can still contain secrets. Enable hydrated checkpoints only for trusted workflows.
- `ENABLE_PRIVATE_NETWORKING` widens sandbox access to Railway private services. Leave it disabled unless required.
- `EXTRA_SETUP`, `DOCKER_WARMUP_SETUP`, and package lists are trusted operator inputs.

## Forgejo API assumptions

Forgejo mode currently uses:

- `GET /api/v1/admin/actions/runners/jobs`
- `GET /api/v1/admin/actions/runners/registration-token`
- `GET /api/v1/repositories/{id}`
- `GET /api/v1/repos/{owner}/{repo}/actions/tasks`

Run one Forgejo controller replica. Duplicate suppression is process-local because Forgejo does not currently provide this controller with a durable queue lease.
