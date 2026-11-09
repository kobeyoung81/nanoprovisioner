// Package docker provides a Provisioner backend that runs agent containers
// directly on a local Docker daemon (single-host compose deployments).
package docker

import (
	"context"
	"fmt"
	"log/slog"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanorunner/internal/config"
	"gitee.com/kobeyoung81/nanorunner/internal/runner"
	"gitee.com/kobeyoung81/nanorunner/internal/runner/backend/netutil"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// Backend provisions per-user nanosec agent containers on a local Docker daemon.
type Backend struct {
	cli *client.Client
}

// New creates a Docker backend from the environment (DOCKER_HOST etc).
func New() (*Backend, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &Backend{cli: cli}, nil
}

// Provision creates and starts the agent container for a request. It is
// idempotent: a running container with the same name is treated as already
// provisioned, and a stale (created/exited) container is removed first so the
// deterministic name can be reused.
func (b *Backend) Provision(ctx context.Context, req nanoprotocol.ContainerRequest, opts runner.ProvisionOptions) error {
	name := ContainerName(req, opts.NamePrefix)

	if existing, err := b.cli.ContainerInspect(ctx, name); err == nil {
		if existing.State != nil && existing.State.Running {
			slog.Info("container already running", "user_id", req.UserID, "name", name, "id", existing.ID)
			return nil
		}
		slog.Info("removing stale container", "user_id", req.UserID, "name", name, "state", stateOf(existing.State))
		if err := b.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); err != nil {
			return fmt.Errorf("remove stale container %s: %w", name, err)
		}
	} else if !client.IsErrNotFound(err) {
		return fmt.Errorf("inspect container %s: %w", name, err)
	}

	network, err := netutil.ResolveNetwork(ctx, b.cli, opts.Network)
	if err != nil {
		slog.Warn("failed to resolve container network; using default", "user_id", req.UserID, "error", err)
		network = ""
	}

	mounts, err := b.storageMounts(ctx, opts.Storage, req, opts.NamePrefix)
	if err != nil {
		return fmt.Errorf("setup storage %s: %w", req.UserID, err)
	}
	username := runner.UserName(req)

	b.warnIfOverCommitted(ctx, opts.Quota)

	spec := provisionConfig(req, opts, mounts, username)

	hostConfig := spec.hostConfig
	if network != "" {
		hostConfig.NetworkMode = container.NetworkMode(network)
	}

	resp, err := b.cli.ContainerCreate(ctx, spec.config, hostConfig, nil, nil, name)
	if err != nil {
		return fmt.Errorf("create container for %s/%d: %w", req.UserID, req.BindingID, err)
	}

	if err := b.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start container for %s/%d: %w", req.UserID, req.BindingID, err)
	}

	slog.Info("container provisioned", "user_id", req.UserID, "binding_id", req.BindingID, "container_id", resp.ID)
	return nil
}

// provisionSpec holds the Docker container configuration for an agent.
type provisionSpec struct {
	config     *container.Config
	hostConfig *container.HostConfig
}

// Agent containers log through the json-file driver. Without a bound, a chatty
// agent fills the host disk via /var/lib/docker/containers/*/*-json.log — this
// host has run out of space before. Three files of 10 MB cap a container at
// roughly 30 MB. Set here rather than daemon-wide: a daemon log-opts change
// needs a dockerd restart, which would stop every container including the
// agents themselves.
const (
	agentLogMaxSize = "10m"
	agentLogMaxFile = "3"
)

// agentCapabilities is the capability set every agent container gets.
//
// It is spelled out rather than inherited so the container's powers cannot
// change silently when the engine's default set changes. The list reproduces
// exactly what an unprivileged Docker container gets today (the 0xa80425fb
// bounding set measured on a live agent container), because that default is
// what the entrypoint and the agent's own tooling rely on:
//
//   - entrypoint.sh starts as root to remove the stock uid-1000 'ubuntu' user,
//     create the runtime identity, chown its home, write /etc/sudoers.d/agent
//     and gosu into that user;
//   - the agent user holds NOPASSWD sudo so it can apt-get install tools.
//
// Dropping any of these breaks container startup or tool installation, so a
// narrower set must be validated against a real agent container first.
// NET_RAW and SYS_CHROOT are the obvious first candidates to remove.
var agentCapabilities = strslice.StrSlice{
	"CHOWN",
	"DAC_OVERRIDE",
	"FOWNER",
	"FSETID",
	"KILL",
	"SETGID",
	"SETUID",
	"SETPCAP",
	"NET_BIND_SERVICE",
	"NET_RAW",
	"SYS_CHROOT",
	"MKNOD",
	"AUDIT_WRITE",
	"SETFCAP",
}

