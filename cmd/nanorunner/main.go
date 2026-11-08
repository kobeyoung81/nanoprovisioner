// Package main provides the nanorunner CLI binary.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanorunner/internal/config"
	runnerlog "gitee.com/kobeyoung81/nanorunner/internal/log"
	"gitee.com/kobeyoung81/nanorunner/internal/runner"
	"gitee.com/kobeyoung81/nanorunner/internal/runner/backend/docker"
	"gitee.com/kobeyoung81/nanorunner/internal/runner/backend/k8s"
	"gitee.com/kobeyoung81/nanorunner/internal/runner/backend/swarm"
)

// Build-time variables set via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	setupLogger()

	var busURL string
	var runnerID string

	rootCmd := &cobra.Command{
		Use:   "nanorunner",
		Short: "Nanorunner - container provisioning service",
		Long: `Nanorunner polls the bus for pending container requests and
provisions Docker containers per user with resource quotas by tier.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedBusURL := config.ResolveBusURL(busURL)

			cfg, err := config.LoadRunnerConfig(resolvedBusURL)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if err := config.ValidateRunnerConfig(cfg); err != nil {
				return fmt.Errorf("invalid config: %w", err)
			}
			// A stable identity is reused across restarts so the runner registry
			// row is updated instead of duplicated on every redeploy.
			if runnerID != "" {
				cfg.Runner.RunnerID = runnerID
			}

			slog.Info("starting nanorunner", "bus_url", resolvedBusURL, "agent_image", cfg.AgentImage, "agent_timezone", cfg.AgentTimezone)

			// Set the resolved bus URL on config so containers inherit it.
			cfg.BusURL = resolvedBusURL

			busClient, err := nanoprotocol.NewClient(resolvedBusURL, nanoprotocol.WithToken(config.InternalToken()))
			if err != nil {
				return fmt.Errorf("connect to bus: %w", err)
			}
			defer busClient.Close()

			// Register this runner with the bus.
			hostname, _ := os.Hostname()
			dockerHost := os.Getenv("DOCKER_HOST")
			if dockerHost == "" {
				dockerHost = "local"
			}
			if _, err := busClient.RegisterRunner(cfg.Runner.RunnerID, hostname, dockerHost); err != nil {
				return fmt.Errorf("register runner: %w", err)
			}
			slog.Info("runner registered", "runner_id", cfg.Runner.RunnerID)

			provisioner, err := newProvisioner(cfg)
			if err != nil {
				return fmt.Errorf("create provisioner: %w", err)
			}

			r := runner.New(cfg, busClient, provisioner)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				sig := <-sigChan
				slog.Info("received signal, shutting down", "signal", sig)
				cancel()
			}()

			if err := r.Run(ctx); err != nil && err != context.Canceled {
				return fmt.Errorf("runner: %w", err)
			}

			slog.Info("nanorunner stopped")
			return nil
		},
	}

	rootCmd.PersistentFlags().StringVar(&busURL, "bus-url", "", "bus WebSocket URL (default: $NANOSEC_RUNNER_BUS_URL or ws://nanobus:9000)")
	rootCmd.PersistentFlags().StringVar(&runnerID, "runner-id", "", "stable runner identity reused across restarts (default: config runner.runnerID or hostname)")
	rootCmd.AddCommand(newVersionCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func newProvisioner(cfg *config.RunnerConfig) (runner.Provisioner, error) {
	switch cfg.Backend {
	case "swarm":
		return swarm.New()
	case "k8s":
		return k8s.New()
	default:
		return docker.New()
	}
}

func setupLogger() {
	level := slog.LevelInfo
	if env := os.Getenv("LOG_LEVEL"); env != "" {
		switch strings.ToUpper(env) {
		case "VERBOSE":
			level = runnerlog.LevelVerbose
		case "DEBUG":
			level = slog.LevelDebug
		case "WARN":
			level = slog.LevelWarn
		case "ERROR":
			level = slog.LevelError
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("nanorunner %s (commit: %s, built: %s)\n", version, commit, date)
		},
	}
}
