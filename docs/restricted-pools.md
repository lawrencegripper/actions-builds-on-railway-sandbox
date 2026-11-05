# Restricted Forgejo pools

Run a separate controller for privileged jobs. Runner labels route work; they do not authorize it. `FORGEJO_RESTRICTED_POLICY` enables one exact allow rule. Unset it to retain the ordinary build pool. Empty, null, malformed and unknown-field policies fail startup.

## Configuration

```sh
FORGEJO_RESTRICTED_POLICY='{"repository":"lawrencegripper/homenet","ref":"refs/heads/master","event":"push","workflow":"build-k3s.yaml","job_name":"deploy-k3s"}'
FORGEJO_RUNNER_VERSION=13.2.1
FORGEJO_RUNNER_LABELS=railway-homenet-deploy:host
FORGEJO_JOB_LABEL_FILTER=railway-homenet-deploy
TAILSCALE_TAG=tag:homenet-cd
RUNNER_SNAPSHOT_NAME_PREFIX=restricted-homenet-deploy
SNAPSHOT_AFTER_DEFAULT_BRANCH_BUILD=false
ENABLE_PRIVATE_NETWORKING=false
```

Use the existing Railway, Forgejo and Tailscale credential settings. Give this controller its own Tailscale OAuth identity, authorized only for the deployment tag. Grant that tag only the required Forgejo and deployment destinations. The tag requires a matching tailnet policy; the controller does not create grants.

| Policy field | Contract |
|---|---|
| `repository` | Exact, case-sensitive `owner/repository`, verified against numeric repository identity |
| `ref` | Exact full branch ref starting `refs/heads/`; tags are not supported |
| `event` | Exact `push`, `workflow_dispatch` or `schedule`; both event and trigger event must match |
| `workflow` | Exact workflow filename, such as `build-k3s.yaml`, not a path |
| `job_name` | Exact **unique display name** in that run, not the YAML job key |

The environment-only policy has no wildcard or expression syntax. Job names accept ASCII letters, digits, spaces, `_`, `-`, and `.`; the first character must be a letter, digit, `_`, or `-`. Workflow and repository names use similarly restricted literal characters. No leading/trailing job-name whitespace is accepted.

Use a distinct label, Tailscale tag and snapshot prefix for each trust boundary. Prefixes must begin `restricted-`, contain only letters, digits, `.`, `_`, and `-`, and never be reused by an ordinary pool. Hydration must be explicitly disabled, including the CLI flag if set. Forgejo jobs/registration endpoint overrides are rejected in restricted mode.

## Versions and authorization

Restricted pools support **Forgejo 17.0.1** (`17.0.1+gitea-1.22.0` is also accepted) and require **runner 13.2.1**. Other server/runner versions fail closed until reviewed. Ordinary pools retain their existing version behavior.

```text
queued job ID + run ID + attempt + handle
  -> verified repository run
  -> exact full-ref/event/workflow/run-number filter
  -> complete run-job list, unique display name and same numeric job ID
  -> current job detail, same attempt and handle
  -> repository registration token + ephemeral Tailscale key + base sandbox
  -> repeat authorization
  -> forgejo-runner one-job --handle <verified handle>
  -> destroy sandbox
```

Authorization uses authenticated v17 APIs:

- `GET /api/v1/version`
- `GET /api/v1/admin/actions/runners/jobs`
- `GET /api/v1/repos/{owner}/{repo}/actions/runs/{run_id}`
- `GET /api/v1/repos/{owner}/{repo}/actions/runs?ref=...&event=...&workflow_id=...&run_number=...`
- `GET /api/v1/repos/{owner}/{repo}/actions/runs/{run_id}/jobs`
- `GET /api/v1/repos/{owner}/{repo}/actions/jobs/{job_id}`
- `GET /api/v1/repos/{owner}/{repo}/actions/runners/registration-token`

The run list has no `run_id` filter. Its unique repository run number narrows the full-ref query, and the returned run ID must match the queued job. `prettyref` is never used: a tag named `master` must not authorize `refs/heads/master`.

Missing identity, attempt, handle, approval/fork metadata, duplicate names, mismatches, unsupported versions and API failures reject the job. Fork repositories, fork pull requests and runs needing approval are rejected. Unrelated same-label jobs cannot consume the bound runner: runner 13.2.1 uses `FetchSingleTask` for a supplied handle without unbound fallback. A rerun changes the server handle, so a race after the final check cannot bind the runner to the new attempt.

