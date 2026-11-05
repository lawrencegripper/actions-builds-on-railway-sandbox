package railway

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/coder/websocket"
)

// /ws/exec wire protocol (see exec-ws-client.ts): stdout/stderr ride binary
// frames tagged by a leading byte; init/stdin-close/signal/exit are JSON text.
const (
	frameStdout byte = 0x01
	frameStdin  byte = 0x02
	frameStderr byte = 0x03

	maxExecStdinBytes = 16 * 1024
	shellSubprotocol  = "railway-shell"
)

// ExecOptions configures a single exec session.
type ExecOptions struct {
	Cwd string
	Env map[string]string
	// Stdin is a single bounded input frame; non-empty input requires exec-control v2.
	Stdin []byte
	// OnStdout / OnStderr receive output chunks as they arrive. Optional.
	OnStdout func([]byte)
	OnStderr func([]byte)
}

// ExecResult is the final outcome of an exec.
type ExecResult struct {
	ExitCode int
	Reason   string
}

type initExecData struct {
	Command string            `json:"command"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type exitFrame struct {
	Type string `json:"type"`
	Data struct {
		ExitCode int    `json:"exit_code"`
		Reason   string `json:"reason"`
	} `json:"data"`
}

// Exec runs command in the sandbox over the /ws/exec bridge and blocks until the
// command exits (or ctx is cancelled). The JWT travels as the last
// Sec-WebSocket-Protocol value, per the bridge's token-extraction contract.
func (c *Client) Exec(ctx context.Context, sandboxID, command string, opts ExecOptions) (*ExecResult, error) {
	if len(opts.Stdin) > maxExecStdinBytes {
		return nil, fmt.Errorf("railway: exec stdin exceeds %d bytes", maxExecStdinBytes)
	}
	jwt, err := c.generateShellToken(ctx, sandboxID)
	if err != nil {
		return nil, err
	}

	conn, _, err := websocket.Dial(ctx, c.tcpProxyWs, &websocket.DialOptions{
		Subprotocols: []string{shellSubprotocol, jwt},
	})
	if err != nil {
		return nil, fmt.Errorf("railway: exec dial failed: %w", err)
	}
	defer conn.CloseNow()
	// Runner logs can be large; lift the default 32KiB read cap.
	conn.SetReadLimit(16 << 20)

	if len(opts.Stdin) > 0 {
		if err := negotiateExecStdin(ctx, conn); err != nil {
			return nil, err
		}
	}

	initFrame := map[string]any{"type": "init_exec", "data": initExecData{
		Command: command,
		Cwd:     opts.Cwd,
		Env:     opts.Env,
	}}
	initBytes, err := json.Marshal(initFrame)
	if err != nil {
		return nil, err
	}
	if err := conn.Write(ctx, websocket.MessageText, initBytes); err != nil {
		return nil, fmt.Errorf("railway: exec init write failed: %w", err)
	}
	if len(opts.Stdin) > 0 {
		if err := conn.Write(ctx, websocket.MessageBinary, append([]byte{frameStdin}, opts.Stdin...)); err != nil {
			return nil, fmt.Errorf("railway: exec stdin write failed: %w", err)
		}
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"stdin_close"}`)); err != nil {
		return nil, fmt.Errorf("railway: exec stdin_close failed: %w", err)
	}

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			// A clean close without an exit frame: report what we know.
			status := websocket.CloseStatus(err)
			if status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway {
				return &ExecResult{ExitCode: -1, Reason: "connection closed before exit"}, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("railway: exec read failed: %w", err)
		}

		switch typ {
		case websocket.MessageBinary:
			if len(data) <= 1 {
				continue
			}
			switch data[0] {
			case frameStdout:
				if opts.OnStdout != nil {
					opts.OnStdout(data[1:])
				}
			case frameStderr:
				if opts.OnStderr != nil {
					opts.OnStderr(data[1:])
				}
			}
		case websocket.MessageText:
			var ef exitFrame
			if err := json.Unmarshal(data, &ef); err != nil {
				continue
			}
			if ef.Type == "stdin_error" || ef.Type == "error" {
				return nil, fmt.Errorf("railway: exec bridge reported %s", ef.Type)
			}
			if ef.Type == "exit" {
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return &ExecResult{ExitCode: ef.Data.ExitCode, Reason: ef.Data.Reason}, nil
			}
		}
	}
}

func negotiateExecStdin(ctx context.Context, conn *websocket.Conn) error {
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"exec_hello"}`)); err != nil {
		return fmt.Errorf("railway: exec stdin negotiation failed: %w", err)
	}
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("railway: exec stdin negotiation failed: %w", err)
	}
	var capabilities struct {
		Type string `json:"type"`
		Data struct {
			Version    int `json:"version"`
			Chunks     int `json:"stdin_chunks"`
			ChunkBytes int `json:"stdin_chunk_bytes"`
		} `json:"data"`
	}
	if typ != websocket.MessageText || json.Unmarshal(data, &capabilities) != nil || capabilities.Type != "exec_capabilities" || capabilities.Data.Version < 2 || capabilities.Data.Chunks < 1 || capabilities.Data.Chunks > 64 || capabilities.Data.ChunkBytes < maxExecStdinBytes {
		return fmt.Errorf("railway: exec bridge lacks supported stdin capabilities; no command sent")
	}
	return nil
}
