// Package main provides the nanorunner CLI binary.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanorunner/internal/config"
	runnerlog "gitee.com/kobeyoung81/nanorunner/internal/log"
	"gitee.com/kobeyoung81/nanorunner/internal/runner"
	"gitee.com/kobeyoung81/nanorunner/internal/runner/backend/docker"
	"gitee.com/kobeyoung81/nanorunner/internal/runner/backend/k8s"
	"gitee.com/kobeyoung81/nanorunner/internal/runner/backend/swarm"
	provserver "gitee.com/kobeyoung81/nanorunner/internal/server"
	"gitee.com/kobeyoung81/nanorunner/internal/store"
	"gitee.com/kobeyoung81/nanorunner/migrations"
	"github.com/spf13/cobra"
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

			cfg, err := config.LoadRunnerConfig(config.AdminURL())
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

			// The provisioning queue is this service's; opening it and applying its
			// manifests is what makes this the writer.
			st, err := store.Open(config.MongoURI())
			if err != nil {
				return fmt.Errorf("store: %w", err)
			}
			defer st.Close()
			manifests, err := migrations.Load()
			if err != nil {
				return fmt.Errorf("load migrations: %w", err)
			}
			if ran, err := st.Migrate(manifests); err != nil {
				return fmt.Errorf("migrate: %w", err)
			} else if len(ran) > 0 {
				slog.Info("applied migrations", "count", len(ran))
			}

			busClient, err := nanoprotocol.NewClient(resolvedBusURL, nanoprotocol.WithToken(config.InternalToken()))
			if err != nil {
				return fmt.Errorf("connect to bus: %w", err)
			}
			defer busClient.Close()

			// Register in this service's own registry; the console reads it through
			// the bus, which no longer writes it.
			hostname, _ := os.Hostname()
			dockerHost := os.Getenv("DOCKER_HOST")
			if dockerHost == "" {
				dockerHost = "local"
			}
			if err := st.RegisterRunner(cfg.Runner.RunnerID, hostname, dockerHost); err != nil {
				return fmt.Errorf("register runner: %w", err)
			}
			slog.Info("runner registered", "runner_id", cfg.Runner.RunnerID)

			provisioner, err := newProvisioner(cfg)
			if err != nil {
				return fmt.Errorf("create provisioner: %w", err)
			}

			r := runner.New(cfg, st, busClient, provisioner)

			// The console's container actions reach the queue through here; the bus
			// forwards them so no caller had to learn a new address.
			api := provserver.New(st)
			go func() {
				slog.Info("provisioner api listening", "addr", config.HTTPAddr())
				if err := api.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					slog.Error("provisioner api", "error", err)
				}
			}()
			defer func() { _ = api.Shutdown() }()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// A heartbeat every minute keeps the row online (the staleness window is
			// two minutes) and sweeps runners that stopped reporting.
			go runHeartbeat(ctx, st, cfg.Runner.RunnerID)

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

// heartbeatInterval is the runner's own reporting cadence. It has to stay well
// inside store.RunnerStaleTimeout, or a busy loop would mark itself offline.
const heartbeatInterval = time.Minute

// runHeartbeat reports this runner's heartbeat and marks the rows of runners that
// stopped reporting as offline, so the fleet view reflects reality.
func runHeartbeat(ctx context.Context, st *store.Store, runnerID string) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := st.TouchRunner(runnerID); err != nil {
				slog.Warn("runner heartbeat failed", "runner_id", runnerID, "error", err)
			}
			if err := st.MarkStaleRunnersOffline(); err != nil {
				slog.Warn("sweep stale runners failed", "error", err)
			}
		}
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
