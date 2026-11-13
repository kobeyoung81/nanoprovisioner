package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
	// Embedded IANA database: the runtime image (alpine) ships no tzdata, so
	// without this every LoadLocation below would reject even valid zones.
	_ "time/tzdata"

	"gitee.com/kobeyoung81/nanoprotocol"
)

const (
	DefaultBusURL = "ws://nanobus:9000"
	// DefaultTunnelURL is this service's own tunnel endpoint, as seen from an
	// agent container on the compose network. Containers dial it instead of the
	// bus so a tunnel survives losing the bus replica that first served it.
	DefaultTunnelURL = "ws://nanorunner:8080"
)

// ResolveBusURL returns the bus URL from the flag value, the
// NANOSEC_RUNNER_BUS_URL environment variable, or DefaultBusURL.
func ResolveBusURL(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("NANOSEC_RUNNER_BUS_URL"); env != "" {
		return env
	}
	return DefaultBusURL
}

// InternalToken returns the shared token the bus requires on its internal
// endpoints. It comes from the environment because the runner needs it before
// its first bus call, and it is handed to every agent container it provisions.
func InternalToken() string {
	return strings.TrimSpace(os.Getenv("NANOSEC_BUS_INTERNAL_TOKEN"))
}

// LoadRunnerConfig fetches the runner configuration from the admin module and
// reconstructs a RunnerConfig from the flat key-value pairs.
//
// It retries briefly: a runner started a moment before admin still comes up
// configured, and after ~30s the failure is reported instead of silently
// provisioning with defaults.
func LoadRunnerConfig(adminURL string) (*RunnerConfig, error) {
	items, err := fetchWithRetry(adminURL, "runner")
	if err != nil {
		return nil, fmt.Errorf("get runner config items: %w", err)
	}
	cfg := applyRunnerConfig(configItemsToMap(items))
	applyEnvOverrides(&cfg)
	if cfg.TunnelURL == "" {
		cfg.TunnelURL = DefaultTunnelURL
	}
	return &cfg, nil
}

// fetchWithRetry retries 1s, 2s, 4s, 8s, 8s, 8s — about 31 seconds.
func fetchWithRetry(adminURL, component string) ([]nanoprotocol.ConfigItem, error) {
	delays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	var lastErr error
	for _, delay := range append([]time.Duration{0}, delays...) {
		if delay > 0 {
			time.Sleep(delay)
		}
		items, err := fetchItemsFrom(adminURL, component, 5*time.Second)
		if err == nil {
			return items, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// applyEnvOverrides lets deployment-specific values (which vary with the
// Docker Compose project name, image registry, etc.) override bus config items.
func applyEnvOverrides(cfg *RunnerConfig) {
	if cfg.AdminURL == "" {
		cfg.AdminURL = DefaultAdminURL
	}
	if v := os.Getenv("NANOSEC_ADMIN_URL"); v != "" {
		cfg.AdminURL = v
	}
	if v := os.Getenv("NANOSEC_RUNNER_NETWORK"); v != "" {
		cfg.Network = v
	}
	if v := os.Getenv("NANOSEC_RUNNER_AGENT_IMAGE"); v != "" {
		cfg.AgentImage = v
	}
	if v := os.Getenv("NANOSEC_RUNNER_BACKEND"); v != "" {
		cfg.Backend = v
	}
	if v := os.Getenv("NANOSEC_RUNNER_TUNNEL_URL"); v != "" {
		cfg.TunnelURL = v
	}
	if v := os.Getenv("NANOSEC_RUNNER_STORAGE_BACKEND"); v != "" {
		cfg.Storage.Backend = v
	}
	if v := os.Getenv("NANOSEC_RUNNER_STORAGE_DRIVER"); v != "" {
		cfg.Storage.Driver = v
	}
	if v := os.Getenv("NANOSEC_RUNNER_STORAGE_CLASS"); v != "" {
		cfg.Storage.StorageClass = v
	}
	if v := os.Getenv("NANOSEC_RUNNER_STORAGE_ACCESS_MODE"); v != "" {
		cfg.Storage.AccessMode = v
	}
	if v := os.Getenv("NANOSEC_RUNNER_STORAGE_MOUNT_PATH"); v != "" {
		cfg.Storage.MountPath = v
	}
	if v := os.Getenv("NANOSEC_RUNNER_STORAGE_BIND_PATH"); v != "" {
		cfg.Storage.BindPath = v
	}
	if v := os.Getenv("NANOSEC_RUNNER_AGENT_TIMEZONE"); v != "" {
		cfg.AgentTimezone = NormalizeTimezone(v)
	}
}

func configItemsToMap(items []nanoprotocol.ConfigItem) map[string]string {
	m := make(map[string]string, len(items))
	for _, item := range items {
		m[item.Key] = item.Value
	}
	return m
}

func applyRunnerConfig(m map[string]string) RunnerConfig {
	cfg := RunnerConfig{
		Backend: "docker",
		Storage: DefaultStorageConfig(),
		Quotas:  DefaultQuotaConfig(),
	}
	if v := m["runner.pollInterval"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Runner.PollInterval = n
		}
	}
	cfg.AgentImage = m["agentImage"]
	cfg.Network = m["network"]
	cfg.NamePrefix = m["namePrefix"]
	cfg.SeccompProfile = m["seccompProfile"]
	// An absent key means the bus has not seeded it yet: fall back to the
	// built-in default rather than leaving agents in UTC. A present-but-empty
	// value is the operator's explicit "no TZ".
	if v, ok := m["runner.agentTimezone"]; ok {
		cfg.AgentTimezone = NormalizeTimezone(v)
	} else {
		cfg.AgentTimezone = NormalizeTimezone(DefaultAgentTimezone)
	}
	if v := m["runner.backend"]; v != "" {
		cfg.Backend = v
	}
	if v := m["runner.tunnelUrl"]; v != "" {
		cfg.TunnelURL = v
	}
	applyRunnerStorage(&cfg.Storage, m)

	applyQuota(&cfg.Quotas.Free, "free", m)
	applyQuota(&cfg.Quotas.Pro, "pro", m)
	applyQuota(&cfg.Quotas.Ultra, "ultra", m)

	// RunnerID: honor an explicit config key, else auto-generate from hostname.
	if id := m["runner.runnerID"]; id != "" {
		cfg.Runner.RunnerID = id
	} else {
		hostname, err := os.Hostname()
		if err != nil {
			hostname = "runner-unknown"
		}
		cfg.Runner.RunnerID = hostname
	}

	return cfg
}

func applyQuota(q *Quota, tier string, m map[string]string) {
	if v := m["runner.quota."+tier+".memory"]; v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			q.MemoryMB = n
		}
	}
	if v := m["runner.quota."+tier+".cpu"]; v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			q.CPUs = n
		}
	}
	if v := m["runner.quota."+tier+".pids"]; v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			q.Pids = n
		}
	}
}

