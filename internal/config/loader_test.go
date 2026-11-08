package config

import (
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
)

func TestApplyRunnerConfigDefaults(t *testing.T) {
	cfg := applyRunnerConfig(map[string]string{})
	if cfg.Backend != "docker" {
		t.Errorf("Backend = %q, want docker", cfg.Backend)
	}
	if cfg.AgentImage != "" {
		t.Errorf("AgentImage = %q, want empty", cfg.AgentImage)
	}
	free := cfg.Quotas.Free
	if free.MemoryMB != 512 || free.CPUs != 1e9 {
		t.Errorf("free quota = %+v, want {512 1e9}", free)
	}
	if got := cfg.Quotas.QuotaForLevel("pro"); got.MemoryMB != 2048 || got.CPUs != 2e9 {
		t.Errorf("pro quota = %+v, want {2048 2e9}", got)
	}
	if cfg.Runner.RunnerID == "" {
		t.Error("RunnerID should default to hostname")
	}
}

// TestApplyRunnerConfigPids covers the per-tier PID cap: a default per tier,
// a flat config override, and an untouched tier keeping its default.
func TestApplyRunnerConfigPids(t *testing.T) {
	cfg := applyRunnerConfig(map[string]string{})
	if cfg.Quotas.Free.Pids != 256 || cfg.Quotas.Pro.Pids != 512 || cfg.Quotas.Ultra.Pids != 1024 {
		t.Errorf("default pids = %d/%d/%d, want 256/512/1024",
			cfg.Quotas.Free.Pids, cfg.Quotas.Pro.Pids, cfg.Quotas.Ultra.Pids)
	}

	cfg = applyRunnerConfig(map[string]string{"runner.quota.ultra.pids": "2048"})
	if got := cfg.Quotas.QuotaForLevel("ultra").Pids; got != 2048 {
		t.Errorf("ultra pids = %d, want 2048", got)
	}
	if got := cfg.Quotas.QuotaForLevel("free").Pids; got != 256 {
		t.Errorf("free pids = %d, want the default 256", got)
	}
}

