FROM golang:1.25-alpine AS builder
WORKDIR /app
RUN go env -w GO111MODULE=on
RUN go env -w GOPROXY=https://goproxy.cn,direct
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" -o /nanorunner ./cmd/nanorunner

FROM alpine:3.19

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
