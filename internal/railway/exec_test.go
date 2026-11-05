package railway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// execTestServer routes GraphQL POSTs (for generateShellToken) and WebSocket
// upgrades (the /ws/exec bridge) on one server, then drives the exec protocol.
func execTestServer(t *testing.T, drive func(t *testing.T, c *websocket.Conn)) (graphqlURL, wsURL string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept: %v", err)
				return
			}
			// The JWT is offered as the last Sec-WebSocket-Protocol value.
			protos := r.Header.Get("Sec-WebSocket-Protocol")
			if !strings.Contains(protos, shellSubprotocol) {
				t.Errorf("missing %q subprotocol, got %q", shellSubprotocol, protos)
			}
			if !strings.Contains(protos, "test-jwt") {
				t.Errorf("jwt not forwarded as subprotocol, got %q", protos)
			}
			drive(t, c)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"generateShellToken": "test-jwt"},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func clientFor(t *testing.T, graphqlURL, wsURL string) *Client {
	t.Helper()
	c, err := NewClient(Config{
		Token:              "t",
		EnvironmentID:      "env",
		GraphQLEndpoint:    graphqlURL,
		TCPProxyWsEndpoint: wsURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestExecStreamsAndExits(t *testing.T) {
	graphqlURL, wsURL := execTestServer(t, func(t *testing.T, c *websocket.Conn) {
		ctx := context.Background()
		defer c.Close(websocket.StatusNormalClosure, "")

		// First frame must be init_exec carrying the command and env.
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Errorf("read init: %v", err)
			return
		}
		var init struct {
			Type string `json:"type"`
			Data struct {
				Command string            `json:"command"`
				Cwd     string            `json:"cwd"`
				Env     map[string]string `json:"env"`
			} `json:"data"`
		}
		_ = json.Unmarshal(data, &init)
		if init.Type != "init_exec" || init.Data.Command != "./run.sh" {
			t.Errorf("bad init frame: %s", data)
		}
		if init.Data.Cwd != "/actions-runner" || init.Data.Env["FOO"] != "bar" {
			t.Errorf("cwd/env not forwarded: %+v", init.Data)
		}

		// stdin_close follows.
		_, _, _ = c.Read(ctx)

		_ = c.Write(ctx, websocket.MessageBinary, append([]byte{frameStdout}, []byte("hello ")...))
		_ = c.Write(ctx, websocket.MessageBinary, append([]byte{frameStdout}, []byte("world")...))
		_ = c.Write(ctx, websocket.MessageBinary, append([]byte{frameStderr}, []byte("oops")...))
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"exit","data":{"exit_code":42,"reason":"done"}}`))
	})

	c := clientFor(t, graphqlURL, wsURL)
	var out, errOut strings.Builder
	res, err := c.Exec(context.Background(), "sbx", "./run.sh", ExecOptions{
		Cwd:      "/actions-runner",
		Env:      map[string]string{"FOO": "bar"},
		OnStdout: func(b []byte) { out.Write(b) },
		OnStderr: func(b []byte) { errOut.Write(b) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 42 || res.Reason != "done" {
		t.Errorf("got exit %d %q", res.ExitCode, res.Reason)
	}
	if out.String() != "hello world" {
		t.Errorf("stdout = %q", out.String())
	}
	if errOut.String() != "oops" {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestExecConnectionClosedWithoutExit(t *testing.T) {
	graphqlURL, wsURL := execTestServer(t, func(t *testing.T, c *websocket.Conn) {
		ctx := context.Background()
		_, _, _ = c.Read(ctx) // init
		_, _, _ = c.Read(ctx) // stdin_close
		_ = c.Write(ctx, websocket.MessageBinary, append([]byte{frameStdout}, []byte("partial")...))
		c.Close(websocket.StatusNormalClosure, "")
	})

	c := clientFor(t, graphqlURL, wsURL)
	res, err := c.Exec(context.Background(), "sbx", "x", ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != -1 {
		t.Errorf("expected -1 on close without exit, got %d", res.ExitCode)
	}
}

func TestExecContextCancel(t *testing.T) {
	graphqlURL, wsURL := execTestServer(t, func(t *testing.T, c *websocket.Conn) {
		_, _, _ = c.Read(context.Background())
		time.Sleep(2 * time.Second) // never sends exit
	})

	c := clientFor(t, graphqlURL, wsURL)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := c.Exec(ctx, "sbx", "sleep", ExecOptions{}); err == nil {
		t.Fatal("expected error on context cancel")
	}
}

func TestExecBootstrapInputUsesStdinNotInit(t *testing.T) {
	payload := []byte("bootstrap-secret\x00registration-secret\x00")
	graphqlURL, wsURL := execTestServer(t, func(t *testing.T, c *websocket.Conn) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		defer c.Close(websocket.StatusNormalClosure, "")
		_, data, err := c.Read(ctx)
		if err != nil || string(data) != `{"type":"exec_hello"}` {
			t.Errorf("stdin must negotiate before init: %s (%v)", data, err)
			return
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"exec_capabilities","data":{"version":2,"stdin_chunks":8,"stdin_chunk_bytes":16384}}`))
		_, data, err = c.Read(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(string(data), "secret") || !strings.Contains(string(data), `"init_exec"`) {
			t.Errorf("credentials in init or missing init: %s", data)
		}
		typ, data, err := c.Read(ctx)
		if err != nil || typ != websocket.MessageBinary || len(data) == 0 || data[0] != 0x02 || string(data[1:]) != string(payload) {
			t.Errorf("incorrect stdin frame: type=%v data=%q err=%v", typ, data, err)
			return
		}
		_, data, err = c.Read(ctx)
		if err != nil || string(data) != `{"type":"stdin_close"}` {
			t.Errorf("expected EOF: %s (%v)", data, err)
			return
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"exit","data":{"exit_code":0}}`))
	})
	options := ExecOptions{Stdin: payload}
	result, err := clientFor(t, graphqlURL, wsURL).Exec(context.Background(), "sbx", "exec /bootstrap", options)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("stdin exec result=%+v error=%v", result, err)
	}
}

func TestExecBootstrapInputRejectsUnsupportedBridge(t *testing.T) {
	for _, response := range []string{
		`{"type":"exec_capabilities","data":{"version":1,"stdin_chunks":8,"stdin_chunk_bytes":16384}}`,
		`{"type":"exec_capabilities","data":{"version":2,"stdin_chunks":0,"stdin_chunk_bytes":16384}}`,
		`{"type":"exec_capabilities","data":{"version":2,"stdin_chunks":8,"stdin_chunk_bytes":1}}`,
		`{"type":"error"}`,
		"close",
	} {
		t.Run(response, func(t *testing.T) {
			graphqlURL, wsURL := execTestServer(t, func(t *testing.T, c *websocket.Conn) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				defer c.Close(websocket.StatusNormalClosure, "")
				_, data, err := c.Read(ctx)
				if err != nil || string(data) != `{"type":"exec_hello"}` {
					t.Errorf("command sent before capabilities: %s", data)
					return
				}
				if response == "close" {
					return
				}
				_ = c.Write(ctx, websocket.MessageText, []byte(response))
				if _, data, err := c.Read(ctx); err == nil {
					t.Errorf("unsupported bridge received command/credentials: %s", data)
				}
			})
			options := ExecOptions{Stdin: []byte("secret")}
			if _, err := clientFor(t, graphqlURL, wsURL).Exec(context.Background(), "sbx", "exec /bootstrap", options); err == nil {
				t.Fatal("unsupported stdin bridge accepted")
			}
		})
	}
}

func TestExecRejectsOversizedBootstrapInputBeforeConnection(t *testing.T) {
	client := clientFor(t, "http://unused.invalid", "ws://unused.invalid")
	_, err := client.Exec(context.Background(), "sbx", "exec /bootstrap", ExecOptions{Stdin: make([]byte, maxExecStdinBytes+1)})
	if err == nil || !strings.Contains(err.Error(), "stdin exceeds") {
		t.Fatalf("oversized input error = %v", err)
	}
}

func TestExecBootstrapInputReportsRemoteStdinFailure(t *testing.T) {
	graphqlURL, wsURL := execTestServer(t, func(t *testing.T, c *websocket.Conn) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		defer c.Close(websocket.StatusNormalClosure, "")
		_, _, _ = c.Read(ctx)
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"exec_capabilities","data":{"version":2,"stdin_chunks":1,"stdin_chunk_bytes":16384}}`))
		for range 3 {
			if _, _, err := c.Read(ctx); err != nil {
				t.Error(err)
				return
			}
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"stdin_error","data":{"message":"secret from remote"}}`))
	})
	_, err := clientFor(t, graphqlURL, wsURL).Exec(context.Background(), "sbx", "exec /bootstrap", ExecOptions{Stdin: []byte("secret")})
	if err == nil || !strings.Contains(err.Error(), "stdin_error") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("stdin failure not safely reported: %v", err)
	}
}