// pidsLimit converts the configured cap into the SDK's pointer form: a
// non-positive value means "leave Docker's default" (unlimited), which is how
// the SDK expresses "unset".
func pidsLimit(n int64) *int64 {
	if n <= 0 {
		return nil
	}
	return &n
}

// boolPtr returns a pointer to v. The Docker SDK models optional flags such as
// HostConfig.Init as *bool so that "unset" is distinguishable from "false".
func boolPtr(v bool) *bool { return &v }

// provisionConfig builds the Docker Config and HostConfig for an agent
// container. The rootfs stays writable so admins/users can install tools
// (apt/pip), and the agent user holds NOPASSWD sudo — the container must NOT
// set no-new-privileges (it would prevent sudo entirely). Hardening is instead
// the non-root uid-1000 runtime identity and the workspace path guards.
//
// A custom seccomp profile can be layered on via opts.SeccompProfile, but the
// default is deliberately to leave it unset so Docker's own profile applies:
// "seccomp=<path>" *replaces* that profile rather than adding to it, and the
// profile shipped in seccomp/ is a denylist that is weaker than Docker's
// allowlist in places. Read seccomp/README.md before enabling it. The path is
// a daemon-host path resolved by the Docker daemon itself; when configured but
// absent on the host, container creation fails loudly rather than silently
// weakening the profile.
func provisionConfig(req nanoprotocol.ContainerRequest, opts runner.ProvisionOptions, mounts []mount.Mount, username string) provisionSpec {
	cmd := []string{"hypervisor", "--user", req.UserID, "--bus-url", opts.BusURL}
	if req.BindingID > 0 {
		cmd = append(cmd, "--binding", fmt.Sprintf("%d", req.BindingID))
	}

	// NOTE: do not add no-new-privileges here — the agent user needs sudo
	// (passwordless) to install tools in its terminal, and the flag forbids it.
	var securityOpts []string
	if opts.SeccompProfile != "" {
		securityOpts = append(securityOpts, "seccomp="+opts.SeccompProfile)
	}

	memBytes := opts.Quota.MemoryMB * 1024 * 1024

	return provisionSpec{
		config: &container.Config{
			Image:    opts.Image,
			Hostname: runner.AgentHostname,
			Env:      agentEnv(username, opts.BusToken, opts.Timezone, opts.AdminURL),
			Cmd:      cmd,
		},
		hostConfig: &container.HostConfig{
			Resources: container.Resources{
				Memory:    memBytes,
				NanoCPUs:  opts.Quota.CPUs,
				PidsLimit: pidsLimit(opts.Quota.Pids),
				// MemorySwap is memory + swap combined. Left unset it defaults
				// to twice Memory, so a container may swap and the memory cap
				// becomes advisory. Setting it equal to Memory is how Docker
				// expresses "no swap".
				MemorySwap: memBytes,
			},
			RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
			Mounts:        mounts,
			SecurityOpt:   securityOpts,
			// Spelling the capability set out (drop everything, add the set
			// above) keeps it from drifting with the engine's defaults.
			CapDrop: strslice.StrSlice{"ALL"},
			CapAdd:  agentCapabilities,
			// PID 1 is the nanosec hypervisor (a Go program, which does not
			// reap). Orphans reparented to it — every command the agent runs
			// forks — would otherwise accumulate as zombies for the life of
			// the container. --init injects a reaping init as PID 1.
			Init: boolPtr(true),
			LogConfig: container.LogConfig{
				Type: "json-file",
				Config: map[string]string{
					"max-size": agentLogMaxSize,
					"max-file": agentLogMaxFile,
				},
			},
		},
	}
}

// agentEnv is the container environment: the agent identity plus the shared
// bus token, without which the agent cannot reach the bus at all. The timezone
// is set here rather than in the image so an operator can change it in the
// config center; empty leaves TZ unset, which is what the image does anyway
// (the container then runs in UTC).
func agentEnv(username, busToken, timezone, adminURL string) []string {
	env := []string{"AGENT_USERNAME=" + username}
	if busToken != "" {
		env = append(env, "NANOSEC_BUS_INTERNAL_TOKEN="+busToken)
	}
	if adminURL != "" {
		env = append(env, "NANOSEC_ADMIN_URL="+adminURL)
	}
	if timezone != "" {
		env = append(env, "TZ="+timezone)
	}
	return env
}

