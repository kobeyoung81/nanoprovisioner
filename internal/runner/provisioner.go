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
	// TunnelURL is where the container dials its hypervisor tunnel (this
	// service). Empty means the container falls back to BusURL.
	TunnelURL string
	AdminURL  string
	// BusToken is the shared token agent containers present to the bus. It is
	// part of the deployment (the runner holds it in its own environment), so
	// every container it provisions inherits it.
	BusToken string
	Network  string
	// NamePrefix prefixes both the container name and the data volume name so
	// several stacks can share one Docker daemon without touching each
	// other's resources. Empty keeps the historical names.
	NamePrefix     string
	Quota          config.Quota
	Storage        config.StorageConfig
	SeccompProfile string // daemon-host path to the custom seccomp profile (optional)
	// Timezone is the resolved IANA zone injected as TZ into the agent
	// container. Empty leaves TZ unset (UTC), which is what a container without
	// this setting gets.
	Timezone string
}

// TunnelURLOrBus is what containers are told to dial: this service when a tunnel
// URL is configured, the bus otherwise (an older deployment, or a rolling
// upgrade where the agent image is already new).
func TunnelURLOrBus(opts ProvisionOptions) string {
	if opts.TunnelURL != "" {
		return opts.TunnelURL
	}
	return opts.BusURL
}

// Provisioner creates and destroys per-user agent containers. Each backend
// derives the resource identity deterministically from the request.
type Provisioner interface {
	Provision(ctx context.Context, req nanoprotocol.ContainerRequest, opts ProvisionOptions) error
	Deprovision(ctx context.Context, req nanoprotocol.ContainerRequest, opts ProvisionOptions) error
}
