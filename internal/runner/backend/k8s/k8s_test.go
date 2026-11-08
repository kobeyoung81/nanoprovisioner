package k8s

import (
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
)

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"nanosec-agent-alice":   "nanosec-agent-alice",
		"nanosec-agent-Alice":   "nanosec-agent-alice",
		"nanosec-agent-a b_c.9": "nanosec-agent-a-b-c-9",
		"nanosec-agent-12345678901234567890123456789012345678901234567890123456789012345": "nanosec-agent-1234567890123456789012345678901234567890123456789",
		"":                  "nanosec-agent",
		"nanosec-agent-__-": "nanosec-agent",
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeploymentName(t *testing.T) {
	req := nanoprotocol.ContainerRequest{UserID: "Alice", BindingID: 7}
	want := "nanosec-agent-alice-7"
	if got := DeploymentName(req); got != want {
		t.Errorf("DeploymentName() = %q, want %q", got, want)
	}
}

// The container image sets no timezone, so the pod spec must carry TZ or the
// agent's dates, cron schedules and git timestamps run in UTC.
func TestAgentEnvCarriesTheTimezone(t *testing.T) {
	env := agentEnv("alice", "", "Asia/Shanghai")

	for _, entry := range env {
		if name, _ := entry["name"].(string); name == "TZ" {
			if value, _ := entry["value"].(string); value != "Asia/Shanghai" {
				t.Errorf("TZ = %q, want Asia/Shanghai", value)
			}
			return
		}
	}
	t.Fatalf("env = %v, want a TZ entry", env)
}

func TestAgentEnvWithoutTimezoneLeavesTZUnset(t *testing.T) {
	for _, entry := range agentEnv("alice", "", "") {
		if name, _ := entry["name"].(string); name == "TZ" {
			t.Errorf("unexpected %v entry: an empty timezone must not inject TZ", entry)
		}
	}
}
