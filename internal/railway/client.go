// Package railway is a minimal Go client for the Railway sandbox API, ported
// from the official TypeScript SDK (sandbox create/get/destroy, template
// builds, shell-token minting, and the /ws/exec bridge).
package railway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultGraphQLEndpoint = "https://backboard.railway.com/graphql/v2"

	readinessTimeout = 5 * time.Minute
	pollInitialDelay = 500 * time.Millisecond
	pollMaxDelay     = 5 * time.Second
)

// AuthType selects how the API token is presented. Account/workspace tokens use
// Bearer; environment-scoped project tokens use the project-access-token header.
type AuthType string

const (
	AuthBearer       AuthType = "bearer"
	AuthProjectToken AuthType = "project-token"
)

type NetworkIsolation string

const (
	NetworkIsolated NetworkIsolation = "ISOLATED"
	NetworkPrivate  NetworkIsolation = "PRIVATE"
)

type SandboxStatus string

const (
	StatusCreating   SandboxStatus = "CREATING"
	StatusRunning    SandboxStatus = "RUNNING"
	StatusDestroying SandboxStatus = "DESTROYING"
	StatusDestroyed  SandboxStatus = "DESTROYED"
	StatusFailed     SandboxStatus = "FAILED"
)

func (s SandboxStatus) terminal() bool {
	return s == StatusFailed || s == StatusDestroyed || s == StatusDestroying
}

// Sandbox mirrors RailwaySandboxFields.
type Sandbox struct {
	ID                 string           `json:"id"`
	Status             SandboxStatus    `json:"status"`
	NetworkIsolation   NetworkIsolation `json:"networkIsolation"`
	EnvironmentID      string           `json:"environmentId"`
	Region             string           `json:"region"`
	IdleTimeoutMinutes int              `json:"idleTimeoutMinutes"`
	CreatedAt          string           `json:"createdAt"`
}

// Config holds connection details. EnvironmentID and Token are required; the
// endpoint and tcp-proxy ws endpoint are derived when left blank.
type Config struct {
	Token              string
	AuthType           AuthType // defaults to bearer
	EnvironmentID      string
	GraphQLEndpoint    string
	TCPProxyWsEndpoint string // override; otherwise derived from GraphQLEndpoint
	HTTPClient         *http.Client
	Logger             *slog.Logger
}

type Client struct {
	cfg        Config
	httpClient *http.Client
	tcpProxyWs string
	log        *slog.Logger
}

func NewClient(cfg Config) (*Client, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("railway: token is required")
	}
	if cfg.EnvironmentID == "" {
		return nil, fmt.Errorf("railway: environment id is required")
	}
	if cfg.GraphQLEndpoint == "" {
		cfg.GraphQLEndpoint = DefaultGraphQLEndpoint
	}
	if cfg.AuthType == "" {
		cfg.AuthType = AuthBearer
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	tcpProxyWs := cfg.TCPProxyWsEndpoint
	if tcpProxyWs == "" {
		derived, err := deriveTCPProxyWsEndpoint(cfg.GraphQLEndpoint)
		if err != nil {
			return nil, err
		}
		tcpProxyWs = derived
	}
	return &Client{cfg: cfg, httpClient: httpClient, tcpProxyWs: tcpProxyWs, log: log}, nil
}

// deriveTCPProxyWsEndpoint mirrors deriveTcpProxyWsEndpoint in the TS SDK:
// backboard.<host> -> wss://ssh.<host>:2226/ws/exec.
func deriveTCPProxyWsEndpoint(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("railway: parse endpoint: %w", err)
	}
	host := u.Hostname()
	if after, ok := strings.CutPrefix(host, "backboard."); ok {
		host = "ssh." + after
	} else {
		host = "ssh." + host
	}
	return fmt.Sprintf("wss://%s:2226/ws/exec", host), nil
}

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type graphQLError struct {
	Message string `json:"message"`
}

