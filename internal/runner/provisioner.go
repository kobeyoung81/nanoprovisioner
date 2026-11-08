package runner

import (
	"context"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanorunner/internal/config"
)

// ProvisionOptions carries per-provision settings resolved from runner config.
type ProvisionOptions struct {
	Image  string
	BusURL string
	// BusToken is the shared token agent containers present to the bus. It is
	// part of the deployment (the runner holds it in its own environment), so
	// every container it provisions inherits it.
	BusToken       string
	Network        string
	Quota          config.Quota
	Storage        config.StorageConfig
	SeccompProfile string // daemon-host path to the custom seccomp profile (optional)
	// Timezone is the resolved IANA zone injected as TZ into the agent
	// container. Empty leaves TZ unset (UTC), which is what a container without
	// this setting gets.
	Timezone string
}

// Provisioner creates and destroys per-user agent containers. Each backend
// derives the resource identity deterministically from the request.
type Provisioner interface {
	Provision(ctx context.Context, req nanoprotocol.ContainerRequest, opts ProvisionOptions) error
	Deprovision(ctx context.Context, req nanoprotocol.ContainerRequest, opts ProvisionOptions) error
}
