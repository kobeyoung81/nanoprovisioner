package swarm

import "testing"

// The swarm backend assembles the container environment on its own, so the
// token contract is pinned here too: without it an agent cannot reach the bus.
func TestAgentEnvCarriesTheBusToken(t *testing.T) {
	env := agentEnv("alice", "s3cret", "", "http://nanoadmin:8080")

	if len(env) != 3 || env[0] != "AGENT_USERNAME=alice" || env[1] != "NANOSEC_BUS_INTERNAL_TOKEN=s3cret" || env[2] != "NANOSEC_ADMIN_URL=http://nanoadmin:8080" {
		t.Errorf("env = %v", env)
	}
}

func TestAgentEnvWithoutTokenStaysMinimal(t *testing.T) {
	env := agentEnv("alice", "", "", "http://nanoadmin:8080")

	// The admin URL is always injected: the agent reads its configuration from
	// there, so a container without it can never become healthy.
	if len(env) != 2 || env[0] != "AGENT_USERNAME=alice" || env[1] != "NANOSEC_ADMIN_URL=http://nanoadmin:8080" {
		t.Errorf("env = %v, want the identity plus the admin URL", env)
	}
}

// The container image sets no timezone, so the agent's dates, cron schedules
// and git timestamps run in UTC unless the runner injects TZ.
func TestAgentEnvCarriesTheTimezone(t *testing.T) {
	env := agentEnv("alice", "", "Asia/Shanghai", "http://nanoadmin:8080")

	if len(env) != 3 || env[0] != "AGENT_USERNAME=alice" || env[1] != "NANOSEC_ADMIN_URL=http://nanoadmin:8080" || env[2] != "TZ=Asia/Shanghai" {
		t.Errorf("env = %v, want the timezone after the identity", env)
	}
}

func TestAgentEnvWithoutTimezoneLeavesTZUnset(t *testing.T) {
	for _, entry := range agentEnv("alice", "", "", "http://nanoadmin:8080") {
		if len(entry) >= 3 && entry[:3] == "TZ=" {
			t.Errorf("unexpected entry %q: an empty timezone must not inject TZ", entry)
		}
	}
}
