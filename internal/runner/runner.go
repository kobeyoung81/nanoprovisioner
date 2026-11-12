package runner

import (
	"context"
	"log/slog"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanorunner/internal/config"
)

// Runner polls the bus for pending container requests and manages agent containers
// via a platform-specific Provisioner backend.
type Runner struct {
	// busClient is still how a provisioned container reports the owner as
	// active: accounts belong to the bus until the auth slice moves them.
	busClient *nanoprotocol.Client
	// queue is the provisioning queue. It used to be the bus's, while this
	// service was its only poller and status writer; the queue is provisioning's.
	queue       Queue
	provisioner Provisioner
	config      *config.RunnerConfig
}

// New creates a new Runner.
// Queue is what the runner reads work from and reports back to. The concrete
// implementation is the provisioner's store; the interface keeps the loop
// testable without a database.
type Queue interface {
	PollPendingContainers(runnerID string) ([]nanoprotocol.ContainerRequest, error)
	UpdateContainerStatus(userID string, bindingID int64, status string) error
}

func New(cfg *config.RunnerConfig, queue Queue, busClient *nanoprotocol.Client, provisioner Provisioner) *Runner {
	return &Runner{
		busClient:   busClient,
		queue:       queue,
		provisioner: provisioner,
		config:      cfg,
	}
}

// Run starts the runner loop.
func (r *Runner) Run(ctx context.Context) error {
	interval := time.Duration(r.config.Runner.PollInterval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}

	slog.Info("runner started",
		"runner_id", r.config.Runner.RunnerID,
		"poll_interval", interval,
		"backend", r.config.Backend,
	)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			r.pollAndDispatch(ctx)
		}
	}
}

func (r *Runner) pollAndDispatch(ctx context.Context) {
	requests, err := r.queue.PollPendingContainers(r.config.Runner.RunnerID)
	if err != nil {
		slog.Error("poll pending containers failed", "error", err)
		return
	}

	for _, req := range requests {
		if req.Status == "disposing" {
			go r.disposeContainer(ctx, req)
			continue
		}
		if req.Status == "restarting" {
			go r.restartContainer(ctx, req)
			continue
		}
		go r.provisionContainer(ctx, req)
	}
}

func (r *Runner) provisionContainer(ctx context.Context, req nanoprotocol.ContainerRequest) {
	slog.Info("provisioning container", "user_id", req.UserID, "binding_id", req.BindingID, "level", req.Level)

	opts := r.provisionOptions(req)
	opts.Timezone = r.agentTimezone()

	if err := r.provisioner.Provision(ctx, req, opts); err != nil {
		slog.Error("provision container failed", "user_id", req.UserID, "binding_id", req.BindingID, "error", err)
		if serr := r.queue.UpdateContainerStatus(req.UserID, req.BindingID, "failed"); serr != nil {
			slog.Error("mark container failed", "user_id", req.UserID, "binding_id", req.BindingID, "error", serr)
		}
		return
	}

	if err := r.queue.UpdateContainerStatus(req.UserID, req.BindingID, "running"); err != nil {
		slog.Error("update container status to running", "user_id", req.UserID, "binding_id", req.BindingID, "error", err)
		return
	}

	if err := r.busClient.UpdateUserStatus(req.UserID, "active"); err != nil {
		slog.Error("update user status to active", "user_id", req.UserID, "error", err)
	}

	slog.Info("container running", "user_id", req.UserID, "binding_id", req.BindingID)
}

func (r *Runner) restartContainer(ctx context.Context, req nanoprotocol.ContainerRequest) {
	slog.Info("restarting container", "user_id", req.UserID, "binding_id", req.BindingID)

	if err := r.provisioner.Deprovision(ctx, req, r.provisionOptions(req)); err != nil {
		slog.Warn("deprovision before restart failed", "user_id", req.UserID, "binding_id", req.BindingID, "error", err)
	}

	opts := r.provisionOptions(req)
	opts.Timezone = r.agentTimezone()

	if err := r.provisioner.Provision(ctx, req, opts); err != nil {
		slog.Error("reprovision container failed", "user_id", req.UserID, "binding_id", req.BindingID, "error", err)
		if serr := r.queue.UpdateContainerStatus(req.UserID, req.BindingID, "failed"); serr != nil {
			slog.Error("mark container failed", "user_id", req.UserID, "binding_id", req.BindingID, "error", serr)
		}
		return
	}

	if err := r.queue.UpdateContainerStatus(req.UserID, req.BindingID, "running"); err != nil {
		slog.Error("update container status to running", "user_id", req.UserID, "binding_id", req.BindingID, "error", err)
		return
	}

	if err := r.busClient.UpdateUserStatus(req.UserID, "active"); err != nil {
		slog.Error("update user status to active", "user_id", req.UserID, "error", err)
	}

	slog.Info("container restarted", "user_id", req.UserID, "binding_id", req.BindingID)
}

func (r *Runner) disposeContainer(ctx context.Context, req nanoprotocol.ContainerRequest) {
	slog.Info("disposing container", "user_id", req.UserID, "binding_id", req.BindingID)

	if err := r.provisioner.Deprovision(ctx, req, r.provisionOptions(req)); err != nil {
		slog.Error("deprovision container failed", "user_id", req.UserID, "binding_id", req.BindingID, "error", err)
		return
	}

	if err := r.queue.UpdateContainerStatus(req.UserID, req.BindingID, "disposed"); err != nil {
		slog.Error("update container status to disposed", "user_id", req.UserID, "binding_id", req.BindingID, "error", err)
	}

	slog.Info("container disposed", "user_id", req.UserID, "binding_id", req.BindingID)
}

func (r *Runner) provisionOptions(req nanoprotocol.ContainerRequest) ProvisionOptions {
	return ProvisionOptions{
		Image:          r.config.AgentImage,
		BusURL:         r.config.BusURL,
		TunnelURL:      r.config.TunnelURL,
		AdminURL:       r.config.AdminURL,
		BusToken:       config.InternalToken(),
		Network:        r.config.Network,
		NamePrefix:     r.config.NamePrefix,
		Quota:          r.config.Quotas.QuotaForLevel(req.Level),
		Storage:        r.config.Storage,
		SeccompProfile: r.config.SeccompProfile,
	}
}

// agentTimezone returns the timezone to inject into a container about to be
// created, re-reading the bus so a config change reaches the next provisioning
// without a runner restart. A failed read never blocks provisioning: it falls
// back to the value loaded at startup (which already carries the default).
func (r *Runner) agentTimezone() string {
	items, err := config.FetchItems("runner")
	if err != nil {
		slog.Warn("re-read agent timezone failed; using the startup value",
			"error", err, "timezone", r.config.AgentTimezone)
		return r.config.AgentTimezone
	}
	return config.TimezoneFromItems(items, r.config.AgentTimezone)
}
