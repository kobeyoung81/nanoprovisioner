package swarm

import "testing"

// The swarm backend assembles the container environment on its own, so the
// token contract is pinned here too: without it an agent cannot reach the bus.
func TestAgentEnvCarriesTheBusToken(t *testing.T) {
	env := agentEnv("alice", "s3cret", "")

	if len(env) != 2 || env[0] != "AGENT_USERNAME=alice" || env[1] != "NANOSEC_BUS_INTERNAL_TOKEN=s3cret" {
		t.Errorf("env = %v", env)
	}
}

func TestAgentEnvWithoutTokenStaysMinimal(t *testing.T) {
	env := agentEnv("alice", "", "")

	if len(env) != 1 || env[0] != "AGENT_USERNAME=alice" {
		t.Errorf("env = %v, want only the agent identity", env)
	}
}

// The container image sets no timezone, so the agent's dates, cron schedules
// and git timestamps run in UTC unless the runner injects TZ.
func TestAgentEnvCarriesTheTimezone(t *testing.T) {
	env := agentEnv("alice", "", "Asia/Shanghai")

	if len(env) != 2 || env[0] != "AGENT_USERNAME=alice" || env[1] != "TZ=Asia/Shanghai" {
		t.Errorf("env = %v, want the timezone after the identity", env)
	}
}

func TestAgentEnvWithoutTimezoneLeavesTZUnset(t *testing.T) {
	for _, entry := range agentEnv("alice", "", "") {
		if len(entry) >= 3 && entry[:3] == "TZ=" {
			t.Errorf("unexpected entry %q: an empty timezone must not inject TZ", entry)
		}
	}
}
