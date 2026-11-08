// Package swarm provides a Provisioner backend that runs one replicated
// agent service per user on a Docker Swarm cluster (medium deployments).
package swarm

import (
	"context"
	"fmt"
	"log/slog"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanorunner/internal/config"
	"gitee.com/kobeyoung81/nanorunner/internal/runner"
	"gitee.com/kobeyoung81/nanorunner/internal/runner/backend/netutil"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
)

// Backend provisions per-user nanosec agent services on a Swarm manager.
type Backend struct {
	cli *client.Client
}

// New creates a Swarm backend from the environment.
func New() (*Backend, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create swarm client: %w", err)
	}
	return &Backend{cli: cli}, nil
}

// Provision creates a single-replica service for the agent container. It is
// idempotent: an existing service with the same name is treated as already
// provisioned.
func (b *Backend) Provision(ctx context.Context, req nanoprotocol.ContainerRequest, opts runner.ProvisionOptions) error {
	name := ServiceName(req)

	if _, _, err := b.cli.ServiceInspectWithRaw(ctx, name, types.ServiceInspectOptions{}); err == nil {
		slog.Info("service already exists", "user_id", req.UserID, "service", name)
		return nil
	} else if !client.IsErrNotFound(err) {
		return fmt.Errorf("inspect service %s: %w", name, err)
	}

	args := []string{"hypervisor", "--user", req.UserID, "--bus-url", opts.BusURL}
	if req.BindingID > 0 {
		args = append(args, "--binding", fmt.Sprintf("%d", req.BindingID))
	}

	replicas := uint64(1)
	username := runner.UserName(req)
	mounts, err := storageMounts(opts.Storage, req)
	if err != nil {
		return fmt.Errorf("setup storage %s: %w", req.UserID, err)
	}
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: name},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:    opts.Image,
				Args:     args,
				Env:      agentEnv(username, opts.BusToken, opts.Timezone),
				Hostname: runner.AgentHostname,
				Mounts:   mounts,
			},
			Resources: &swarm.ResourceRequirements{
				Limits: &swarm.Limit{
					MemoryBytes: opts.Quota.MemoryMB * 1024 * 1024,
					NanoCPUs:    opts.Quota.CPUs,
				},
			},
			RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionAny},
		},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
	}
	network, err := netutil.ResolveNetwork(ctx, b.cli, opts.Network)
	if err != nil {
		slog.Warn("failed to resolve service network; using default", "user_id", req.UserID, "error", err)
		network = ""
	}
	if network != "" {
		spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: network}}
	}

	if _, err := b.cli.ServiceCreate(ctx, spec, types.ServiceCreateOptions{}); err != nil {
		return fmt.Errorf("create service for %s/%d: %w", req.UserID, req.BindingID, err)
	}

	slog.Info("service provisioned", "user_id", req.UserID, "binding_id", req.BindingID, "service", spec.Annotations.Name)
	return nil
}

// Deprovision removes the agent service for a request.
func (b *Backend) Deprovision(ctx context.Context, req nanoprotocol.ContainerRequest, _ runner.ProvisionOptions) error {
	name := ServiceName(req)
	if err := b.cli.ServiceRemove(ctx, name); err != nil {
		return fmt.Errorf("remove service %s: %w", name, err)
	}
	slog.Info("service disposed", "user_id", req.UserID, "binding_id", req.BindingID, "service", name)
	return nil
}

// agentEnv is the container environment: the agent identity plus the shared bus
// token, without which the agent cannot reach the bus at all.
func agentEnv(username, busToken, timezone string) []string {
	env := []string{"AGENT_USERNAME=" + username}
	if busToken != "" {
		env = append(env, "NANOSEC_BUS_INTERNAL_TOKEN="+busToken)
	}
	if timezone != "" {
		env = append(env, "TZ="+timezone)
	}
	return env
}

// storageMounts returns the service mounts that persist the agent data root. A
// named volume is declared on the service (Docker Swarm creates it on first
// use); with "bind" a host path is bind-mounted (single-manager only).
func storageMounts(st config.StorageConfig, req nanoprotocol.ContainerRequest) ([]mount.Mount, error) {
	path := st.DataMountPath(runner.UserName(req))

	if st.Backend == "bind" {
		if st.BindPath == "" {
			return nil, fmt.Errorf("storage backend 'bind' requires bindPath")
		}
		return []mount.Mount{
			{Type: mount.TypeBind, Source: st.BindPath, Target: path},
		}, nil
	}

	return []mount.Mount{
		{Type: mount.TypeVolume, Source: runner.DataVolumeName(req), Target: path},
	}, nil
}

// ServiceName returns the deterministic Swarm service name for a request.
func ServiceName(req nanoprotocol.ContainerRequest) string {
	if req.BindingID > 0 {
		return fmt.Sprintf("nanosec-agent-%s-%d", req.UserID, req.BindingID)
	}
	return fmt.Sprintf("nanosec-agent-%s", req.UserID)
}