// storageMounts returns the container mounts that persist the agent data root.
// With the "volume" backend (default) a named volume is created (if absent) and
// mounted at the data root; with "bind" a host path is bind-mounted instead.
func (b *Backend) storageMounts(ctx context.Context, st config.StorageConfig, req nanoprotocol.ContainerRequest, prefix string) ([]mount.Mount, error) {
	path := st.DataMountPath(runner.UserName(req))

	if st.Backend == "bind" {
		if st.BindPath == "" {
			return nil, fmt.Errorf("storage backend 'bind' requires bindPath")
		}
		return []mount.Mount{
			{Type: mount.TypeBind, Source: st.BindPath, Target: path},
		}, nil
	}

	name := runner.DataVolumeName(req, prefix)
	if _, err := b.cli.VolumeInspect(ctx, name); err != nil {
		if client.IsErrNotFound(err) {
			volOpts := volume.CreateOptions{Name: name, Driver: st.Driver}
			if st.DriverOptsKey != "" && st.DriverOptsValue != "" {
				volOpts.DriverOpts = map[string]string{st.DriverOptsKey: st.DriverOptsValue}
			}
			if _, err := b.cli.VolumeCreate(ctx, volOpts); err != nil {
				return nil, fmt.Errorf("create data volume %s: %w", name, err)
			}
			slog.Info("created data volume", "user_id", req.UserID, "volume", name)
		} else if err != nil {
			return nil, fmt.Errorf("inspect data volume %s: %w", name, err)
		}
	}

	return []mount.Mount{
		{Type: mount.TypeVolume, Source: name, Target: path},
	}, nil
}

// stateOf returns a container state's status string, or "unknown" when nil.
func stateOf(state *types.ContainerState) string {
	if state == nil {
		return "unknown"
	}
	return state.Status
}

// Deprovision stops and removes the agent container for a request.
func (b *Backend) Deprovision(ctx context.Context, req nanoprotocol.ContainerRequest, opts runner.ProvisionOptions) error {
	name := ContainerName(req, opts.NamePrefix)

	if err := b.cli.ContainerStop(ctx, name, container.StopOptions{}); err != nil {
		slog.Warn("stop container failed", "name", name, "error", err)
	}
	if err := b.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); err != nil {
		slog.Warn("remove container failed", "name", name, "error", err)
	}

	slog.Info("container disposed", "user_id", req.UserID, "binding_id", req.BindingID, "name", name)
	return nil
}

// warnIfOverCommitted logs when this host's running containers, plus the one
// about to start, have been promised more memory than the host actually has.
//
// It never blocks provisioning — sharing a host between agents is the design,
// and the kernel decides who dies under pressure. The point is to leave a trace
// an operator can act on before the OOM killer picks a victim: a four-core host
// running a dozen agents at the 4 GB tier routinely sums its caps well past the
// machine's own RAM.
func (b *Backend) warnIfOverCommitted(ctx context.Context, quota config.Quota) {
	if quota.MemoryMB <= 0 {
		return
	}

	info, err := b.cli.Info(ctx)
	if err != nil || info.MemTotal <= 0 {
		return
	}
	listed, err := b.cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return
	}

	// types.Container carries no memory limit, so each container is inspected.
	committed := quota.MemoryMB * 1024 * 1024
	for _, c := range listed {
		insp, err := b.cli.ContainerInspect(ctx, c.ID)
		if err != nil || insp.HostConfig == nil {
			continue
		}
		committed += insp.HostConfig.Memory
	}
	if committed <= info.MemTotal {
		return
	}

	slog.Warn("agent containers over-commit this host's memory",
		"committed_bytes", committed,
		"host_bytes", info.MemTotal,
		"running_containers", len(listed),
		"requesting_mb", quota.MemoryMB)
}

// ContainerName returns the deterministic Docker container name for a request.
func ContainerName(req nanoprotocol.ContainerRequest, prefix string) string {
	if req.BindingID > 0 {
		return fmt.Sprintf("%snanosec-agent-%s-%d", prefix, req.UserID, req.BindingID)
	}
	return fmt.Sprintf("%snanosec-agent-%s", prefix, req.UserID)
}
