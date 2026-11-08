# nanorunner

Container provisioning service for MyriadMind.

## Overview

`nanorunner` polls the bus (`nanobus`) for pending container requests, provisions a container (or service) per user/agent with resource limits based on tier, and reports status back to the bus. It supports three pluggable provisioner backends — **docker**, **swarm**, and **k8s** — selected at runtime via the `runner.backend` config item.

## Architecture

```
nanorunner
  |-- runner/
  |   |-- runner.go      (poll loop, request dispatch)
  |   |-- provisioner.go (Provisioner interface)
  |   |-- backend/
  |       |-- docker/    (Docker SDK: containers + resource limits)
  |       |-- swarm/     (Docker Swarm: single-replica services per user)
  |       |-- k8s/       (Kubernetes: per-user Deployments via in-cluster client)
  |
  |-- config/
  |   |-- config.go      (RunnerConfig, quotas)
  |   |-- loader.go      (bus-backed config loader)
  |
  |-- WebSocket --> nanobus (PollPendingContainers, UpdateContainerStatus, RegisterRunner)
  |
  Docker Engine / Swarm manager / kube-apiserver
```

## Lifecycle

```
1. Startup
   - Load config from bus (backend, agentImage, network, quotas)
   - Register as a runner: RegisterRunner(runnerID, hostname, dockerHost)

2. Poll Loop (every PollInterval seconds)
   - PollPendingContainers(runnerID)
   - For each pending request:
     a. Determine quota by user tier (free/pro/ultra)
     b. Provisioner.Provision(...)  -- container (docker), service (swarm), or Deployment (k8s)
     c. UpdateContainerStatus(userID, bindingID, "running")
     d. UpdateUserStatus(userID, "active")

3. Admin dispose
   - Deletion goes through the `disposing` -> `disposed` lifecycle (migration 023)
```

## Provisioner Backends

| Backend | Config value | Provision unit | Notes |
|---------|--------------|----------------|-------|
| Docker | `docker` (default) | container | Local/remote Docker Engine via socket |
| Swarm | `swarm` | single-replica service | One service per user with resource limits |
| Kubernetes | `k8s` | Deployment | In-cluster client; RBAC in `deploy/k8s/rbac.yaml` |

The backend is set with the bus config item:

```bash
nanobus config set runner runner.backend swarm
```

## Quota Tiers

Per-tier limits are stored as config items `runner.quota.<tier>.memory` (MB) and
`runner.quota.<tier>.cpu` (nanoCPUs; `1e9` = 1 CPU).

| Tier | Memory | CPU |
|------|--------|-----|
| `free` | 512 MB | 1 CPU |
| `pro` | 2048 MB | 2 CPU |
| `ultra` | 4096 MB | 4 CPU |

## Container Details

- **Naming**: deterministic per (user, binding) — e.g. `nanosec-agent-{userID}-{bindingID}`
- **Image**: config item `agentImage` (default `kobeyoung81/nanosec:latest`)
- **Network**: config item `network` (default `nanosec_default`)
- **Timezone**: config item `runner.agentTimezone` (default `Asia/Shanghai`), injected as the `TZ` environment variable (empty = no `TZ`, i.e. UTC). The agent image sets no timezone, so without it prompt dates, cron schedules and git timestamps run in UTC. The value is re-read from the bus before every provisioning, so changing it takes effect on the next container created or restarted — no runner restart needed.
- **Bus URL**: passed as an environment variable to the agent container
- **Lifecycle**: containers persist the agent state on a mounted volume

## Provisioner Interface

```go
type Provisioner interface {
    Provision(ctx context.Context, req nanoprotocol.ContainerRequest, opts ProvisionOptions) error
    Deprovision(ctx context.Context, req nanoprotocol.ContainerRequest, opts ProvisionOptions) error
}
```

The interface enables easy mocking for unit tests of the `Runner`.

## CLI Reference

```bash
nanorunner                               # Start runner service
nanorunner --bus-url ws://bus:9000        # Specify bus URL
nanorunner --runner-id nanorunner         # Pin a stable runner identity (recommended)
nanorunner version                        # Print version info
```

> **Stable runner identity**: pass `--runner-id <name>` (or set the `runner.runnerID`
> config item) so every restart reuses the same `runners` row instead of inserting a
> new one per deploy. The bus marks runners whose heartbeat goes stale (default 2 min)
> as `offline` and exposes `DELETE /api/v1/runners/{id}` for pruning dead rows by hand.

## Configuration

Loaded from the bus at startup via `GetConfigItems("runner")` (seeded by `nanobus config seed`).

```json
{
  "runner": {
    "pollInterval": 5,
    "runnerID": "myhost",
    "backend": "docker"
  },
  "agentImage": "kobeyoung81/nanosec:latest",
  "agentTimezone": "Asia/Shanghai",
  "network": "nanosec_default",
  "quotas": {
    "free":  { "memoryMB": 512,  "cpus": 1000000000 },
    "pro":   { "memoryMB": 2048, "cpus": 2000000000 },
    "ultra": { "memoryMB": 4096, "cpus": 4000000000 }
  }
}
```

Environment prefix: `NANOSEC_RUNNER_` (e.g., `NANOSEC_RUNNER_BUS_URL`).

### Network resolution

The `network` config item names the Docker network agent containers attach to
(default `nanosec_default`). Because the actual network name depends on the
Compose project name, `nanorunner` resolves it as follows:

1. `NANOSEC_RUNNER_NETWORK` env var (explicit override)
2. the `network` config item, if a network with that name exists on the daemon
3. the network the runner container itself is attached to (auto-detected)

`NANOSEC_RUNNER_AGENT_IMAGE` similarly overrides the `agentImage` config item,
and `NANOSEC_RUNNER_AGENT_TIMEZONE` overrides `runner.agentTimezone`.

## Deployment

### Requirements
- Docker Engine with accessible socket (`/var/run/docker.sock`) — or Swarm/K8s credentials for the other backends
- Network access to `nanobus` WebSocket endpoint

### Docker
```bash
docker build -t nanorunner .
docker run -v /var/run/docker.sock:/var/run/docker.sock nanorunner --bus-url ws://nanobus:9000
```

### Docker Compose
Part of top-level `docker-compose.yml` (docker backend, mounts the host socket).

### Docker Swarm
Use `deploy/swarm/docker-stack.yml` from the super-repo and set `runner.backend=swarm`.

### Kubernetes
Apply `nanorunner/deploy/k8s/rbac.yaml` + `runner.yaml` from the super-repo and set `runner.backend=k8s`.

### Standalone
```bash
go build ./cmd/nanorunner
./nanorunner --bus-url ws://localhost:9000
```

## Packages

| Package | Purpose |
|---------|---------|
| `cmd/nanorunner` | CLI entry point |
| `internal/runner` | Poll loop, lifecycle, Provisioner interface |
| `internal/runner/backend/docker` | Docker SDK container provisioning |
| `internal/runner/backend/swarm` | Swarm service provisioning |
| `internal/runner/backend/k8s` | Kubernetes Deployment provisioning |
| `internal/config` | Configuration types and bus-backed loader |
| `internal/log` | Custom slog log level |
