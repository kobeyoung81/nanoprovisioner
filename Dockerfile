FROM kobeyoung81/base-go:1.27.4-alpine3.24-r3 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,id=nanosec-gomod,target=/root/go/pkg/mod \
    --mount=type=cache,id=nanosec-gocache,target=/root/.cache/go-build \
    go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
RUN --mount=type=cache,id=nanosec-gomod,target=/root/go/pkg/mod \
    --mount=type=cache,id=nanosec-gocache,target=/root/.cache/go-build \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" -o /nanorunner ./cmd/nanorunner

FROM kobeyoung81/base-runtime:alpine3.24.4-r3

# Build metadata. All three come from the tagged commit (the version tag, the
# commit sha, and that commit's time), so two builds of one tag describe the same
# moment. `created` must be RFC 3339; the build script passes the commit time.
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="nanoprovisioner" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://gitee.com/kobeyoung81/nanoprovisioner" \
      org.opencontainers.image.licenses="AGPL-3.0-only"

COPY --from=builder /nanorunner /usr/local/bin/nanorunner
# Custom seccomp profile for agent containers. Referenced by the runner via the
# seccompProfile config (a daemon-host path); the deploy step installs this file
# onto the host at /etc/docker/seccomp/nanosec.json.
COPY seccomp/ /seccomp/
ENTRYPOINT ["nanorunner"]
