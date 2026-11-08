package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanorunner/internal/config"
)

// fakeBus answers every rpc with the given runner config items; that is all
// agentTimezone ever asks for.
func fakeBus(t *testing.T, items *[]nanoprotocol.ConfigItem) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := nanoprotocol.NewMessage(nanoprotocol.TypeConfigItems, nanoprotocol.GetConfigItemsResponse{
			ComponentType: "runner",
			Items:         *items,
		})
		if err != nil {
			t.Errorf("build response: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The timezone is re-read before every provisioning, so a config change must
// reach the next container without restarting the runner.
func TestAgentTimezoneReReadsTheBus(t *testing.T) {
	items := []nanoprotocol.ConfigItem{{Key: "runner.agentTimezone", Value: "Asia/Tokyo"}}
	srv := fakeBus(t, &items)
	client, err := nanoprotocol.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	r := New(&config.RunnerConfig{AgentTimezone: "Europe/Berlin"}, client, nil)

	if got := r.agentTimezone(); got != "Asia/Tokyo" {
		t.Errorf("agentTimezone() = %q, want the freshly read Asia/Tokyo", got)
	}

	items = []nanoprotocol.ConfigItem{{Key: "runner.agentTimezone", Value: "America/New_York"}}
	if got := r.agentTimezone(); got != "America/New_York" {
		t.Errorf("agentTimezone() = %q, want the changed value America/New_York", got)
	}
}

// A failed read must never block provisioning: the runner falls back to the
// value it loaded at startup.
func TestAgentTimezoneFallsBackWhenTheBusIsUnreachable(t *testing.T) {
	items := []nanoprotocol.ConfigItem{}
	srv := fakeBus(t, &items)
	client, err := nanoprotocol.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	srv.Close()

	r := New(&config.RunnerConfig{AgentTimezone: "Europe/Berlin"}, client, nil)

	if got := r.agentTimezone(); got != "Europe/Berlin" {
		t.Errorf("agentTimezone() = %q, want the startup fallback Europe/Berlin", got)
	}
}

// recordingProvisioner captures what provisioning handed the backend.
type recordingProvisioner struct {
	options []ProvisionOptions
}

func (p *recordingProvisioner) Provision(_ context.Context, _ nanoprotocol.ContainerRequest, opts ProvisionOptions) error {
	p.options = append(p.options, opts)
	return nil
}

func (p *recordingProvisioner) Deprovision(context.Context, nanoprotocol.ContainerRequest, ProvisionOptions) error {
	return nil
}

// Provisioning itself (not just the lookup) must carry the freshly-read zone
// into the backend, or a config change would only land after a runner restart.
func TestProvisionContainerUsesTheFreshTimezone(t *testing.T) {
	items := []nanoprotocol.ConfigItem{{Key: "runner.agentTimezone", Value: "Asia/Tokyo"}}
	srv := fakeBus(t, &items)
	client, err := nanoprotocol.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	p := &recordingProvisioner{}
	r := New(&config.RunnerConfig{AgentImage: "nanosec:latest"}, client, p)

	r.provisionContainer(context.Background(), nanoprotocol.ContainerRequest{UserID: "alice", Level: "free"})

	if len(p.options) != 1 {
		t.Fatalf("provisioned %d containers, want 1", len(p.options))
	}
	if got := p.options[0].Timezone; got != "Asia/Tokyo" {
		t.Errorf("provisioned with timezone %q, want the freshly read Asia/Tokyo", got)
	}
}
