FROM golang:1.25-alpine AS builder
WORKDIR /app
RUN go env -w GO111MODULE=on
RUN go env -w GOPROXY=https://goproxy.cn,direct
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /nanorunner ./cmd/nanorunner

FROM alpine:3.19
COPY --from=builder /nanorunner /usr/local/bin/nanorunner
# Custom seccomp profile for agent containers. Referenced by the runner via the
# seccompProfile config (a daemon-host path); the deploy step installs this file
# onto the host at /etc/docker/seccomp/nanosec.json.
COPY seccomp/ /seccomp/
ENTRYPOINT ["nanorunner"]