func (c *Client) do(ctx context.Context, query string, variables map[string]any, out any) error {
	operation := graphqlOperation(query)
	body, err := json.Marshal(graphQLRequest{Query: query, Variables: variables})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.GraphQLEndpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.AuthType == AuthProjectToken {
		req.Header.Set("Project-Access-Token", c.cfg.Token)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("railway: request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphQLError  `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		c.log.Error("railway graphql response decode failed", slog.String("operation", operation), slog.Int("status", resp.StatusCode), slog.String("response", truncate(raw)), slog.String("error", err.Error()))
		return fmt.Errorf("railway: decode response (status %d): %w: %s", resp.StatusCode, err, truncate(raw))
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, len(envelope.Errors))
		for i, e := range envelope.Errors {
			msgs[i] = e.Message
		}
		c.log.Error("railway graphql error", slog.String("operation", operation), slog.Int("status", resp.StatusCode), slog.String("response", truncate(raw)))
		return fmt.Errorf("railway: graphql error (status %d): %s", resp.StatusCode, strings.Join(msgs, "; "))
	}
	if resp.StatusCode >= 400 {
		c.log.Error("railway graphql http error", slog.String("operation", operation), slog.Int("status", resp.StatusCode), slog.String("response", truncate(raw)))
		return fmt.Errorf("railway: http %d: %s", resp.StatusCode, truncate(raw))
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("railway: decode data: %w", err)
		}
	}
	return nil
}

func graphqlOperation(query string) string {
	query = strings.TrimSpace(query)
	for _, prefix := range []string{"query", "mutation", "subscription"} {
		if strings.HasPrefix(query, prefix) {
			return prefix
		}
	}
	return "unknown"
}

func truncate(b []byte) string {
	const max = 512
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}

// CreateOptions are the knobs for CreateSandbox.
type CreateOptions struct {
	IdleTimeoutMinutes int
	NetworkIsolation   NetworkIsolation
	// Variables are runtime environment variables attached to the sandbox. Do not
	// log their values; they may contain short-lived runner credentials.
	Variables map[string]string
	// TemplateInstructions builds (or reuses a cached build of) a template from
	// these shell instructions before booting. The backend content-addresses
	// the build, so passing identical instructions reuses the cache.
	TemplateInstructions []string
	// CheckpointName boots from a named checkpoint. It is mutually exclusive with
	// TemplateInstructions.
	CheckpointName string
}

const sandboxFields = `id status networkIsolation environmentId region idleTimeoutMinutes createdAt`

// CreateSandbox creates a sandbox and waits until it is RUNNING.
func (c *Client) CreateSandbox(ctx context.Context, opts CreateOptions) (*Sandbox, error) {
	if len(opts.TemplateInstructions) > 0 && opts.CheckpointName != "" {
		return nil, fmt.Errorf("railway: template instructions and checkpoint name are mutually exclusive")
	}
	input := map[string]any{"environmentId": c.cfg.EnvironmentID}
	if opts.IdleTimeoutMinutes > 0 {
		input["idleTimeoutMinutes"] = opts.IdleTimeoutMinutes
	}
	if opts.NetworkIsolation != "" {
		input["networkIsolation"] = opts.NetworkIsolation
	}
	if len(opts.Variables) > 0 {
		input["variables"] = opts.Variables
	}
	if len(opts.TemplateInstructions) > 0 {
		input["template"] = map[string]any{"instructions": opts.TemplateInstructions}
	}
	if opts.CheckpointName != "" {
		input["template"] = map[string]any{"name": opts.CheckpointName}
	}

	query := `mutation($input: SandboxCreateInput!){ sandboxCreate(input: $input){ ` + sandboxFields + ` } }`
	var data struct {
		SandboxCreate Sandbox `json:"sandboxCreate"`
	}
	if err := c.do(ctx, query, map[string]any{"input": input}, &data); err != nil {
		return nil, err
	}
	c.log.Info("sandbox created", slog.String("id", data.SandboxCreate.ID), slog.String("status", string(data.SandboxCreate.Status)))
	sb, err := c.waitForRunning(ctx, data.SandboxCreate)
	if err != nil {
		// The sandbox exists server-side even though it never became RUNNING;
		// best-effort destroy so a readiness failure doesn't leak it. Use a
		// detached context so a cancelled ctx still lets the cleanup through.
		if derr := c.DestroySandbox(context.WithoutCancel(ctx), data.SandboxCreate.ID); derr != nil {
			c.log.Error("failed to destroy sandbox after readiness failure", slog.String("id", data.SandboxCreate.ID), slog.String("error", derr.Error()))
		}
		return nil, err
	}
	return sb, nil
}

