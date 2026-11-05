package forgejo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

type RestrictedPolicy struct {
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
	Event      string `json:"event"`
	Workflow   string `json:"workflow"`
	JobName    string `json:"job_name"`
}

var repositoryPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$`)
var workflowPattern = regexp.MustCompile(`^[a-zA-Z0-9_-][a-zA-Z0-9_.-]*\.ya?ml$`)
var jobNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-][a-zA-Z0-9_. -]*$`)

func parseRestrictedPolicy(raw string) (*RestrictedPolicy, error) {
	var policy *RestrictedPolicy
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return nil, fmt.Errorf("FORGEJO_RESTRICTED_POLICY: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("FORGEJO_RESTRICTED_POLICY must contain one JSON object")
	}
	if policy == nil {
		return nil, fmt.Errorf("FORGEJO_RESTRICTED_POLICY must not be null")
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	return policy, nil
}

func (p RestrictedPolicy) validate() error {
	if !repositoryPattern.MatchString(p.Repository) || strings.Contains(p.Repository, "..") ||
		!strings.HasPrefix(p.Ref, "refs/heads/") || len(p.Ref) == len("refs/heads/") || strings.ContainsAny(p.Ref, " *?[]\\~^:\t\r\n") || strings.Contains(p.Ref, "..") ||
		!workflowPattern.MatchString(p.Workflow) || !jobNamePattern.MatchString(p.JobName) || strings.TrimSpace(p.JobName) != p.JobName {
		return fmt.Errorf("restricted policy requires exact repository, full branch ref, workflow filename and plain job display name (no matrix suffixes)")
	}
	switch p.Event {
	case "push", "workflow_dispatch", "schedule":
	default:
		return fmt.Errorf("restricted policy event must be push, workflow_dispatch or schedule")
	}
	return nil
}

func (c Config) validateRestricted() error {
	if c.RestrictedPolicy == nil {
		return nil
	}
	if err := c.RestrictedPolicy.validate(); err != nil {
		return err
	}
	if c.RunnerVersion != "13.2.1" {
		return fmt.Errorf("restricted pools require FORGEJO_RUNNER_VERSION=13.2.1")
	}
	if c.SnapshotDefaultBranchJobs {
		return fmt.Errorf("restricted pools require SNAPSHOT_AFTER_DEFAULT_BRANCH_BUILD=false")
	}
	if !strings.HasPrefix(c.SnapshotNamePrefix, "restricted-") || c.SnapshotNamePrefix == "restricted-" || sanitizeCheckpointPart(c.SnapshotNamePrefix) != c.SnapshotNamePrefix {
		return fmt.Errorf("restricted pools require a separate RUNNER_SNAPSHOT_NAME_PREFIX starting with restricted-")
	}
	if c.JobsURL != c.URL+"/api/v1/admin/actions/runners/jobs" || c.RegistrationTokenURL != c.URL+"/api/v1/admin/actions/runners/registration-token" {
		return fmt.Errorf("restricted pools do not support Forgejo endpoint overrides")
	}
	return nil
}

type actionRun struct {
	ID                int64      `json:"id"`
	Index             int64      `json:"index_in_repo"`
	Repository        Repository `json:"repository"`
	WorkflowID        string     `json:"workflow_id"`
	Event             string     `json:"event"`
	TriggerEvent      string     `json:"trigger_event"`
	NeedApproval      *bool      `json:"need_approval"`
	IsForkPullRequest *bool      `json:"is_fork_pull_request"`
	Status            string     `json:"status"`
}

func explicitlyFalse(value *bool) bool { return value != nil && !*value }

func (p RestrictedPolicy) matchesRun(run actionRun, job Job) bool {
	return run.ID == job.RunID && run.Index > 0 && run.Repository.ID == job.RepoID && run.Repository.FullName == p.Repository &&
		explicitlyFalse(run.Repository.Fork) && explicitlyFalse(run.NeedApproval) && explicitlyFalse(run.IsForkPullRequest) &&
		run.WorkflowID == p.Workflow && run.Event == p.Event && run.TriggerEvent == p.Event && (run.Status == "waiting" || run.Status == "running")
}

func (p RestrictedPolicy) repositoryURL(base string) string {
	owner, name, _ := strings.Cut(p.Repository, "/")
	return base + "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

func sameAttempt(a, b Job) bool {
	return a.ID == b.ID && a.RepoID == b.RepoID && a.RunID == b.RunID && a.Attempt == b.Attempt && a.Handle == b.Handle && a.Name == b.Name
}

func (a *API) AuthorizeJob(ctx context.Context, job Job) error {
	if a.cfg.RestrictedPolicy == nil {
		return fmt.Errorf("restricted policy is missing")
	}
	if err := a.cfg.validateRestricted(); err != nil {
		return err
	}
	p := *a.cfg.RestrictedPolicy
	if job.ID <= 0 || job.RepoID <= 0 || job.RunID <= 0 || job.Attempt <= 0 || job.Name != p.JobName || job.Status != "waiting" {
		return fmt.Errorf("queued job lacks matching restricted metadata")
	}
	if _, err := uuid.Parse(job.Handle); err != nil {
		return fmt.Errorf("queued job lacks a valid attempt handle")
	}
	var version struct {
		Version string `json:"version"`
	}
	if err := a.getJSON(ctx, a.cfg.URL+"/api/v1/version", &version); err != nil {
		return err
	}
	if version.Version != "17.0.1" && version.Version != "17.0.1+gitea-1.22.0" {
		return fmt.Errorf("restricted pools require Forgejo 17.0.1, got %q", version.Version)
	}
	base := p.repositoryURL(a.cfg.URL)
	var run actionRun
	if err := a.getJSON(ctx, fmt.Sprintf("%s/actions/runs/%d", base, job.RunID), &run); err != nil {
		return err
	}
	if !p.matchesRun(run, job) {
		return fmt.Errorf("run does not match restricted policy or has unknown/untrusted metadata")
	}
	query := url.Values{"ref": {p.Ref}, "event": {p.Event}, "workflow_id": {p.Workflow}, "run_number": {strconv.FormatInt(run.Index, 10)}, "limit": {"2"}}
	var filtered struct {
		Runs       []actionRun `json:"workflow_runs"`
		TotalCount *int64      `json:"total_count"`
	}
	// PrettyRef erases the branch/tag distinction; the server filter checks the full ref.
	if err := a.getJSON(ctx, base+"/actions/runs?"+query.Encode(), &filtered); err != nil {
		return err
	}
	if filtered.TotalCount == nil || *filtered.TotalCount != 1 || len(filtered.Runs) != 1 || !p.matchesRun(filtered.Runs[0], job) || filtered.Runs[0].Index != run.Index {
		return fmt.Errorf("run does not match the exact full-ref filter")
	}
	var jobs []Job
	if err := a.getJSON(ctx, fmt.Sprintf("%s/actions/runs/%d/jobs", base, job.RunID), &jobs); err != nil {
		return err
	}
	matches := 0
	ids := make(map[int64]bool, len(jobs))
	for _, candidate := range jobs {
		if candidate.ID <= 0 || candidate.RunID != job.RunID || candidate.RepoID != job.RepoID || candidate.Name == "" || ids[candidate.ID] {
			return fmt.Errorf("run job list has ambiguous or incomplete metadata")
		}
		ids[candidate.ID] = true
		if candidate.Name == p.JobName {
			matches++
			if !sameAttempt(candidate, job) || candidate.Status != "waiting" {
				return fmt.Errorf("run job does not match queued attempt")
			}
		}
	}
	if matches != 1 {
		return fmt.Errorf("restricted job display name must be unique within the run")
	}
	current, err := a.actionJob(ctx, job.ID)
	if err != nil {
		return err
	}
	if !sameAttempt(current, job) || current.Status != "waiting" {
		return fmt.Errorf("queued job attempt changed")
	}
	return nil
}

func (a *API) actionJob(ctx context.Context, id int64) (Job, error) {
	var job Job
	err := a.getJSON(ctx, fmt.Sprintf("%s/actions/jobs/%d", a.cfg.RestrictedPolicy.repositoryURL(a.cfg.URL), id), &job)
	return job, err
}

func (a *API) restrictedCompletion(ctx context.Context, job Job) (JobCompletion, error) {
	current, err := a.actionJob(ctx, job.ID)
	if err != nil {
		return JobCompletion{}, err
	}
	if !sameAttempt(current, job) {
		return JobCompletion{}, fmt.Errorf("completed job attempt changed")
	}
	outcome := "unknown"
	switch current.Status {
	case "success":
		outcome = "succeeded"
	case "failure":
		outcome = "failed"
	case "cancelled", "skipped":
		outcome = current.Status
	}
	return JobCompletion{Outcome: outcome}, nil
}
