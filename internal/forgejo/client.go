package forgejo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Job is a queued Forgejo Actions runner job returned by the admin API.
type Job struct {
	ID     int64  `json:"id"`
	RepoID int64  `json:"repo_id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

func (j Job) Key() string {
	if j.ID != 0 {
		return fmt.Sprintf("%d", j.ID)
	}
	return fmt.Sprintf("%d:%s", j.RepoID, j.Name)
}

type Repository struct {
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Owner         struct {
		Login    string `json:"login"`
		Username string `json:"username"`
		Name     string `json:"name"`
	} `json:"owner"`
}

type ActionTask struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	HeadBranch   string `json:"head_branch"`
	Status       string `json:"status"`
	WorkflowID   string `json:"workflow_id"`
	UpdatedAt    string `json:"updated_at"`
	RunStartedAt string `json:"run_started_at"`
}

type API struct {
	cfg    Config
	http   *http.Client
	logger *slog.Logger
}

func NewAPI(cfg Config, httpClient *http.Client, logger *slog.Logger) *API {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &API{cfg: cfg, http: httpClient, logger: logger}
}

func (a *API) getJSON(ctx context.Context, rawURL string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "token "+a.cfg.APIToken)
	req.Header.Set("User-Agent", "actions-builds-on-railway-sandbox")
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("Forgejo API request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("Forgejo API %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode Forgejo API response: %w", err)
	}
	return nil
}

func (a *API) WaitingJobs(ctx context.Context) ([]Job, error) {
	parsed, err := url.Parse(a.cfg.JobsURL)
	if err != nil {
		return nil, err
	}
	query := parsed.Query()
	if query.Get("labels") == "" {
		query.Set("labels", a.cfg.JobLabelFilter)
	}
	parsed.RawQuery = query.Encode()
	var jobs []Job
	if err := a.getJSON(ctx, parsed.String(), &jobs); err != nil {
		return nil, err
	}
	out := jobs[:0]
	for _, job := range jobs {
		if job.Status == "waiting" || job.Status == "queued" {
			out = append(out, job)
		}
	}
	return out, nil
}

func (a *API) RegistrationToken(ctx context.Context) (string, error) {
	var response struct {
		Token string `json:"token"`
	}
	if err := a.getJSON(ctx, a.cfg.RegistrationTokenURL, &response); err != nil {
		return "", err
	}
	if response.Token == "" {
		return "", fmt.Errorf("Forgejo registration-token response did not contain token")
	}
	return response.Token, nil
}

func (a *API) Repository(ctx context.Context, id int64) (Repository, error) {
	var repository Repository
	rawURL := fmt.Sprintf("%s/api/v1/repositories/%d", a.cfg.URL, id)
	if err := a.getJSON(ctx, rawURL, &repository); err != nil {
		return Repository{}, err
	}
	return repository, nil
}

func repositoryOwnerName(repository Repository) (owner, name string, ok bool) {
	if before, after, found := strings.Cut(repository.FullName, "/"); found && before != "" && after != "" {
		return before, after, true
	}
	owner = repository.Owner.Login
	if owner == "" {
		owner = repository.Owner.Username
	}
	if owner == "" {
		owner = repository.Owner.Name
	}
	return owner, repository.Name, owner != "" && repository.Name != ""
}

func (a *API) ActionTasks(ctx context.Context, repository Repository) ([]ActionTask, error) {
	owner, name, ok := repositoryOwnerName(repository)
	if !ok {
		return nil, fmt.Errorf("Forgejo repository response did not contain owner/name")
	}
	rawURL := fmt.Sprintf("%s/api/v1/repos/%s/%s/actions/tasks?limit=20", a.cfg.URL, url.PathEscape(owner), url.PathEscape(name))
	var response struct {
		WorkflowRuns []ActionTask `json:"workflow_runs"`
	}
	if err := a.getJSON(ctx, rawURL, &response); err != nil {
		return nil, err
	}
	return response.WorkflowRuns, nil
}

type JobCompletion struct {
	Outcome string
	Hydrate bool
}

// Completion finds the task most likely claimed by a completed one-job runner.
// Forgejo does not expose a direct claim correlation here, so this preserves the
// existing scaler's best-effort match by recent timestamp and job name.
func (a *API) Completion(ctx context.Context, job Job, runnerStarted time.Time) (JobCompletion, error) {
	if job.RepoID == 0 {
		return JobCompletion{Outcome: "unknown"}, nil
	}
	repository, err := a.Repository(ctx, job.RepoID)
	if err != nil {
		return JobCompletion{}, err
	}
	tasks, err := a.ActionTasks(ctx, repository)
	if err != nil {
		return JobCompletion{}, err
	}
	cutoff := runnerStarted.Add(-5 * time.Minute)
	sort.SliceStable(tasks, func(i, j int) bool { return taskTime(tasks[i]).After(taskTime(tasks[j])) })
	var selected *ActionTask
	for i := range tasks {
		if taskTime(tasks[i]).Before(cutoff) || (job.Name != "" && tasks[i].Name != job.Name) {
			continue
		}
		selected = &tasks[i]
		break
	}
	if selected == nil && job.Name != "" {
		for i := range tasks {
			if tasks[i].Name == job.Name {
				selected = &tasks[i]
				break
			}
		}
	}
	if selected == nil && len(tasks) > 0 {
		selected = &tasks[0]
	}
	if selected == nil {
		return JobCompletion{Outcome: "unknown"}, nil
	}
	branch := strings.TrimPrefix(strings.TrimPrefix(selected.HeadBranch, "refs/heads/"), "heads/")
	outcome := selected.Status
	switch outcome {
	case "success":
		outcome = "succeeded"
	case "failure":
		outcome = "failed"
	}
	if outcome == "" {
		outcome = "unknown"
	}
	hydrate := a.cfg.SnapshotDefaultBranchJobs && outcome == "succeeded" && repository.DefaultBranch != "" && branch == repository.DefaultBranch
	a.logger.Info("matched Forgejo job completion",
		"jobId", job.ID,
		"repoId", job.RepoID,
		"taskId", selected.ID,
		"taskStatus", selected.Status,
		"branch", branch,
		"defaultBranch", repository.DefaultBranch,
		"outcome", outcome,
		"hydrate", hydrate,
	)
	return JobCompletion{Outcome: outcome, Hydrate: hydrate}, nil
}

func taskTime(task ActionTask) time.Time {
	raw := task.UpdatedAt
	if raw == "" {
		raw = task.RunStartedAt
	}
	parsed, _ := time.Parse(time.RFC3339, raw)
	return parsed
}
