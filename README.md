# nanoprovisioner

Containers and tunnels: the module that asks Docker, Swarm or Kubernetes for a
user's agent container, and the place the platform's tunnels terminate.

It is one of the eight first-class services. The binary and the compose service are
still called `nanorunner`; the repository, image and prefix are `nanoprovisioner` /
`provisioner_`.

## What it is

- **A container request queue.** A user or an operator asks; this module turns the
  request into a container with the tier's limits.
- **Three backends.** Docker on one host, Swarm, or Kubernetes — chosen by
  `runner.backend`, with per-tier memory, CPU and storage.
- **The tunnel endpoint.** Hypervisors and terminals connect here; a request for a
  connection this replica does not hold is relayed to the one that does.
- **A runner registry.** Every runner keeps a row with a heartbeat, so the fleet is
  visible and stale rows can be pruned.
- **Storage that survives a node.** Optional shared-volume provisioning for
  clusters, so a container can be rebuilt somewhere else.

## Where it sits

| | |
|---|---|
| Collections | `provisioner_container_requests`, `provisioner_runners`, `provisioner_tunnels` |
| Port | `8080` in the network |
| Upstream | `nanobus` (requests from onboarding), `nanoportal` (a console action), `nanoadmin` (restart and dispose) |
| Downstream | the container runtime — Docker socket, Swarm manager or the Kubernetes API — and the agent containers it creates |
| Health | `GET /healthz`, plus `/metrics` |

## Quick start

```bash
go build ./cmd/nanorunner
NANOSEC_PROVISIONER_MONGO_URI="mongodb://localhost:27017/nanosec" \
NANOSEC_RUNNER_BACKEND=docker \
NANOSEC_RUNNER_AGENT_IMAGE=docker.io/kobeyoung81/nanoagent:0.1.0 \
NANOSEC_RUNNER_BUS_URL="http://localhost:9000" \
NANOSEC_BUS_INTERNAL_TOKEN="…" \
  ./nanorunner
```

The Docker backend needs the socket mounted (`/var/run/docker.sock`); the Swarm
backend needs a manager; the Kubernetes backend needs a service account that may
create pods, services and persistent volume claims.

## Configuration

| Variable | Meaning |
|---|---|
| `NANOSEC_PROVISIONER_MONGO_URI` | The platform database |
| `NANOSEC_PROVISIONER_ADDR` | Listen address, default `:8080` |
| `NANOSEC_RUNNER_BACKEND` | `docker`, `swarm` or `k8s` |
| `NANOSEC_RUNNER_AGENT_IMAGE` | The agent image every container runs |
| `NANOSEC_RUNNER_BUS_URL` | The container's bus URL, passed into the container |
| `NANOSEC_RUNNER_NETWORK` | Docker/Swarm network the containers join |
| `NANOSEC_RUNNER_NAME_PREFIX` | Prefix for container names — one prefix names a stack |
| `NANOSEC_RUNNER_ADVERTISE_URL` | The address peers use to relay a tunnel here |
| `NANOSEC_RUNNER_STORAGE_BACKEND` / `…_ACCESS_MODE` | Shared-volume backend and access mode (`ReadWriteMany` for clusters) |
| `NANOSEC_BUS_INTERNAL_TOKEN` | Shared secret for every internal surface |

Quotas, timezone, image overrides and the storage class are configuration items in
`admin_config` (`runner.*`), not environment variables.

## API

| Surface | What it does |
|---|---|
| `POST /api/v1/internal/containers` | Enqueue a container request |
| `POST /api/v1/internal/containers/{userID}/restart`, `/reprovision` | Ask for a new container |
| `POST /api/v1/internal/containers/{userID}/dispose` | Remove one |
| `GET /ws/hypervisor` | A hypervisor registers its tunnel |
| `POST /api/v1/internal/tunnels/{name}/request` | Relay a request through a tunnel |
| `POST /api/v1/internal/tunnels/drain` | Mark this replica as draining before a rollout |
| `GET /ws/terminal` | The terminal bridge |
| `POST` / `DELETE /api/v1/internal/runners[/{id}]` | Register or prune a runner |

## Design decisions

- **Tunnels terminate here, not at the bus.** The registry is
  `provisioner_tunnels`; a request that arrives at the wrong replica is relayed to
  the one holding the connection, so no sticky routing is needed.
- **Drain before a rollout.** Tunnels are long-lived; a replica that is about to
  restart announces it so new registrations land elsewhere.
- **The queue is idempotent per agent.** Asking twice for the same agent does not
  create two containers.
- **Storage is a capability, not an assumption.** Without a shared volume a container
  can only be rebuilt on the same node; with one, anywhere.
- **At least three replicas in a cluster.** Each container holds one connection;
  capacity is roughly 60 KB per idle tunnel plus the transfer buffers.

## Docs

- [`docs/DEPLOYMENT.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/DEPLOYMENT.md) — compose, Swarm, Kubernetes and the tunnel capacity rules
- [`docs/ARCHITECTURE.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/ARCHITECTURE.md) — the platform around this module
- [`docs/README.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/README.md) — every document

## Status

Released as part of `v0.1.0`; the version of this module is pinned in the
super-repo's `versions.lock`.

## Contributing and security

[`CONTRIBUTING.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/CONTRIBUTING.md)
for setup and commit rules; report vulnerabilities privately through
[`SECURITY.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/SECURITY.md), never
in a public issue.

## License

[AGPL-3.0](./LICENSE). The name and logo are covered separately — see
[`NOTICE`](./NOTICE).
