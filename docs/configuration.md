# Configuration

The binary has three subcommands:

```text
actions-builds-on-railway-sandbox github
actions-builds-on-railway-sandbox forgejo
actions-builds-on-railway-sandbox smoke
```

Flags override environment variables. Run a subcommand with `--help` for flag details.

## Shared Railway settings

| Environment | Flag | Required | Description |
|---|---|---:|---|
| `RAILWAY_PROJECT_TOKEN` | `--railway-project-token` | one token | Environment-scoped project token, preferred for Railway deployments |
| `RAILWAY_API_TOKEN` | `--railway-token` | one token | Account/workspace token |
| `RAILWAY_ENVIRONMENT_ID` | `--railway-environment-id` | yes | Environment containing the sandboxes/checkpoints |
| `RAILWAY_GRAPHQL_ENDPOINT` | `--railway-endpoint` | no | GraphQL endpoint override |
| `ENABLE_PRIVATE_NETWORKING` | `--enable-private-networking` | no | Attach job sandboxes to Railway private networking |
| `LOG_LEVEL` | `--log-level` | no | `debug`, `info`, `warn`, or `error` |
| `LOG_FORMAT` | `--log-format` | no | `text` or `json` |
| `PORT` | — | no | Dashboard port, default `8080` |
| `RAILWAY_VOLUME_MOUNT_PATH` | — | no | Dashboard SQLite volume directory |

## GitHub mode

| Environment | Flag | Required | Default |
|---|---|---:|---|
| `GITHUB_URL` | `--url` | yes | — |
| `GITHUB_SCALE_SET_NAME` | `--name` | yes | — |
| `GITHUB_RUNNER_LABELS` | `--labels` | no | scale-set name |
| `GITHUB_RUNNER_GROUP` | `--runner-group` | no | `Default` |
| `GITHUB_TOKEN` | `--token` | credential* | — |
| `GITHUB_APP_CLIENT_ID` | `--app-client-id` | credential* | — |
| `GITHUB_APP_INSTALLATION_ID` | `--app-installation-id` | credential* | — |
| `GITHUB_APP_PRIVATE_KEY` | `--app-private-key` | credential* | — |
| `GITHUB_MIN_RUNNERS` | `--min-runners` | no | `0` |
| `GITHUB_MAX_RUNNERS` | `--max-runners` | no | `5` |
| `GITHUB_RUNNER_VERSION` | `--runner-version` | no | latest |
| `IDLE_TIMEOUT_MINUTES` | `--idle-timeout-minutes` | no | `30` |
| `EXTRA_PACKAGES` | `--extra-packages` | no | — |
| `EXTRA_SETUP` | `--extra-setup` | no | — |

* Provide a PAT or all three GitHub App values. GitHub App authentication is preferred.

## Forgejo mode

### Forgejo and runner

| Environment | Flag | Required | Default |
|---|---|---:|---|
| `FORGEJO_URL` | `--forgejo-url` | yes | — |
| `FORGEJO_API_TOKEN` | `--forgejo-token` | yes | — |
| `FORGEJO_JOBS_URL` | `--forgejo-jobs-url` | no | `{FORGEJO_URL}/api/v1/admin/actions/runners/jobs` |
| `FORGEJO_REGISTRATION_TOKEN_URL` | `--forgejo-registration-token-url` | no | `{FORGEJO_URL}/api/v1/admin/actions/runners/registration-token` |
| `FORGEJO_RUNNER_LABELS` | `--runner-labels` | no | `railway-ubuntu:host` |
| `FORGEJO_JOB_LABEL_FILTER` | `--job-label-filter` | no | label names from runner labels |
| `FORGEJO_RUNNER_VERSION` | `--runner-version` | no | latest |
| `POLL_INTERVAL_SECONDS` | `--poll-interval` | no | `5s` |
| `MAX_CONCURRENT_SANDBOXES` | `--max-runners` | no | `2` |
| `MAX_LAUNCH_PER_POLL` | `--max-launch-per-poll` | no | `2` |
| `SANDBOX_IDLE_TIMEOUT_MINUTES` | `--sandbox-idle-timeout-minutes` | no | `10` |
| `RUNNER_TIMEOUT_SECONDS` | `--runner-timeout` | no | `2h` |

Duration flags use Go duration syntax; the corresponding environment variables are integer seconds/hours as named.

### Restricted Forgejo pools

`FORGEJO_RESTRICTED_POLICY` is an optional environment-only JSON object with exactly `repository`, `ref`, `event`, `workflow`, and `job_name`. It authorizes one exact tuple; `job_name` is a unique display name, not a YAML job key. Unset preserves ordinary behavior; empty or invalid configuration fails startup.

Restricted mode pins Forgejo 17.0.1 and runner 13.2.1, requires `SNAPSHOT_AFTER_DEFAULT_BRANCH_BUILD=false` and a separate `restricted-` snapshot prefix, and rejects endpoint overrides. See [restricted pool configuration and trust contract](restricted-pools.md) for the complete example, matrix metadata limitation, API checks and Tailscale separation.

### Forgejo checkpoints

| Environment | Flag | Default |
|---|---|---|
| `EXTRA_PACKAGES` | `--extra-packages` | — |
| `EXTRA_SETUP` | `--extra-setup` | — |
| `PRELOAD_DOCKER_IMAGES` | `--preload-docker-images` | — |
| `DOCKER_WARMUP_SETUP` | `--docker-warmup-setup` | — |
| `SNAPSHOT_REFRESH_HOURS` | `--snapshot-refresh` | `24h` |
| `RUNNER_SNAPSHOT_NAME_PREFIX` | `--snapshot-name-prefix` | `forgejo-runner` |
| `SNAPSHOT_BUILD_TIMEOUT_SECONDS` | `--snapshot-build-timeout` | `30m` |
| `RETAINED_RUNNER_SNAPSHOTS` | `--retained-snapshots` | `3` per checkpoint family |
| `SNAPSHOT_AFTER_DEFAULT_BRANCH_BUILD` | `--snapshot-default-branch-jobs` | `true` |

Hydrated checkpoints are partitioned by repository id and job name. Rebuilding the base checkpoint clears the controller's in-memory hydrated checkpoint selection, so each job family rehydrates from the fresh base.

### Tailscale

| Environment | Flag | Required | Default |
|---|---|---:|---|
| `TAILSCALE_TAILNET` | `--tailscale-tailnet` | yes | — |
| `TAILSCALE_API_TOKEN` | `--tailscale-api-token` | credential* | — |
| `TAILSCALE_OAUTH_CLIENT_ID` | `--tailscale-oauth-client-id` | credential* | — |
| `TAILSCALE_OAUTH_CLIENT_SECRET` | `--tailscale-oauth-secret` | credential* | — |
| `TAILSCALE_TAG` | `--tailscale-tag` | no | `tag:railway-forgejo-runner` |
| `TAILSCALE_KEY_EXPIRY_SECONDS` | `--tailscale-key-expiry` | no | `10m` |

* Provide an API token or both OAuth values. OAuth is preferred. The controller caches OAuth access tokens but creates a new non-reusable ephemeral device key per runner.