Registration is repository-scoped and the runner is ephemeral. The registration-token endpoint returns a **reusable repository credential**, not an intrinsically short-lived or one-job token. Controller API credentials and Tailscale OAuth credentials never enter the sandbox.

Bootstrap credentials travel in a bounded stdin frame, never in Railway's exec command or environment. This requires exec-control v2 with a nonzero stdin credit window and at least 16 KiB chunks; an unsupported bridge fails before receiving a command. There is no environment fallback. This transport requirement applies to ordinary Forgejo pools too.

```text
Railway command wrappers: no bootstrap credentials in argv/environment
  -> bootstrap reads two NUL-terminated values, closes stdin, keeps variables unexported
  -> dockerd/tailscaled start without either credential in their environment
  -> Tailscale join and repository registration finish
  -> bootstrap unsets variables and exec-replaces itself
  -> clean supervisor runs the job, then logs out and stops tailscaled
```

The transient join/registration commands receive their credentials as arguments and finish before workflow code starts. Re-exec replaces the bootstrap shell's memory and initial environment block; `unset` alone would not remove the latter from `/proc`. The job's own runner credential and Tailscale session remain usable until teardown. Sandbox destruction remains the final cleanup boundary.

Local `/proc` tests inspect the actual bootstrap script under a deliberately retaining command wrapper, including runner/supervisor ancestry and both daemons. WebSocket tests verify that init frames contain no bootstrap credentials. The opt-in live test checks Railway stdin support and process ancestry with the actual bootstrap script and harmless command stubs. Provider-side buffers, memory and telemetry are not inspected; this is not a claim of universal memory erasure.

## Workflow and snapshot trust

The allowed repository, ref and workflow code are the authorization boundary. Protect the branch and workflow changes, including scripts and actions they invoke. Do not execute untrusted pull-request code, artifacts or caches from the privileged job.

For the example, `build-k3s.yaml` must expose exactly one job with display name `deploy-k3s` and `runs-on: railway-homenet-deploy`. Set `name: deploy-k3s` explicitly or ensure the fallback display name is identical. The controller does not verify the YAML job key.

Matrices are unsupported. Duplicate matching names and standard parenthesized matrix names are rejected. **The v17 API does not expose matrix metadata:** a matrix entry with a unique custom display name is indistinguishable from a non-matrix job. The trusted workflow must keep the selected job non-matrix; this implementation cannot prove that property from the API. A requirement to reject every custom-named matrix needs additional server metadata or immutable workflow inspection.

Restricted pools build fresh base checkpoints, ignore all hydrated-cache selections, reject hydrated images and cannot capture post-job checkpoints. Base packages, setup scripts and preloaded images remain trusted operator inputs. Do not share the restricted prefix or base setup with an untrusted workload.

## Verification

Local tests cover authorization mismatches, duplicate names, missing metadata, tag/branch confusion, changed attempts, same-label queue selection, exact handle propagation, shell invocation with no fallback, cache restrictions and unchanged ordinary behavior. These use HTTP and runner-command fixtures. Deployment, server upgrade completion, tailnet grants and an end-to-end privileged job require separate verification.

With `RAILWAY_API_TOKEN` and `RAILWAY_ENVIRONMENT_ID` supplied locally, run the optional provider probe through the existing test target:

```sh
FORGEJO_LIVE_BOOTSTRAP_TEST=true GOFLAGS='-run=TestLiveBootstrap -v -count=1 -timeout=8m' make test
```

It creates one isolated sandbox per invocation and destroys it afterward. Dummy join and registration commands exercise both runner modes without enrolling a Tailscale node or registering a Forgejo runner. No live credentials are passed into the sandbox.

Source contracts reviewed: [Forgejo 17.0.1](https://codeberg.org/forgejo/forgejo/src/commit/363f3dc30beef902b187af5a5244ff7af44901e6/modules/structs/action.go), [run endpoints](https://codeberg.org/forgejo/forgejo/src/commit/363f3dc30beef902b187af5a5244ff7af44901e6/routers/api/v1/repo/action.go), and [runner 13.2.1 bound polling](https://code.forgejo.org/forgejo/runner/src/tag/v13.2.1/internal/app/poll/single.go).