func (c *Client) waitForRunning(ctx context.Context, sb Sandbox) (*Sandbox, error) {
	if sb.Status == StatusRunning {
		return &sb, nil
	}
	if sb.Status.terminal() {
		return nil, fmt.Errorf("railway: sandbox %s reached terminal state %s before RUNNING", sb.ID, sb.Status)
	}
	deadline := time.Now().Add(readinessTimeout)
	delay := pollInitialDelay
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		cur, err := c.GetSandbox(ctx, sb.ID)
		if err != nil {
			return nil, err
		}
		if cur == nil {
			return nil, fmt.Errorf("railway: sandbox %s not found while waiting for RUNNING", sb.ID)
		}
		if cur.Status == StatusRunning {
			return cur, nil
		}
		if cur.Status.terminal() {
			return nil, fmt.Errorf("railway: sandbox %s reached terminal state %s", cur.ID, cur.Status)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("railway: timed out waiting for sandbox %s to be RUNNING (last status %s)", sb.ID, cur.Status)
		}
		delay = min(delay*2, pollMaxDelay)
	}
}

func (c *Client) GetSandbox(ctx context.Context, id string) (*Sandbox, error) {
	query := `query($environmentId: String!, $id: String!){ sandbox(environmentId: $environmentId, id: $id){ ` + sandboxFields + ` } }`
	var data struct {
		Sandbox *Sandbox `json:"sandbox"`
	}
	if err := c.do(ctx, query, map[string]any{"environmentId": c.cfg.EnvironmentID, "id": id}, &data); err != nil {
		return nil, err
	}
	return data.Sandbox, nil
}

func (c *Client) DestroySandbox(ctx context.Context, id string) error {
	query := `mutation($id: String!, $environmentId: String!){ sandboxDestroy(id: $id, environmentId: $environmentId){ id status } }`
	return c.do(ctx, query, map[string]any{"id": id, "environmentId": c.cfg.EnvironmentID}, nil)
}

// Checkpoint is a named, bootable disk snapshot stored in a Railway environment.
type Checkpoint struct {
	ID            string `json:"id"`
	Key           string `json:"key"`
	EnvironmentID string `json:"environmentId"`
	CreatedAt     string `json:"createdAt"`
}

const checkpointFields = `id key environmentId createdAt`

// CreateCheckpoint captures a running sandbox. The backend call is synchronous:
// the checkpoint is bootable when it returns.
func (c *Client) CreateCheckpoint(ctx context.Context, sandboxID, name string) (*Checkpoint, error) {
	query := `mutation($environmentId: String!, $name: String!, $sandboxId: String!){ sandboxCheckpointCreate(environmentId: $environmentId, name: $name, sandboxId: $sandboxId){ ` + checkpointFields + ` } }`
	var data struct {
		Checkpoint Checkpoint `json:"sandboxCheckpointCreate"`
	}
	vars := map[string]any{"environmentId": c.cfg.EnvironmentID, "name": name, "sandboxId": sandboxID}
	c.log.Info("creating sandbox checkpoint", slog.String("sandbox", sandboxID), slog.String("checkpoint", name))
	if err := c.do(ctx, query, vars, &data); err != nil {
		c.log.Error("sandbox checkpoint creation failed", slog.String("sandbox", sandboxID), slog.String("checkpoint", name), slog.String("error", err.Error()))
		return nil, err
	}
	c.log.Info("sandbox checkpoint created", slog.String("sandbox", sandboxID), slog.String("checkpoint", name), slog.String("id", data.Checkpoint.ID))
	return &data.Checkpoint, nil
}

// ListCheckpoints lists all named checkpoints in the environment.
func (c *Client) ListCheckpoints(ctx context.Context) ([]Checkpoint, error) {
	query := `query($environmentId: String!){ sandboxCheckpoints(environmentId: $environmentId){ ` + checkpointFields + ` } }`
	var data struct {
		Checkpoints []Checkpoint `json:"sandboxCheckpoints"`
	}
	if err := c.do(ctx, query, map[string]any{"environmentId": c.cfg.EnvironmentID}, &data); err != nil {
		return nil, err
	}
	return data.Checkpoints, nil
}