// applyRunnerStorage fills storage settings from flat config items, overriding
// the defaults already applied in applyRunnerConfig. Empty config values are
// left as-is so operator-provided defaults are preserved.
func applyRunnerStorage(st *StorageConfig, m map[string]string) {
	if v := m["runner.storage.backend"]; v != "" {
		st.Backend = v
	}
	if v := m["runner.storage.driver"]; v != "" {
		st.Driver = v
	}
	if v := m["runner.storage.driverOptsKey"]; v != "" {
		st.DriverOptsKey = v
	}
	if v := m["runner.storage.driverOptsValue"]; v != "" {
		st.DriverOptsValue = v
	}
	if v := m["runner.storage.storageClass"]; v != "" {
		st.StorageClass = v
	}
	if v := m["runner.storage.accessMode"]; v != "" {
		st.AccessMode = v
	}
	if v := m["runner.storage.mountPath"]; v != "" {
		st.MountPath = v
	}
	if v := m["runner.storage.bindPath"]; v != "" {
		st.BindPath = v
	}
}

// NormalizeTimezone validates an IANA zone name and returns it trimmed, or ""
// when the value is empty or cannot be resolved. The agent container resolves
// the same name with the same rules, so a value Go rejects here would leave the
// container in UTC anyway — better to say so at the runner than to inject a
// name that silently does nothing. "" means "do not set TZ".
func NormalizeTimezone(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if _, err := time.LoadLocation(v); err != nil {
		slog.Warn("ignoring invalid agent timezone", "value", v, "error", err)
		return ""
	}
	return v
}

// TimezoneFromItems picks the agent timezone out of freshly-read runner config
// items, falling back to the value loaded at startup when the key is absent
// (i.e. it has not been seeded yet). A present-but-empty value means "no TZ".
func TimezoneFromItems(items []nanoprotocol.ConfigItem, fallback string) string {
	for _, item := range items {
		if item.Key == "runner.agentTimezone" {
			return NormalizeTimezone(item.Value)
		}
	}
	return NormalizeTimezone(fallback)
}

// ValidateRunnerConfig checks that required runner configuration fields are set.
func ValidateRunnerConfig(cfg *RunnerConfig) error {
	if cfg.AgentImage == "" {
		return fmt.Errorf("agent image is required")
	}
	if cfg.Runner.PollInterval <= 0 {
		return fmt.Errorf("poll interval must be positive")
	}
	return nil
}
