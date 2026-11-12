package docker

import (
	"context"
	"strings"
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanorunner/internal/config"
	"gitee.com/kobeyoung81/nanorunner/internal/runner"
)

func TestProvisionConfig_Hardening(t *testing.T) {
	req := nanoprotocol.ContainerRequest{UserID: "alice"}
	opts := runner.ProvisionOptions{
		Image:  "nanosec:latest",
		BusURL: "ws://bus:9000",
		Quota:  config.Quota{MemoryMB: 512, CPUs: 1e9},
	}

	spec := provisionConfig(req, opts, nil, runner.UserName(req))
	if spec.hostConfig.ReadonlyRootfs {
		t.Error("rootfs must stay writable so apk/pip installs work")
	}
	if spec.hostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Errorf("unexpected restart policy %q", spec.hostConfig.RestartPolicy.Name)
	}

	security := strings.Join(spec.hostConfig.SecurityOpt, ",")
	if strings.Contains(security, "no-new-privileges") {
		t.Error("no-new-privileges must NOT be set — the agent user needs sudo (NOPASSWD) to install tools")
	}
	if strings.Contains(security, "seccomp=") {
		t.Error("expected no seccomp opt when no profile configured")
	}

	if len(spec.config.Cmd) == 0 || spec.config.Cmd[0] != "hypervisor" {
		t.Errorf("expected hypervisor command, got %v", spec.config.Cmd)
	}
	if spec.config.Image != "nanosec:latest" {
		t.Errorf("unexpected image %q", spec.config.Image)
	}
	env := strings.Join(spec.config.Env, ",")
	if !strings.Contains(env, "AGENT_USERNAME=") {
		t.Errorf("expected AGENT_USERNAME env, got %v", spec.config.Env)
	}
	if spec.config.Hostname != runner.AgentHostname {
		t.Errorf("unexpected hostname %q", spec.config.Hostname)
	}
}

func TestProvisionConfig_ReapingAndLogBounds(t *testing.T) {
	req := nanoprotocol.ContainerRequest{UserID: "alice"}
	opts := runner.ProvisionOptions{
		Image:  "nanosec:latest",
		BusURL: "ws://bus:9000",
		Quota:  config.Quota{MemoryMB: 512, CPUs: 1e9},
	}

	spec := provisionConfig(req, opts, nil, runner.UserName(req))

	// PID 1 (the nanosec hypervisor) does not reap, so orphans reparented to
	// it would pile up as zombies unless Docker injects an init.
	if spec.hostConfig.Init == nil || !*spec.hostConfig.Init {
		t.Errorf("expected HostConfig.Init = true, got %v", spec.hostConfig.Init)
	}

	// An unbounded json-file log can fill the host disk.
	lc := spec.hostConfig.LogConfig
	if lc.Type != "json-file" {
		t.Errorf("log driver = %q, want json-file", lc.Type)
	}
	if lc.Config["max-size"] != agentLogMaxSize || lc.Config["max-file"] != agentLogMaxFile {
		t.Errorf("log opts = %v, want max-size=%s max-file=%s",
			lc.Config, agentLogMaxSize, agentLogMaxFile)
	}
}

func TestProvisionConfig_PidsLimit(t *testing.T) {
	req := nanoprotocol.ContainerRequest{UserID: "alice"}
	opts := runner.ProvisionOptions{
		Image:  "nanosec:latest",
		BusURL: "ws://bus:9000",
		Quota:  config.Quota{MemoryMB: 512, CPUs: 1e9, Pids: 256},
	}

	spec := provisionConfig(req, opts, nil, runner.UserName(req))
	if spec.hostConfig.PidsLimit == nil {
		t.Fatal("expected a PidsLimit for a positive cap, got nil (unlimited)")
	}
	if *spec.hostConfig.PidsLimit != 256 {
		t.Errorf("PidsLimit = %d, want 256", *spec.hostConfig.PidsLimit)
	}

	// A non-positive cap means "leave Docker's default" (unlimited).
	opts.Quota.Pids = 0
	spec = provisionConfig(req, opts, nil, runner.UserName(req))
	if spec.hostConfig.PidsLimit != nil {
		t.Errorf("PidsLimit = %d, want nil for an unset cap", *spec.hostConfig.PidsLimit)
	}
}

