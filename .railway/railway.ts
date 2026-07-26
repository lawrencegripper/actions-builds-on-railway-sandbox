// Railway Infrastructure-as-Code for actions-builds-on-railway-sandbox.
//
// Defines the whole project: the runner/orchestrator service (built from this
// repo's Dockerfile), a volume for the dashboard's SQLite database, and the
// service's input variables. DSL: github.com/railwayapp/railway-ts-sdk.
//
// Usage:
//   bun install                 # resolves the `railway` DSL into node_modules
//   railway config plan         # preview the changes
//   railway config apply        # create/update the project
//   railway domain              # expose the dashboard (generates a public URL)
//
// Fill the sealed variables (RAILWAY_PROJECT_TOKEN, a GitHub credential) when
// prompted on apply; RAILWAY_ENVIRONMENT_ID and RAILWAY_VOLUME_MOUNT_PATH are
// injected by Railway at runtime.

import { defineRailway, service, volume, github } from "railway";

const REPO = "lawrencegripper/actions-builds-on-railway-sandbox";

export default defineRailway((_ctx, project) => {
  // Persists the dashboard's SQLite database across deploys. Mounted at /data,
  // which the service reads via RAILWAY_VOLUME_MOUNT_PATH.
  const data = volume("runner-data", { sizeMB: 1024 });

  const runner = service("actions-builds-on-railway-sandbox", {
    source: github(REPO),
    volumeMounts: { "runner-data": { mountPath: "/data" } },
    deploy: { restartPolicyType: "ON_FAILURE", restartPolicyMaxRetries: 10 },
    env: {
      // Railway API access for the sandbox lifecycle (sent as Project-Access-Token).
      RAILWAY_PROJECT_TOKEN: {
        description: "Railway project token, environment-scoped (Project Settings, Tokens)",
        isSealed: true,
        isOptional: false,
      },
      // Where to register the scale set, e.g. https://github.com/you/repo.
      GITHUB_URL: { description: "GitHub repo/org/enterprise URL", isOptional: false },
      GITHUB_SCALE_SET_NAME: {
        description: "GitHub scale set name, also the default runs-on label",
        defaultValue: "railway-runners",
        isOptional: false,
      },
      // Provide a PAT, or the three GITHUB_APP_* variables instead.
      GITHUB_TOKEN: { description: "GitHub PAT (or use the GITHUB_APP_* variables)", isSealed: true, isOptional: true },
      GITHUB_APP_CLIENT_ID: { description: "GitHub App client id", isOptional: true },
      GITHUB_APP_INSTALLATION_ID: { description: "GitHub App installation id", isOptional: true },
      GITHUB_APP_PRIVATE_KEY: { description: "GitHub App private key (PEM)", isSealed: true, isOptional: true },
      // Optional tuning; see docs/configuration.md for the full set.
      GITHUB_MAX_RUNNERS: { description: "Max concurrent GitHub runners", defaultValue: "5", isOptional: true },
      GITHUB_RUNNER_VERSION: { description: "Actions runner version; defaults to the latest release", isOptional: true },
      LOG_FORMAT: "json",
    },
  });

  return project("actions-builds-on-railway-sandbox", { resources: [data, runner] });
});
