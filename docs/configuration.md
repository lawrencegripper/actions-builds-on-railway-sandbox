# Configuration

Every setting is available as a CLI flag or an environment variable; an explicit flag wins over its env var. On Railway you set the environment variables, and the flags are for local runs. `RAILWAY_ENVIRONMENT_ID` is auto-injected when running as a Railway service, so leave it unset there.

## Credentials

Railway, provide exactly one token:

- `RAILWAY_PROJECT_TOKEN` (`--railway-project-token`): environment-scoped project token (Project Settings, Tokens). Preferred; sent via the `Project-Access-Token` header.
- `RAILWAY_API_TOKEN` (`--railway-token`): account/workspace token (Bearer auth).

GitHub, provide a PAT or a GitHub App (App preferred):

- `GITHUB_TOKEN` (`--token`): personal access token.
- `GITHUB_APP_CLIENT_ID` / `GITHUB_APP_INSTALLATION_ID` / `GITHUB_APP_PRIVATE_KEY` (`--app-client-id` / `--app-installation-id` / `--app-private-key`).

See the [scale set auth docs](https://docs.github.com/en/actions/tutorials/use-actions-runner-controller/authenticate-to-the-api) for the required permissions.

## Reference

| Env | Flag | Default | Notes |
| --- | --- | --- | --- |
| `GITHUB_URL` | `--url` | _(required)_ | repo/org/enterprise URL to register the scale set against |
| `SCALE_SET_NAME` | `--name` | _(required)_ | scale set name, also the default `runs-on` label |
| `GITHUB_TOKEN` | `--token` | \* | GitHub PAT |
| `GITHUB_APP_CLIENT_ID` / `_INSTALLATION_ID` / `_PRIVATE_KEY` | `--app-*` | \* | GitHub App (preferred over a PAT) |
| `RAILWAY_PROJECT_TOKEN` | `--railway-project-token` | \*\* | environment-scoped project token (preferred) |
| `RAILWAY_API_TOKEN` | `--railway-token` | \*\* | account/workspace token |
| `RAILWAY_ENVIRONMENT_ID` | `--railway-environment-id` | auto-injected | target environment (set only for local runs) |
| `RAILWAY_GRAPHQL_ENDPOINT` | `--railway-endpoint` | prod | Railway API endpoint |
| `RUNNER_LABELS` | `--labels` | = `SCALE_SET_NAME` | comma/space-separated targeting labels |
| `RUNNER_GROUP` | `--runner-group` | `default` | custom groups are org/enterprise-level only |
| `MIN_RUNNERS` | `--min-runners` | `0` | pre-provisioned (warm) runners |
| `MAX_RUNNERS` | `--max-runners` | `5` | concurrent runner cap |
| `IDLE_TIMEOUT_MINUTES` | `--idle-timeout-minutes` | `30` | sandbox idle timeout |
| `RUNNER_VERSION` | `--runner-version` | latest | actions runner release; pin for production |
| `EXTRA_PACKAGES` | `--extra-packages` | - | extra apt packages baked into the image |
| `EXTRA_SETUP` | `--extra-setup` | - | extra shell run as the final image build step |
| `ENABLE_PRIVATE_NETWORKING` | `--enable-private-networking` | `false` | give sandboxes access to the [Railway private network](https://docs.railway.com/networking/private-networking) |
| `LOG_LEVEL` | `--log-level` | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | `--log-format` | `text` | `text` or `json` (use `json` on Railway) |
| `PORT` | - | `8080` | dashboard HTTP port (Railway sets this automatically) |

The dashboard records job history to SQLite at `$RAILWAY_VOLUME_MOUNT_PATH/dashboard.db` when a Railway volume is attached (`RAILWAY_VOLUME_MOUNT_PATH` is injected by the volume mount), falling back to `./data/dashboard.db` locally. Attach a volume so history survives deploys.

\* Provide either a PAT or the three GitHub App variables.

\*\* Provide exactly one Railway token.