// DeleteCheckpoint deletes a checkpoint and its underlying disk snapshot by id.
func (c *Client) DeleteCheckpoint(ctx context.Context, id string) error {
	query := `mutation($environmentId: String!, $id: ID!){ sandboxCheckpointDelete(environmentId: $environmentId, id: $id) }`
	return c.do(ctx, query, map[string]any{"environmentId": c.cfg.EnvironmentID, "id": id}, nil)
}

type TemplateBuildStatus string

const (
	BuildPending  TemplateBuildStatus = "PENDING"
	BuildBuilding TemplateBuildStatus = "BUILDING"
	BuildReady    TemplateBuildStatus = "READY"
	BuildFailed   TemplateBuildStatus = "FAILED"
)

type templateBuild struct {
	ID     string              `json:"id"`
	Status TemplateBuildStatus `json:"status"`
}

// TemplateBuildResult reports the outcome of a template build for the dashboard.
// Railway's API exposes only the content-addressed build id and a status enum,
// so there are no per-line build logs to capture here.
type TemplateBuildResult struct {
	ID     string
	Status TemplateBuildStatus
	Err    error
}

// BuildTemplateUntilReady pre-warms a template build so later CreateSandbox
// calls fork from the cached build instead of building inline. It always returns
// a result (with the build id and last-seen status) so callers can record the
// attempt even when it fails; err mirrors result.Err.
func (c *Client) BuildTemplateUntilReady(ctx context.Context, instructions []string) (TemplateBuildResult, error) {
	query := `mutation($environmentId: String!, $input: SandboxTemplateInput!){ sandboxTemplateBuild(environmentId: $environmentId, input: $input){ id status } }`
	input := map[string]any{"instructions": instructions}
	var data struct {
		SandboxTemplateBuild templateBuild `json:"sandboxTemplateBuild"`
	}
	if err := c.do(ctx, query, map[string]any{"environmentId": c.cfg.EnvironmentID, "input": input}, &data); err != nil {
		return TemplateBuildResult{Status: BuildFailed, Err: err}, err
	}
	build := data.SandboxTemplateBuild
	c.log.Info("template build started", slog.String("id", build.ID), slog.String("status", string(build.Status)))

	fail := func(err error) (TemplateBuildResult, error) {
		return TemplateBuildResult{ID: build.ID, Status: build.Status, Err: err}, err
	}

	statusQuery := `query($environmentId: String!, $id: ID!){ sandboxTemplateBuild(environmentId: $environmentId, id: $id){ id status } }`
	deadline := time.Now().Add(readinessTimeout)
	delay := pollInitialDelay
	for {
		switch build.Status {
		case BuildReady:
			c.log.Info("template build ready", slog.String("id", build.ID))
			return TemplateBuildResult{ID: build.ID, Status: build.Status}, nil
		case BuildFailed:
			return fail(fmt.Errorf("railway: template build %s failed", build.ID))
		}
		if time.Now().After(deadline) {
			return fail(fmt.Errorf("railway: template build %s timed out (last status %s)", build.ID, build.Status))
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(delay):
		}
		var sd struct {
			SandboxTemplateBuild templateBuild `json:"sandboxTemplateBuild"`
		}
		if err := c.do(ctx, statusQuery, map[string]any{"environmentId": c.cfg.EnvironmentID, "id": build.ID}, &sd); err != nil {
			return fail(err)
		}
		build = sd.SandboxTemplateBuild
		delay = min(delay*2, pollMaxDelay)
	}
}

// generateShellToken mints a short-lived JWT authorizing /ws/exec for a sandbox.
func (c *Client) generateShellToken(ctx context.Context, sandboxID string) (string, error) {
	query := `mutation($input: ShellTokenInput!){ generateShellToken(input: $input) }`
	input := map[string]any{
		"environmentId": c.cfg.EnvironmentID,
		"instanceId":    sandboxID,
		"kind":          "sandbox",
		"scope":         "shell",
	}
	var data struct {
		GenerateShellToken string `json:"generateShellToken"`
	}
	if err := c.do(ctx, query, map[string]any{"input": input}, &data); err != nil {
		return "", err
	}
	if data.GenerateShellToken == "" {
		return "", fmt.Errorf("railway: empty shell token for sandbox %s", sandboxID)
	}
	return data.GenerateShellToken, nil
}
