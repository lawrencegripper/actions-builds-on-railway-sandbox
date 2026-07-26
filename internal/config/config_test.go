package config

import (
	"context"
	"strings"
	"testing"
)

func TestResolveRunnerVersionExplicit(t *testing.T) {
	c := &Config{RunnerVersion: "v2.331.0"}
	if err := c.ResolveRunnerVersion(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.RunnerVersion != "2.331.0" {
		t.Errorf("expected v-prefix stripped, got %q", c.RunnerVersion)
	}
}

func TestResolveRunnerVersionRejectsGarbage(t *testing.T) {
	c := &Config{RunnerVersion: "1.2"} // not X.Y.Z
	if err := c.ResolveRunnerVersion(context.Background()); err == nil {
		t.Fatal("expected rejection of malformed version")
	}
}

func TestValidateRejectsUnsafeWorkdir(t *testing.T) {
	c := &Config{
		RegistrationURL:      "https://github.com/o/r",
		ScaleSetName:         "x",
		Token:                "t",
		MaxRunners:           1,
		RailwayProjectToken:  "p",
		RailwayEnvironmentID: "e",
		RunnerWorkdir:        "/app; rm -rf /",
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected unsafe --runner-workdir to be rejected")
	}
}

func allUnset(string) bool { return false }

func TestApplyEnvReadsWhenFlagUnset(t *testing.T) {
	t.Setenv("GITHUB_URL", "https://github.com/o/r")
	t.Setenv("GITHUB_MAX_RUNNERS", "9")
	t.Setenv("GITHUB_RUNNER_LABELS", "a, b ,c")
	t.Setenv("EXTRA_PACKAGES", "python3 ffmpeg")
	t.Setenv("ENABLE_PRIVATE_NETWORKING", "yes")
	t.Setenv("GITHUB_APP_INSTALLATION_ID", "42")
	t.Setenv("RAILWAY_PROJECT_TOKEN", "ptok")

	c := &Config{}
	if err := c.ApplyEnv(allUnset); err != nil {
		t.Fatal(err)
	}
	if c.RegistrationURL != "https://github.com/o/r" || c.MaxRunners != 9 {
		t.Errorf("string/int env not applied: %+v", c)
	}
	if len(c.Labels) != 3 || c.Labels[0] != "a" || c.Labels[2] != "c" {
		t.Errorf("labels not split/trimmed: %v", c.Labels)
	}
	if len(c.ExtraPackages) != 2 {
		t.Errorf("extra packages not split: %v", c.ExtraPackages)
	}
	if !c.EnablePrivateNetworking {
		t.Error("truthy env not applied")
	}
	if c.GitHubApp.InstallationID != 42 || c.RailwayProjectToken != "ptok" {
		t.Errorf("int64/railway env not applied: %+v", c.GitHubApp)
	}
}

func TestApplyEnvFlagWins(t *testing.T) {
	t.Setenv("GITHUB_URL", "https://env")
	c := &Config{RegistrationURL: "https://flag"}
	if err := c.ApplyEnv(func(f string) bool { return f == "url" }); err != nil {
		t.Fatal(err)
	}
	if c.RegistrationURL != "https://flag" {
		t.Errorf("explicit flag should win over env, got %q", c.RegistrationURL)
	}
}

func TestApplyEnvBadInt(t *testing.T) {
	t.Setenv("GITHUB_MAX_RUNNERS", "notanint")
	if err := (&Config{}).ApplyEnv(allUnset); err == nil {
		t.Fatal("expected error on non-integer GITHUB_MAX_RUNNERS")
	}
}

func TestRunnerTemplateInstructions(t *testing.T) {
	c := &Config{RunnerVersion: "2.328.0", RunnerWorkdir: "/actions-runner"}
	steps := c.RunnerTemplateInstructions()
	all := strings.Join(steps, "\n")

	if !strings.Contains(all, "actions-runner-linux-x64-2.328.0.tar.gz") {
		t.Error("download step missing the x64/version tarball")
	}
	for _, pkg := range []string{"git", "jq", "make", "docker.io"} {
		if !strings.Contains(all, pkg) {
			t.Errorf("base toolchain missing %q", pkg)
		}
	}
	if !strings.Contains(all, "cat > "+RunnerStartCommand) {
		t.Error("start script is not baked into the image")
	}
	if !strings.Contains(all, "/actions-runner/bin/installdependencies.sh") {
		t.Error("installdependencies.sh step missing")
	}
}

func TestRunnerTemplateExtras(t *testing.T) {
	c := &Config{
		RunnerVersion: "2.328.0", RunnerWorkdir: "/actions-runner",
		ExtraPackages: []string{"python3", "ffmpeg"},
		ExtraSetup:    "pip install uv",
	}
	steps := c.RunnerTemplateInstructions()
	all := strings.Join(steps, "\n")
	for _, pkg := range []string{"python3", "ffmpeg"} {
		if !strings.Contains(all, pkg) {
			t.Errorf("extra package %q not baked in", pkg)
		}
	}
	if steps[len(steps)-1] != "pip install uv" {
		t.Errorf("ExtraSetup should be the last build step, got %q", steps[len(steps)-1])
	}
}

func TestStartScriptShape(t *testing.T) {
	c := &Config{RunnerWorkdir: "/actions-runner"}
	step := c.writeStartScriptStep()
	for _, want := range []string{
		"export PATH=",
		"ACTIONS_RUNNER_INPUT_JITCONFIG",
		"dockerd",
		"cd /actions-runner",
		"exec ./run.sh",
		"chmod +x " + RunnerStartCommand,
	} {
		if !strings.Contains(step, want) {
			t.Errorf("start script missing %q", want)
		}
	}
}
