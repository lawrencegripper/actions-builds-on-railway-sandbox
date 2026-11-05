package forgejo

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestPublishingWorkflowCredentials(t *testing.T) {
	data, err := os.ReadFile("../../.forgejo/workflows/image.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	for _, match := range regexp.MustCompile(`secrets\.([A-Za-z_][A-Za-z0-9_]*)`).FindAllStringSubmatch(workflow, -1) {
		name := strings.ToUpper(match[1])
		for _, prefix := range []string{"FORGEJO_", "GITEA_", "GITHUB_"} {
			if strings.HasPrefix(name, prefix) {
				t.Errorf("Forgejo rejects configured secret name %s", name)
			}
		}
	}
	for _, required := range []string{
		"REGISTRY_TOKEN: ${{ secrets.REGISTRY_TOKEN }}",
		`printf '%s' "$REGISTRY_TOKEN" | docker login`,
		"--password-stdin",
		"Missing REGISTRY_TOKEN secret",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("missing publishing credential safeguard: %s", required)
		}
	}
}
