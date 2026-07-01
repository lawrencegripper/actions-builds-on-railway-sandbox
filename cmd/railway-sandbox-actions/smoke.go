package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/lawrencegripper/railway-sandbox-actions/internal/config"
	"github.com/lawrencegripper/railway-sandbox-actions/internal/railway"
)

// smokeCmd exercises the Railway path only (create sandbox -> exec -> destroy),
// with no GitHub involvement, so the sandbox+exec transport can be verified
// against the live API.
func smokeCmd(cfg *config.Config) *cobra.Command {
	var command string
	var keep bool
	cmd := &cobra.Command{
		Use:   "smoke",
		Short: "Create a sandbox, run a command, and destroy it (Railway transport check)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg.Defaults()
			if cfg.RailwayToken == "" || cfg.RailwayEnvironmentID == "" {
				return fmt.Errorf("set RAILWAY_API_TOKEN and RAILWAY_ENVIRONMENT_ID (or --railway-* flags)")
			}
			logger := cfg.Logger()
			ctx, cancel := context.WithTimeout(cmd.Context(), 8*time.Minute)
			defer cancel()

			client, err := cfg.RailwayClient(logger.WithGroup("railway"))
			if err != nil {
				return err
			}

			logger.Info("creating sandbox")
			sb, err := client.CreateSandbox(ctx, railway.CreateOptions{
				IdleTimeoutMinutes: 10,
				NetworkIsolation:   cfg.NetworkIsolation(),
			})
			if err != nil {
				return fmt.Errorf("create: %w", err)
			}
			logger.Info("sandbox running", "id", sb.ID, "region", sb.Region)

			if !keep {
				defer func() {
					if err := client.DestroySandbox(context.WithoutCancel(ctx), sb.ID); err != nil {
						logger.Error("destroy failed", "error", err.Error())
					} else {
						logger.Info("sandbox destroyed", "id", sb.ID)
					}
				}()
			}

			logger.Info("running command", "command", command)
			res, err := client.Exec(ctx, sb.ID, command, railway.ExecOptions{
				OnStdout: func(b []byte) { os.Stdout.Write(b) },
				OnStderr: func(b []byte) { os.Stderr.Write(b) },
			})
			if err != nil {
				return fmt.Errorf("exec: %w", err)
			}
			fmt.Printf("\n--- exit code: %d ---\n", res.ExitCode)
			if keep {
				logger.Info("keeping sandbox", "id", sb.ID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&command, "command", "echo hello from railway sandbox && uname -a", "command to run")
	cmd.Flags().BoolVar(&keep, "keep", false, "do not destroy the sandbox afterwards")
	return cmd
}