func TestApplyRunnerConfigOverrides(t *testing.T) {
	m := map[string]string{
		"runner.backend":            "k8s",
		"runner.pollInterval":       "10",
		"runner.runnerID":           "runner-01",
		"agentImage":                "registry.example.com/nanosec:v1",
		"network":                   "overlay-net",
		"runner.quota.pro.memory":   "4096",
		"runner.quota.ultra.memory": "8192",
		"runner.quota.ultra.cpu":    "8000000000",
	}
	cfg := applyRunnerConfig(m)
	if cfg.Backend != "k8s" {
		t.Errorf("Backend = %q, want k8s", cfg.Backend)
	}
	if cfg.Runner.PollInterval != 10 {
		t.Errorf("PollInterval = %d, want 10", cfg.Runner.PollInterval)
	}
	if cfg.Runner.RunnerID != "runner-01" {
		t.Errorf("RunnerID = %q, want runner-01", cfg.Runner.RunnerID)
	}
	if cfg.AgentImage != "registry.example.com/nanosec:v1" {
		t.Errorf("AgentImage = %q, want registry image", cfg.AgentImage)
	}
	if cfg.Network != "overlay-net" {
		t.Errorf("Network = %q, want overlay-net", cfg.Network)
	}
	if got := cfg.Quotas.QuotaForLevel("pro"); got.MemoryMB != 4096 {
		t.Errorf("pro memory = %d, want 4096", got.MemoryMB)
	}
	ultra := cfg.Quotas.QuotaForLevel("ultra")
	if ultra.MemoryMB != 8192 || ultra.CPUs != 8000000000 {
		t.Errorf("ultra quota = %+v, want {8192 8e9}", ultra)
	}
	// untouched tier keeps default
	if got := cfg.Quotas.QuotaForLevel("free"); got.MemoryMB != 512 {
		t.Errorf("free memory = %d, want default 512", got.MemoryMB)
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	t.Setenv("NANOSEC_RUNNER_NETWORK", "devmyriadmind_default")
	t.Setenv("NANOSEC_RUNNER_AGENT_IMAGE", "registry.example.com/nanosec:dev")

	cfg := applyRunnerConfig(map[string]string{
		"network":    "myriadmind_default",
		"agentImage": "kobeyoung81/nanosec:latest",
	})
	applyEnvOverrides(&cfg)

	if cfg.Network != "devmyriadmind_default" {
		t.Errorf("Network = %q, want env override devmyriadmind_default", cfg.Network)
	}
	if cfg.AgentImage != "registry.example.com/nanosec:dev" {
		t.Errorf("AgentImage = %q, want env override registry.example.com/nanosec:dev", cfg.AgentImage)
	}
}

func TestApplyEnvOverrides_EmptyEnvKeepsConfig(t *testing.T) {
	t.Setenv("NANOSEC_RUNNER_NETWORK", "")
	t.Setenv("NANOSEC_RUNNER_AGENT_IMAGE", "")

	cfg := applyRunnerConfig(map[string]string{
		"network":    "overlay-net",
		"agentImage": "kobeyoung81/nanosec:latest",
	})
	applyEnvOverrides(&cfg)

	if cfg.Network != "overlay-net" {
		t.Errorf("Network = %q, want config value overlay-net", cfg.Network)
	}
	if cfg.AgentImage != "kobeyoung81/nanosec:latest" {
		t.Errorf("AgentImage = %q, want config value", cfg.AgentImage)
	}
}

func TestApplyRunnerStorageConfig(t *testing.T) {
	m := map[string]string{
		"runner.storage.backend":         "volume",
		"runner.storage.driver":          "nfs",
		"runner.storage.driverOptsKey":   "type",
		"runner.storage.driverOptsValue": "nfs",
		"runner.storage.storageClass":    "fast-ssd",
		"runner.storage.mountPath":       "/data/.nanosec",
	}
	cfg := applyRunnerConfig(m)
	st := cfg.Storage
	if st.Backend != "volume" {
		t.Errorf("Storage.Backend = %q, want volume", st.Backend)
	}
	if st.Driver != "nfs" {
		t.Errorf("Storage.Driver = %q, want nfs", st.Driver)
	}
	if st.DriverOptsKey != "type" || st.DriverOptsValue != "nfs" {
		t.Errorf("DriverOpts = %q/%q, want type/nfs", st.DriverOptsKey, st.DriverOptsValue)
	}
	if st.StorageClass != "fast-ssd" {
		t.Errorf("Storage.StorageClass = %q, want fast-ssd", st.StorageClass)
	}
	if st.MountPath != "/data/.nanosec" {
		t.Errorf("Storage.MountPath = %q, want /data/.nanosec", st.MountPath)
	}
}

func TestStorageDataMountPathDefault(t *testing.T) {
	if got := (StorageConfig{}).DataMountPath("kobe"); got != "/home/kobe" {
		t.Errorf("DataMountPath() = %q, want /home/kobe", got)
	}
	if got := (StorageConfig{}).DataMountPath("nanosec"); got != "/home/nanosec" {
		t.Errorf("DataMountPath() = %q, want /home/nanosec", got)
	}
	if got := (StorageConfig{MountPath: "/custom"}).DataMountPath("kobe"); got != "/custom" {
		t.Errorf("DataMountPath() = %q, want /custom", got)
	}
}

func TestDefaultStorageConfig(t *testing.T) {
	d := DefaultStorageConfig()
	if d.Backend != "volume" {
		t.Errorf("default Backend = %q, want volume", d.Backend)
	}
	if d.Driver != "local" {
		t.Errorf("default Driver = %q, want local", d.Driver)
	}
	if d.DataMountPath("kobe") != "/home/kobe" {
		t.Errorf("default mount = %q, want /home/kobe", d.DataMountPath("kobe"))
	}
}

// The agent container image sets no timezone, so an unset config must still
// produce a usable zone; a present-but-empty value is the explicit "no TZ".
func TestApplyRunnerConfigTimezone(t *testing.T) {
	if got := applyRunnerConfig(map[string]string{}).AgentTimezone; got != DefaultAgentTimezone {
		t.Errorf("AgentTimezone = %q, want the default %q before the key is seeded", got, DefaultAgentTimezone)
	}

	cases := map[string]struct {
		value string
		want  string
	}{
		"valid zone":         {"Europe/Berlin", "Europe/Berlin"},
		"surrounding spaces": {"  Asia/Tokyo  ", "Asia/Tokyo"},
		"explicit none":      {"", ""},
		"unresolvable zone":  {"Mars/Olympus", ""},
		// A POSIX offset string is not an IANA name: Go cannot resolve it, so
		// injecting it would leave the agent in UTC without saying so.
		"posix offset string": {"CST-8", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := applyRunnerConfig(map[string]string{"runner.agentTimezone": tc.value}).AgentTimezone
			if got != tc.want {
				t.Errorf("agentTimezone %q -> %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// The per-provision re-read decides between the freshly fetched value and the
// value loaded at startup; an absent key means "not seeded yet".
func TestTimezoneFromItems(t *testing.T) {
	items := []nanoprotocol.ConfigItem{{Key: "agentImage", Value: "nanosec:latest"}}
	if got := TimezoneFromItems(items, "Europe/Berlin"); got != "Europe/Berlin" {
		t.Errorf("absent key -> %q, want the startup fallback Europe/Berlin", got)
	}

	items = append(items, nanoprotocol.ConfigItem{Key: "runner.agentTimezone", Value: "Asia/Tokyo"})
	if got := TimezoneFromItems(items, "Europe/Berlin"); got != "Asia/Tokyo" {
		t.Errorf("present key -> %q, want Asia/Tokyo (the fresh value wins)", got)
	}

	items[len(items)-1].Value = ""
	if got := TimezoneFromItems(items, "Europe/Berlin"); got != "" {
		t.Errorf("emptied key -> %q, want no timezone even though a fallback exists", got)
	}

	items[len(items)-1].Value = "Nowhere/Special"
	if got := TimezoneFromItems(items, "Europe/Berlin"); got != "" {
		t.Errorf("invalid key -> %q, want no timezone", got)
	}
}

func TestApplyEnvOverrides_AgentTimezone(t *testing.T) {
	t.Setenv("NANOSEC_RUNNER_AGENT_TIMEZONE", "Europe/Berlin")
	cfg := applyRunnerConfig(map[string]string{"runner.agentTimezone": "Asia/Shanghai"})
	applyEnvOverrides(&cfg)
	if cfg.AgentTimezone != "Europe/Berlin" {
		t.Errorf("AgentTimezone = %q, want the env override Europe/Berlin", cfg.AgentTimezone)
	}

	t.Setenv("NANOSEC_RUNNER_AGENT_TIMEZONE", "")
	cfg = applyRunnerConfig(map[string]string{"runner.agentTimezone": "Asia/Shanghai"})
	applyEnvOverrides(&cfg)
	if cfg.AgentTimezone != "Asia/Shanghai" {
		t.Errorf("AgentTimezone = %q, want the config value Asia/Shanghai", cfg.AgentTimezone)
	}
}
