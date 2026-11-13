package config

// RunnerConfig holds all configuration for the nanorunner service.
type RunnerConfig struct {
	Runner     RunnerSettings `json:"runner"      mapstructure:"runner"`
	AgentImage string         `json:"agentImage"  mapstructure:"agentImage"`
	BusURL     string         `json:"busURL" mapstructure:"busURL"` // resolved bus WebSocket URL
	// TunnelURL is where agent containers dial the hypervisor tunnel. Tunnels
	// belong to this service, so containers point here; an empty value falls back
	// to BusURL, which is how a deployment that has not rolled over yet behaves.
	TunnelURL string `json:"tunnelUrl" mapstructure:"tunnelUrl"`
	// AdminURL is passed to every agent container: the agent reads its
	// configuration from the admin module, not from the bus.
	AdminURL string `json:"adminURL" mapstructure:"adminURL"`
	Network  string `json:"network"     mapstructure:"network"`
	// NamePrefix prefixes the agent container name and the data volume name
	// so several stacks can share one Docker daemon without collisions.
	NamePrefix string        `json:"namePrefix"  mapstructure:"namePrefix"`
	Backend    string        `json:"backend"     mapstructure:"backend"` // docker | swarm | k8s
	Storage    StorageConfig `json:"storage"     mapstructure:"storage"`
	Quotas     QuotaConfig   `json:"quotas"      mapstructure:"quotas"`
	// SeccompProfile is an optional daemon-host path to a custom seccomp
	// profile. Leave it empty to use Docker's built-in profile, which is
	// recommended: setting it replaces that profile rather than adding to it,
	// and the profile shipped in seccomp/ is weaker in places. See
	// seccomp/README.md.
	SeccompProfile string `json:"seccompProfile" mapstructure:"seccompProfile"`
	// AgentTimezone is the IANA zone injected as TZ into every agent container
	// the runner provisions (empty = leave TZ unset, i.e. UTC). The image itself
	// sets no timezone, so without this the agent's dates, cron schedules and
	// git timestamps run in UTC. The value is re-read from the bus before each
	// provisioning, so a config change needs no runner restart.
	AgentTimezone string `json:"agentTimezone" mapstructure:"agentTimezone"`
}

// DefaultAgentTimezone is applied when the runner config carries no timezone
// (including the window before the bus has seeded the key). It matches the
// seeded value in nanobus's defaultConfigItems, so both paths agree.
const DefaultAgentTimezone = "Asia/Shanghai"

// StorageConfig describes how per-user agent data is persisted. Each backend
// maps these settings onto its native persistent primitive (docker/swarm named
// volume or a k8s PVC), mounted at MountPath inside the agent container.
type StorageConfig struct {
	// Backend selects the storage mechanism: "volume" (default) or "bind".
	Backend string `json:"backend" mapstructure:"backend"`
	// Driver is the docker/swarm volume driver ("local" by default); set
	// driverOptsKey/Value for remote drivers such as NFS or a cloud block store.
	Driver string `json:"driver" mapstructure:"driver"`
	// DriverOptsKey/DriverOptsValue let operators configure the volume driver
	// options (e.g. "type" -> "nfs") for remote storage across docker/swarm.
	DriverOptsKey   string `json:"driverOptsKey"   mapstructure:"driverOptsKey"`
	DriverOptsValue string `json:"driverOptsValue" mapstructure:"driverOptsValue"`
	// StorageClass is the k8s storage class for the per-user PVC (empty = default class).
	StorageClass string `json:"storageClass" mapstructure:"storageClass"`
	// AccessMode is the PVC access mode (empty = ReadWriteOnce). ReadWriteMany
	// is only meaningful on a storage class that supports it; it lets an agent
	// pod move between nodes while the claim stays attached.
	AccessMode string `json:"accessMode" mapstructure:"accessMode"`
	// MountPath is where the volume is mounted inside the agent container.
	MountPath string `json:"mountPath" mapstructure:"mountPath"`
	// BindPath, used only when Backend=="bind", is a host path mounted read-write
	// (single-host/docker/compose only; empty disables bind mounts).
	BindPath string `json:"bindPath" mapstructure:"bindPath"`
}

// DataMountPath returns the mount path for the persistent data root. It
// matches the agent image's data root: /home/<username>/ where username is the
// sanitized Linux user for the container (the nanosec user home; the agent's
// "~" data root resolves there). An explicit MountPath override still wins.
func (s StorageConfig) DataMountPath(username string) string {
	if s.MountPath != "" {
		return s.MountPath
	}
	return "/home/" + username
}

// DefaultStorageConfig returns sensible defaults: volume-backed at the agent
// data root, using the local docker/swarm volume driver.
func DefaultStorageConfig() StorageConfig {
	return StorageConfig{
		Backend: "volume",
		Driver:  "local",
	}
}

// RunnerSettings holds runner-specific settings.
type RunnerSettings struct {
	PollInterval int    `json:"pollInterval" mapstructure:"pollInterval"` // seconds
	RunnerID     string `json:"runnerID"     mapstructure:"runnerID"`     // auto-generated from hostname
}

// Quota defines resource limits for an agent container.
type Quota struct {
	MemoryMB int64 `json:"memoryMB" mapstructure:"memoryMB"`
	CPUs     int64 `json:"cpus"     mapstructure:"cpus"` // nanoCPUs (1e9 = 1 CPU)
	// Pids caps the number of processes and threads in the container. Zero or
	// negative leaves Docker's default, which is unlimited — an agent that
	// forks without bound can then exhaust the host's PID table and take the
	// other containers down with it. Honoured by the Docker backend; the k8s
	// and swarm backends have their own resource models.
	Pids int64 `json:"pids" mapstructure:"pids"`
}

// QuotaConfig holds per-tier resource limits.
type QuotaConfig struct {
	Free  Quota `json:"free"  mapstructure:"free"`
	Pro   Quota `json:"pro"   mapstructure:"pro"`
	Ultra Quota `json:"ultra" mapstructure:"ultra"`
}

// QuotaForLevel returns the quota for a user tier.
func (q QuotaConfig) QuotaForLevel(level string) Quota {
	switch level {
	case "pro":
		return q.Pro
	case "ultra":
		return q.Ultra
	default: // "free"
		return q.Free
	}
}

// DefaultQuotaConfig returns the default per-tier quotas. Pids is a guard
// against a runaway fork inside one agent taking the whole host down; it is
// set well above what a normal agent session needs.
func DefaultQuotaConfig() QuotaConfig {
	return QuotaConfig{
		Free:  Quota{MemoryMB: 512, CPUs: 1e9, Pids: 256},
		Pro:   Quota{MemoryMB: 2048, CPUs: 2e9, Pids: 512},
		Ultra: Quota{MemoryMB: 4096, CPUs: 4e9, Pids: 1024},
	}
}
