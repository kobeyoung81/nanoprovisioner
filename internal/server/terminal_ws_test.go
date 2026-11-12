package server

// The terminal bridge needs two things before it upgrades the browser: the agent
// must belong to the user, and the container must be connected to *this*
// replica. The second check answers 404 so the gateway can fall back to the bus
// while containers still dial it.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanorunner/internal/portalclient"
	"github.com/gorilla/websocket"
)

// fakePortal answers the agent list with one row.
func fakePortal(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"agents":[{"id":"agent-1"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTerminalBridge_RejectsAnotherUsersAgent(t *testing.T) {
	s := newTunnelTestServer(t)
	s.portal = portalclient.New(fakePortal(t).URL, "s3cret")
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal?token=s3cret&user_id=user-1&agent_id=someone-elses"
	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("the upgrade succeeded for an agent the user does not own")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %v, want 403", resp)
	}
}

// TestTerminalBridge_NoTunnelIs404 is the fallback signal: the gateway then
// retries against the bus, where containers with the previous image still
// connect.
func TestTerminalBridge_NoTunnelIs404(t *testing.T) {
	s := newTunnelTestServer(t)
	s.portal = portalclient.New(fakePortal(t).URL, "s3cret")
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal?token=s3cret&user_id=user-1&agent_id=agent-1"
	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("the upgrade succeeded without a tunnel")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %v, want 404", resp)
	}
}

// TestTerminalBridge_BridgesWhenTheTunnelExists pins the happy path: with a
// registered tunnel the browser connection is accepted and its bind frame is
// parked until the terminal answers (the answer itself is the agent's job).
func TestTerminalBridge_BridgesWhenTheTunnelExists(t *testing.T) {
	s := newTunnelTestServer(t)
	s.portal = portalclient.New(fakePortal(t).URL, "s3cret")
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	tunnel := dialTunnel(t, srv, "user-1")
	defer tunnel.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal?token=s3cret&user_id=user-1&agent_id=agent-1"
	browser, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial terminal: %v (status %v)", err, resp)
	}
	defer browser.Close()

	if err := browser.WriteJSON(terminalFrame{Type: "bind", TerminalID: "t1"}); err != nil {
		t.Fatalf("write bind: %v", err)
	}
	// The bind is recorded; output fan-out finds it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.terminalSubs.list("t1")) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the bind frame did not register a subscription")
}

// TestTerminalBridge_RequiresTheToken keeps the bridge behind the shared secret
// even though the gateway validated the JWT already.
func TestTerminalBridge_RequiresTheToken(t *testing.T) {
	s := newTunnelTestServer(t)
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal?user_id=user-1&agent_id=agent-1"
	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("the bridge accepted a connection without the internal token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", resp)
	}
}