func TestProvisionConfig_MemoryHasNoSwapHeadroom(t *testing.T) {
	req := nanoprotocol.ContainerRequest{UserID: "alice"}
	opts := runner.ProvisionOptions{
		Image:  "nanosec:latest",
		BusURL: "ws://bus:9000",
		Quota:  config.Quota{MemoryMB: 512, CPUs: 1e9},
	}

	spec := provisionConfig(req, opts, nil, runner.UserName(req))
	r := spec.hostConfig.Resources
	if r.Memory != 512*1024*1024 {
		t.Errorf("Memory = %d, want %d", r.Memory, 512*1024*1024)
	}
	// Docker reads MemorySwap as memory+swap, so leaving it unset would allow
	// up to twice Memory. Equal to Memory is "no swap": the cap cannot be
	// escaped by swapping.
	if r.MemorySwap != r.Memory {
		t.Errorf("MemorySwap = %d, want it equal to Memory (%d) so swap cannot exceed the cap",
			r.MemorySwap, r.Memory)
	}
}

func TestProvisionConfig_CapabilitiesAreExplicit(t *testing.T) {
	req := nanoprotocol.ContainerRequest{UserID: "alice"}
	opts := runner.ProvisionOptions{
		Image:  "nanosec:latest",
		BusURL: "ws://bus:9000",
		Quota:  config.Quota{MemoryMB: 512, CPUs: 1e9},
	}

	hc := provisionConfig(req, opts, nil, runner.UserName(req)).hostConfig

	// Drop everything, then add back exactly what the entrypoint and the
	// agent's own `sudo apt-get install` need.
	if len(hc.CapDrop) != 1 || hc.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop = %v, want [ALL]", hc.CapDrop)
	}

	want := []string{
		"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "SETGID", "SETUID",
		"SETPCAP", "NET_BIND_SERVICE", "NET_RAW", "SYS_CHROOT", "MKNOD",
		"AUDIT_WRITE", "SETFCAP",
	}
	got := make(map[string]bool, len(hc.CapAdd))
	for _, c := range hc.CapAdd {
		got[c] = true
	}
	if len(got) != len(want) {
		t.Fatalf("CapAdd = %v, want %d caps", hc.CapAdd, len(want))
	}
	for _, c := range want {
		if !got[c] {
			t.Errorf("CapAdd is missing %s", c)
		}
	}
}

// TestWarnIfOverCommitted_NoQuotaIsNoOp pins the early return: with no memory
// quota there is nothing to compare, and the check must not touch the Docker
// client (a nil client here would panic).
func TestWarnIfOverCommitted_NoQuotaIsNoOp(t *testing.T) {
	b := &Backend{}
	b.warnIfOverCommitted(context.Background(), config.Quota{})
}

func TestProvisionConfig_SeccompProfile(t *testing.T) {
	req := nanoprotocol.ContainerRequest{UserID: "alice"}
	opts := runner.ProvisionOptions{
		Image:          "nanosec:latest",
		BusURL:         "ws://bus:9000",
		SeccompProfile: "/etc/docker/seccomp/nanosec.json",
	}

	spec := provisionConfig(req, opts, nil, runner.UserName(req))
	security := strings.Join(spec.hostConfig.SecurityOpt, ",")
	if !strings.Contains(security, "seccomp=/etc/docker/seccomp/nanosec.json") {
		t.Errorf("expected seccomp opt for configured profile, got %q", security)
	}

	// No profile configured: no seccomp opt (Docker default applies).
	opts.SeccompProfile = ""
	spec = provisionConfig(req, opts, nil, runner.UserName(req))
	security = strings.Join(spec.hostConfig.SecurityOpt, ",")
	if strings.Contains(security, "seccomp=") {
		t.Errorf("expected no seccomp opt when unconfigured, got %q", security)
	}
}

