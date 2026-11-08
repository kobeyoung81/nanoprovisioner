package runner

import (
	"fmt"

	"gitee.com/kobeyoung81/nanoprotocol"
)

// AgentHostname is the container hostname given to every provisioned agent
// container. It is backend-independent so the agent always sees the same host
// name regardless of whether it runs on docker, swarm, or k8s.
const AgentHostname = "nanosec"

// reservedUsernames are system user names a sanitized display name must not
// shadow; those inputs fall back to the default username.
var reservedUsernames = map[string]bool{
	"root": true, "daemon": true, "bin": true, "sys": true, "sync": true,
	"games": true, "man": true, "lp": true, "mail": true, "news": true,
	"uucp": true, "proxy": true, "www-data": true, "backup": true, "list": true,
	"irc": true, "gnats": true, "nobody": true, "sshd": true, "messagebus": true,
	"dbus": true, "systemd-coredump": true, "systemd-network": true,
}

// UserName returns the Linux username the agent container should run as: the
// sanitized account display name (max 32 chars). Empty, invalid (e.g. CJK-only
// display names) or reserved results fall back to "nanosec".
func UserName(req nanoprotocol.ContainerRequest) string {
	name := nanoprotocol.SanitizeName(req.DisplayName, 32)
	if name == "" || reservedUsernames[name] {
		return "nanosec"
	}
	return name
}

// DataVolumeName returns the deterministic persistent data volume name for a
// user's agent. The same volume is used across docker named volumes, swarm
// volumes, and (sanitized) k8s PVCs so an agent's data survives container
// recreation regardless of backend.
func DataVolumeName(req nanoprotocol.ContainerRequest) string {
	if req.BindingID > 0 {
		return fmt.Sprintf("nanosec-data-%s-%d", req.UserID, req.BindingID)
	}
	return fmt.Sprintf("nanosec-data-%s", req.UserID)
}