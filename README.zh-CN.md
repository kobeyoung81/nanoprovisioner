# nanoprovisioner

容器与隧道：向 Docker、Swarm 或 Kubernetes 要一个用户的智能体容器，并让平台的隧道在这里终止。

它是八个一级服务之一。二进制与 compose 服务名仍叫 `nanorunner`；仓库、镜像与前缀是
`nanoprovisioner` / `provisioner_`。

## 它是什么

- **一条容器请求队列。** 用户或运维提请求，这个模块把它变成一个带该档位限额的容器。
- **三个后端。** 单机 Docker、Swarm、Kubernetes —— 由 `runner.backend` 选，按档位给内存、CPU 与存储。
- **隧道端点。** hypervisor 与终端在这里连接；请求落到没有那条连接的副本时，被转给持有的那个。
- **runner 注册表。** 每个 runner 一行带心跳，集群看得见，陈旧行可以清。
- **能跨节点活下来的存储。** 集群可选配共享卷，容器就能在别的节点重建。

## 它在哪

| | |
|---|---|
| 集合 | `provisioner_container_requests`、`provisioner_runners`、`provisioner_tunnels` |
| 端口 | 网络内 `8080` |
| 上游 | `nanobus`（开户时的请求）、`nanoportal`（控制台操作）、`nanoadmin`（重启与销毁） |
| 下游 | 容器运行时 —— Docker socket、Swarm manager 或 Kubernetes API —— 以及它建出来的智能体容器 |
| 健康 | `GET /healthz`，以及 `/metrics` |

## 快速开始

```bash
go build ./cmd/nanorunner
NANOSEC_PROVISIONER_MONGO_URI="mongodb://localhost:27017/nanosec" \
NANOSEC_RUNNER_BACKEND=docker \
NANOSEC_RUNNER_AGENT_IMAGE=docker.io/kobeyoung81/nanoagent:0.1.0 \
NANOSEC_RUNNER_BUS_URL="http://localhost:9000" \
NANOSEC_BUS_INTERNAL_TOKEN="…" \
  ./nanorunner
```

Docker 后端要挂 socket（`/var/run/docker.sock`）；Swarm 后端要 manager；Kubernetes 后端要一个
能建 pod、service 与 PVC 的 service account。

## 配置

| 变量 | 含义 |
|---|---|
| `NANOSEC_PROVISIONER_MONGO_URI` | 平台库 |
| `NANOSEC_PROVISIONER_ADDR` | 监听地址，默认 `:8080` |
| `NANOSEC_RUNNER_BACKEND` | `docker`、`swarm` 或 `k8s` |
| `NANOSEC_RUNNER_AGENT_IMAGE` | 每个容器都跑的那个智能体镜像 |
| `NANOSEC_RUNNER_BUS_URL` | 传进容器里的总线地址 |
| `NANOSEC_RUNNER_NETWORK` | 容器加入的 Docker / Swarm 网络 |
| `NANOSEC_RUNNER_NAME_PREFIX` | 容器名前缀 —— 一个前缀就是一个栈 |
| `NANOSEC_RUNNER_ADVERTISE_URL` | 其它副本转发隧道时用的地址 |
| `NANOSEC_RUNNER_STORAGE_BACKEND` / `…_ACCESS_MODE` | 共享卷后端与访问模式（集群用 `ReadWriteMany`） |
| `NANOSEC_BUS_INTERNAL_TOKEN` | 所有内部面共用的密钥 |

配额、时区、镜像覆盖与 storage class 是 `admin_config` 里的配置项（`runner.*`），不是环境变量。

## API

| 面 | 做什么 |
|---|---|
| `POST /api/v1/internal/containers` | 入队一个容器请求 |
| `POST /api/v1/internal/containers/{userID}/restart`、`/reprovision` | 要一个新容器 |
| `POST /api/v1/internal/containers/{userID}/dispose` | 销毁一个 |
| `GET /ws/hypervisor` | hypervisor 注册它的隧道 |
| `POST /api/v1/internal/tunnels/{name}/request` | 通过隧道转发一个请求 |
| `POST /api/v1/internal/tunnels/drain` | 滚动前把本副本标成排空 |
| `GET /ws/terminal` | 终端桥 |
| `POST` / `DELETE /api/v1/internal/runners[/{id}]` | 注册或清掉一个 runner |

## 设计决策

- **隧道在 provisioner 终止，不在总线。** 注册表是 `provisioner_tunnels`；请求落到错的副本就被转给
  持有连接的那个，所以不需要粘性路由。
- **滚动前先排空。** 隧道是长连接；要重启的副本先声明自己，新的注册就落到别处。
- **队列对智能体幂等。** 对同一个智能体问两次，不会建出两个容器。
- **存储是能力，不是前提。** 没有共享卷，容器只能在原节点重建；有了它，哪儿都行。
- **集群至少三个副本。** 每个容器占一条连接；容量大致是每条空闲隧道 60 KB，加上传输缓冲。

## 文档

- [`docs/DEPLOYMENT.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/DEPLOYMENT.md) —— compose、Swarm、Kubernetes 与隧道容量规则
- [`docs/ARCHITECTURE.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/ARCHITECTURE.md) —— 它所在的整个平台
- [`docs/README.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/README.md) —— 全部文档索引

## 状态

随 `v0.1.0` 发布；本模块的版本钉在超仓的 `versions.lock` 里。

## 贡献与安全

开发与提交规则见
[`CONTRIBUTING.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/CONTRIBUTING.md)；
漏洞请按
[`SECURITY.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/SECURITY.md) 私下报告，
不要开公开 issue。

## 许可证

[AGPL-3.0](./LICENSE)。名称与标志另有说明 —— 见 [`NOTICE`](./NOTICE)。