// An agent container cannot reach the bus without the shared token, so every
// container the runner provisions inherits it from the runner's own environment.
func TestProvisionConfig_CarriesTheBusToken(t *testing.T) {
	req := nanoprotocol.ContainerRequest{UserID: "alice"}
	opts := runner.ProvisionOptions{
		Image:    "nanosec:latest",
		BusURL:   "ws://bus:9000",
		BusToken: "s3cret",
		Quota:    config.Quota{MemoryMB: 512, CPUs: 1e9},
	}

	spec := provisionConfig(req, opts, nil, runner.UserName(req))

	var env []string
	env = append(env, spec.config.Env...)
	if !contains(env, "AGENT_USERNAME="+runner.UserName(req)) {
		t.Errorf("env lost the agent identity: %v", env)
	}
	if !contains(env, "NANOSEC_BUS_INTERNAL_TOKEN=s3cret") {
		t.Errorf("env lost the bus token: %v", env)
	}
}

// No token configured means nothing to hand down; an empty variable would only
// make the container look configured.
func TestProvisionConfig_NoTokenNoEnvEntry(t *testing.T) {
	req := nanoprotocol.ContainerRequest{UserID: "alice"}
	opts := runner.ProvisionOptions{Image: "nanosec:latest", BusURL: "ws://bus:9000"}

	spec := provisionConfig(req, opts, nil, runner.UserName(req))

	for _, v := range spec.config.Env {
		if strings.HasPrefix(v, "NANOSEC_BUS_INTERNAL_TOKEN=") {
			t.Errorf("unexpected token variable %q", v)
		}
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// The nanosec image sets no timezone, so without TZ the agent's dates, cron
// schedules and git timestamps all run in UTC.
func TestProvisionConfig_CarriesTheAgentTimezone(t *testing.T) {
	req := nanoprotocol.ContainerRequest{UserID: "alice"}
	opts := runner.ProvisionOptions{
		Image:    "nanosec:latest",
		BusURL:   "ws://bus:9000",
		Timezone: "Asia/Shanghai",
		Quota:    config.Quota{MemoryMB: 512, CPUs: 1e9},
	}

	spec := provisionConfig(req, opts, nil, runner.UserName(req))
	if env := strings.Join(spec.config.Env, ","); !strings.Contains(env, "TZ=Asia/Shanghai") {
		t.Errorf("env = %v, want TZ=Asia/Shanghai", spec.config.Env)
	}

	// An empty timezone is the explicit "no TZ" setting: the container then
	// falls back to UTC, which is what an unset image gives it.
	opts.Timezone = ""
	spec = provisionConfig(req, opts, nil, runner.UserName(req))
	for _, entry := range spec.config.Env {
		if strings.HasPrefix(entry, "TZ=") {
			t.Errorf("unexpected entry %q: an empty timezone must not inject TZ", entry)
		}
	}
}

// TestProvisionConfig_PointsTheTunnelAtTheProvisioner pins the rollout wiring:
// containers are told to dial the provisioner, which owns tunnels now. Without
// the flag every container keeps dialing the bus, and a second bus replica still
// cannot reach it.
func TestProvisionConfig_PointsTheTunnelAtTheProvisioner(t *testing.T) {
	req := nanoprotocol.ContainerRequest{UserID: "alice"}
	opts := runner.ProvisionOptions{
		Image:     "nanosec:latest",
		BusURL:    "ws://bus:9000",
		TunnelURL: "ws://runner:8080",
		Quota:     config.Quota{MemoryMB: 512, CPUs: 1e9},
	}

	cmd := strings.Join(provisionConfig(req, opts, nil, runner.UserName(req)).config.Cmd, " ")
	if !strings.Contains(cmd, "--tunnel-url ws://runner:8080") {
		t.Fatalf("the container is not told where to dial the tunnel: %q", cmd)
	}
}

// TestProvisionConfig_FallsBackToTheBusWithoutATunnelURL keeps a deployment that
// has not set runner.tunnelUrl working exactly as before.
func TestProvisionConfig_FallsBackToTheBusWithoutATunnelURL(t *testing.T) {
	req := nanoprotocol.ContainerRequest{UserID: "alice"}
	opts := runner.ProvisionOptions{
		Image:  "nanosec:latest",
		BusURL: "ws://bus:9000",
		Quota:  config.Quota{MemoryMB: 512, CPUs: 1e9},
	}

	cmd := strings.Join(provisionConfig(req, opts, nil, runner.UserName(req)).config.Cmd, " ")
	if !strings.Contains(cmd, "--tunnel-url ws://bus:9000") {
		t.Fatalf("the tunnel URL did not fall back to the bus: %q", cmd)
	}
}
