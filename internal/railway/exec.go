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
	frameStderr byte = 0x03

	shellSubprotocol = "railway-shell"
)

// ExecOptions configures a single exec session.
type ExecOptions struct {
	Cwd string
	Env map[string]string
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
	// No stdin; EOF it so commands reading stdin can finish.
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
			if ef.Type == "exit" {
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return &ExecResult{ExitCode: ef.Data.ExitCode, Reason: ef.Data.Reason}, nil
			}
		}
	}
}
