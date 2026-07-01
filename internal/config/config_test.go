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
