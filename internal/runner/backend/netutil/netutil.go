// Package netutil resolves the Docker network that agent containers should be
// attached to. It avoids hardcoding the Compose project name by falling back to
// the network the runner itself is attached to when the configured network is
// missing.
package netutil

import (
	"context"
	"fmt"
	"os"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

// ResolveNetwork returns the network name to attach provisioned containers to.
//
// If configured is non-empty and exists on the daemon it is returned as-is.
// Otherwise the runner's own container is inspected and the first non-default
// network it is attached to is returned. It returns "" (with a nil error) when
// no suitable network can be determined.
func ResolveNetwork(ctx context.Context, cli *client.Client, configured string) (string, error) {
	if configured != "" {
		if _, err := cli.NetworkInspect(ctx, configured, types.NetworkInspectOptions{}); err == nil {
			return configured, nil
		}
	}

	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("resolve runner hostname: %w", err)
	}

	info, err := cli.ContainerInspect(ctx, hostname)
	if err != nil {
		return "", fmt.Errorf("inspect runner container: %w", err)
	}

	names := make([]string, 0, len(info.NetworkSettings.Networks))
	for name := range info.NetworkSettings.Networks {
		names = append(names, name)
	}
	return preferredNetwork(names), nil
}

// preferredNetwork returns the first network name that is not one of Docker's
// built-in networks, or "" if none.
func preferredNetwork(names []string) string {
	for _, name := range names {
		switch name {
		case "bridge", "host", "none":
			continue
		default:
			return name
		}
	}
	return ""
}
