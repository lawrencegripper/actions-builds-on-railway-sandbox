package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/config"
	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/railway"
)

// smokeCmd exercises Railway without an Actions provider. With --checkpoint it
// additionally captures the first sandbox, restores another from it, verifies a
// marker on disk, and deletes the checkpoint.
func smokeCmd(cfg *config.Config) *cobra.Command {
	var command string
	var keep bool
	var checkpoint bool
	cmd := &cobra.Command{
		Use:   "smoke",
		Short: "Create a sandbox, run a command, and destroy it (Railway transport check)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cfg.ApplyRailwayEnv(cmd.Flags().Changed); err != nil {
				return err
			}
			if err := cfg.ValidateRailway(); err != nil {
				return err
			}
			logger := cfg.Logger()
			ctx, cancel := context.WithTimeout(cmd.Context(), 12*time.Minute)
			defer cancel()
			client, err := cfg.RailwayClient(logger.WithGroup("railway"))
			if err != nil {
				return err
			}

			create := func(options railway.CreateOptions) (*railway.Sandbox, error) {
				sandbox, err := client.CreateSandbox(ctx, options)
				if err != nil {
					return nil, fmt.Errorf("create: %w", err)
				}
				logger.Info("sandbox running", "id", sandbox.ID, "region", sandbox.Region)
				return sandbox, nil
			}
			destroy := func(sandbox *railway.Sandbox) {
				if sandbox == nil || keep {
					return
				}
				if err := client.DestroySandbox(context.WithoutCancel(ctx), sandbox.ID); err != nil {
					logger.Error("destroy failed", "sandbox", sandbox.ID, "error", err)
				} else {
					logger.Info("sandbox destroyed", "id", sandbox.ID)
				}
			}
			exec := func(sandbox *railway.Sandbox, shell string) error {
				result, err := client.Exec(ctx, sandbox.ID, shell, railway.ExecOptions{
					OnStdout: func(chunk []byte) { _, _ = os.Stdout.Write(chunk) },
					OnStderr: func(chunk []byte) { _, _ = os.Stderr.Write(chunk) },
				})
				if err != nil {
					return fmt.Errorf("exec: %w", err)
				}
				if result.ExitCode != 0 {
					return fmt.Errorf("exec exited %d: %s", result.ExitCode, result.Reason)
				}
				return nil
			}

			first, err := create(railway.CreateOptions{IdleTimeoutMinutes: 10, NetworkIsolation: cfg.NetworkIsolation()})
			if err != nil {
				return err
			}
			firstDestroyed := false
			defer func() {
				if !firstDestroyed {
					destroy(first)
				}
			}()
			runCommand := command
			if checkpoint {
				runCommand += "\nprintf checkpoint-smoke > /tmp/actions-builds-checkpoint-smoke"
			}
			if err := exec(first, runCommand); err != nil {
				return err
			}
			if !checkpoint {
				if keep {
					logger.Info("keeping sandbox", "id", first.ID)
				}
				return nil
			}

			checkpointName := fmt.Sprintf("actions-builds-smoke-%d", time.Now().UnixNano())
			captured, err := client.CreateCheckpoint(ctx, first.ID, checkpointName)
			if err != nil {
				return fmt.Errorf("checkpoint: %w", err)
			}
			if !keep {
				destroy(first)
				firstDestroyed = true
			}
			defer func() {
				if keep {
					logger.Info("keeping checkpoint", "name", checkpointName, "id", captured.ID)
					return
				}
				if err := client.DeleteCheckpoint(context.WithoutCancel(ctx), captured.ID); err != nil {
					logger.Error("delete checkpoint failed", "name", checkpointName, "error", err)
				}
			}()

			restored, err := create(railway.CreateOptions{
				CheckpointName:     checkpointName,
				IdleTimeoutMinutes: 10,
				NetworkIsolation:   cfg.NetworkIsolation(),
			})
			if err != nil {
				return fmt.Errorf("restore checkpoint: %w", err)
			}
			defer destroy(restored)
			if err := exec(restored, "test \"$(cat /tmp/actions-builds-checkpoint-smoke)\" = checkpoint-smoke && echo checkpoint restore ok"); err != nil {
				return fmt.Errorf("verify checkpoint restore: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&command, "command", "echo hello from railway sandbox && uname -a", "command to run")
	cmd.Flags().BoolVar(&checkpoint, "checkpoint", false, "also capture, restore, verify, and delete a named checkpoint")
	cmd.Flags().BoolVar(&keep, "keep", false, "do not destroy sandboxes or checkpoints")
	return cmd
}
