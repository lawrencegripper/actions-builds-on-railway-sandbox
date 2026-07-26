package railway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDeriveTCPProxyWsEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://backboard.railway.com/graphql/v2":         "wss://ssh.railway.com:2226/ws/exec",
		"https://backboard.eu-west.example.com/graphql/v2": "wss://ssh.eu-west.example.com:2226/ws/exec",
		"https://example.com/graphql":                      "wss://ssh.example.com:2226/ws/exec",
	}
	for in, want := range cases {
		got, err := deriveTCPProxyWsEndpoint(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}

// graphQLStub responds to named operations with canned data.
func graphQLStub(t *testing.T, handler func(op string, vars map[string]any) any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("missing Authorization header")
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		op := operationName(req.Query)
		data := handler(op, req.Variables)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
}

func operationName(query string) string {
	switch {
	case strings.Contains(query, "sandboxCheckpointCreate"):
		return "sandboxCheckpointCreate"
	case strings.Contains(query, "sandboxCheckpointDelete"):
		return "sandboxCheckpointDelete"
	case strings.Contains(query, "sandboxCheckpoints"):
		return "sandboxCheckpoints"
	case strings.Contains(query, "sandboxCreate"):
		return "sandboxCreate"
	case strings.Contains(query, "sandboxDestroy"):
		return "sandboxDestroy"
	case strings.Contains(query, "generateShellToken"):
		return "generateShellToken"
	case strings.Contains(query, "sandbox("):
		return "sandbox"
	}
	return ""
}

func newTestClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	c, err := NewClient(Config{Token: "t", EnvironmentID: "env", GraphQLEndpoint: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCreateSandboxImmediateRunning(t *testing.T) {
	srv := graphQLStub(t, func(op string, _ map[string]any) any {
		if op == "sandboxCreate" {
			return map[string]any{"sandboxCreate": map[string]any{"id": "sbx_1", "status": "RUNNING"}}
		}
		return nil
	})
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	sb, err := c.CreateSandbox(context.Background(), CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sb.ID != "sbx_1" || sb.Status != StatusRunning {
		t.Fatalf("got %+v", sb)
	}
}

func TestCreateSandboxFromCheckpointWithVariables(t *testing.T) {
	var input map[string]any
	srv := graphQLStub(t, func(op string, vars map[string]any) any {
		if op == "sandboxCreate" {
			input = vars["input"].(map[string]any)
			return map[string]any{"sandboxCreate": map[string]any{"id": "sbx_checkpoint", "status": "RUNNING"}}
		}
		return nil
	})
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.CreateSandbox(context.Background(), CreateOptions{
		CheckpointName: "runner-base",
		Variables:      map[string]string{"SHORT_LIVED": "value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	template, ok := input["template"].(map[string]any)
	if !ok || template["name"] != "runner-base" {
		t.Fatalf("checkpoint template input = %#v", input["template"])
	}
	variables, ok := input["variables"].(map[string]any)
	if !ok || variables["SHORT_LIVED"] != "value" {
		t.Fatalf("variables input = %#v", input["variables"])
	}
}

func TestCreateSandboxRejectsTemplateAndCheckpoint(t *testing.T) {
	c := newTestClient(t, "http://unused.invalid")
	_, err := c.CreateSandbox(context.Background(), CreateOptions{
		TemplateInstructions: []string{"echo template"},
		CheckpointName:       "checkpoint",
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually exclusive error, got %v", err)
	}
}

func TestCheckpoints(t *testing.T) {
	var deletedID string
	srv := graphQLStub(t, func(op string, vars map[string]any) any {
		switch op {
		case "sandboxCheckpointCreate":
			if vars["sandboxId"] != "sbx_1" || vars["name"] != "runner-base" {
				t.Errorf("create checkpoint vars = %#v", vars)
			}
			return map[string]any{"sandboxCheckpointCreate": map[string]any{
				"id": "cp_1", "key": "runner-base", "environmentId": "env", "createdAt": "2026-07-26T10:00:00Z",
			}}
		case "sandboxCheckpoints":
			return map[string]any{"sandboxCheckpoints": []map[string]any{
				{"id": "cp_1", "key": "runner-base", "environmentId": "env", "createdAt": "2026-07-26T10:00:00Z"},
			}}
		case "sandboxCheckpointDelete":
			deletedID, _ = vars["id"].(string)
			return map[string]any{"sandboxCheckpointDelete": true}
		}
		return nil
	})
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cp, err := c.CreateCheckpoint(context.Background(), "sbx_1", "runner-base")
	if err != nil || cp.ID != "cp_1" || cp.Key != "runner-base" {
		t.Fatalf("create checkpoint = %+v, %v", cp, err)
	}
	checkpoints, err := c.ListCheckpoints(context.Background())
	if err != nil || len(checkpoints) != 1 || checkpoints[0].ID != "cp_1" {
		t.Fatalf("list checkpoints = %+v, %v", checkpoints, err)
	}
	if err := c.DeleteCheckpoint(context.Background(), "cp_1"); err != nil {
		t.Fatal(err)
	}
	if deletedID != "cp_1" {
		t.Fatalf("deleted id = %q", deletedID)
	}
}

func TestCreateSandboxPollsToRunning(t *testing.T) {
	var polls atomic.Int32
	srv := graphQLStub(t, func(op string, _ map[string]any) any {
		switch op {
		case "sandboxCreate":
			return map[string]any{"sandboxCreate": map[string]any{"id": "sbx_2", "status": "CREATING"}}
		case "sandbox":
			n := polls.Add(1)
			status := "CREATING"
			if n >= 2 {
				status = "RUNNING"
			}
			return map[string]any{"sandbox": map[string]any{"id": "sbx_2", "status": status}}
		}
		return nil
	})
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	sb, err := c.CreateSandbox(context.Background(), CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sb.Status != StatusRunning {
		t.Fatalf("expected RUNNING, got %s", sb.Status)
	}
	if polls.Load() < 2 {
		t.Fatalf("expected to poll at least twice, got %d", polls.Load())
	}
}

func TestCreateSandboxTerminalFails(t *testing.T) {
	srv := graphQLStub(t, func(op string, _ map[string]any) any {
		if op == "sandboxCreate" {
			return map[string]any{"sandboxCreate": map[string]any{"id": "sbx_3", "status": "FAILED"}}
		}
		return nil
	})
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := c.CreateSandbox(context.Background(), CreateOptions{}); err == nil {
		t.Fatal("expected error on terminal status")
	}
}

func TestGraphQLErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"message": "Not authorized"}}})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.GetSandbox(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "Not authorized") {
		t.Fatalf("expected authz error, got %v", err)
	}
}

func TestProjectTokenAuthHeader(t *testing.T) {
	var sawProjectHeader, sawBearer bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawProjectHeader = r.Header.Get("Project-Access-Token") == "ptok"
		sawBearer = r.Header.Get("Authorization") != ""
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"sandbox": nil}})
	}))
	defer srv.Close()

	c, err := NewClient(Config{Token: "ptok", AuthType: AuthProjectToken, EnvironmentID: "env", GraphQLEndpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetSandbox(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if !sawProjectHeader {
		t.Error("expected Project-Access-Token header")
	}
	if sawBearer {
		t.Error("must not send Authorization header for project token")
	}
}

func TestDestroySandbox(t *testing.T) {
	var called atomic.Bool
	srv := graphQLStub(t, func(op string, _ map[string]any) any {
		if op == "sandboxDestroy" {
			called.Store(true)
			return map[string]any{"sandboxDestroy": map[string]any{"id": "x", "status": "DESTROYING"}}
		}
		return nil
	})
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if err := c.DestroySandbox(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if !called.Load() {
		t.Fatal("destroy not called")
	}
}
