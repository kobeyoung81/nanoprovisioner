package server

// The four capacity rules around the tunnel: a bounded in-flight queue, a
// five-second memory of who holds a tunnel elsewhere, a rolling drain, and a body
// size beyond which the JSON tunnel stops being the right carrier.
//
// Each of them is a "the platform stays reachable" rule rather than a feature, so
// they are pinned here: without the bound a single slow container can pile up
// requests, without the cache every forward costs a database read, without the
// drain a rolling restart drops live containers, and without the cut-off a large
// upload is base64'd twice and held in memory at both ends.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanorunner/internal/store"
)

func TestAddPendingRefusesPastTheLimit(t *testing.T) {
	c := newHypervisorConn(nil)
	for i := 0; i < maxPendingTunnelRequests; i++ {
		if _, err := c.addPending(strings.Repeat("x", i+1)); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if _, err := c.addPending("one-too-many"); err == nil {
		t.Fatal("the 33rd in-flight request was accepted")
	} else if err != errTunnelBusy {
		t.Fatalf("err = %v, want errTunnelBusy", err)
	}
}

// TestTunnelForwardBusyIs429: the bound has to reach the caller as a retryable
// answer, not as a timeout a minute later.
func TestTunnelForwardBusyIs429(t *testing.T) {
	s := newTunnelTestServer(t)
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	conn := dialTunnel(t, srv, "user-1")
	defer func() { _ = conn.Close() }()
	hc, ok := s.tunnels.lookup("hypervisor-user-1")
	if !ok {
		t.Fatal("the tunnel did not register")
	}
	for i := 0; i < maxPendingTunnelRequests; i++ {
		if _, err := hc.addPending("busy-" + strings.Repeat("x", i)); err != nil {
			t.Fatalf("prefill %d: %v", i, err)
		}
	}

	resp := postTunnel(t, srv, "hypervisor-user-1", `{"method":"GET","path":"/x"}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
}

func TestTunnelBodyOverTheLimitIs413(t *testing.T) {
	s := newTunnelTestServer(t)
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	// The size check runs before the lookup, so no container is needed: an
	// oversize body is refused whatever the tunnel's state. The body is a byte
	// slice on the wire (base64 in JSON), which is exactly why the limit exists.
	body, err := json.Marshal(nanoprotocol.TunnelRequest{
		Method: "POST", Path: "/upload",
		Body: make([]byte, maxTunnelInlineBody+1),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp := postTunnel(t, srv, "hypervisor-user-1", string(body))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

// TestTunnelHolderCacheIsShortLived: a cached holder is trusted for five seconds
// at most, and a registration or a disconnect drops it at once.
func TestTunnelHolderCacheIsShortLived(t *testing.T) {
	cache := newTunnelHolderCache(5 * time.Second)
	now := time.Now()
	cache.now = func() time.Time { return now }

	holder := &store.Tunnel{Name: "hypervisor-user-1", RunnerID: "runner-2"}
	cache.put("hypervisor-user-1", holder)
	if got, ok := cache.get("hypervisor-user-1"); !ok || got != holder {
		t.Fatalf("get = %+v (%v), want the cached holder", got, ok)
	}

	// "No replica holds it" is a real answer and is cached too, so an offline
	// container does not cause a database read per attempt.
	cache.put("hypervisor-user-2", nil)
	if got, ok := cache.get("hypervisor-user-2"); !ok || got != nil {
		t.Fatalf("negative answer = %+v (%v), want cached nil", got, ok)
	}

	now = now.Add(6 * time.Second)
	if _, ok := cache.get("hypervisor-user-1"); ok {
		t.Fatal("an entry outlived its five seconds")
	}

	cache.put("hypervisor-user-3", holder)
	cache.invalidate("hypervisor-user-3")
	if _, ok := cache.get("hypervisor-user-3"); ok {
		t.Fatal("invalidate left the entry behind")
	}
}

// TestDrainRefusesNewTunnelsAndClosesTheLiveOnes: after drain, the socket is
// refused at the HTTP level (503 before the upgrade), and the connection that was
// already up is closed at the deadline rather than kept forever.
func TestDrainRefusesNewTunnelsAndClosesTheLiveOnes(t *testing.T) {
	s := newTunnelTestServer(t)
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	conn := dialTunnel(t, srv, "user-1")
	live, until := s.StartDrain(80 * time.Millisecond)
	if live != 1 {
		t.Fatalf("live = %d, want 1", live)
	}
	if until.IsZero() {
		t.Fatal("the drain reported no deadline")
	}

	// A new container is refused before the handshake.
	if _, _, err := dialRaw(t, srv, "user-2"); err == nil {
		t.Fatal("a new tunnel was accepted while draining")
	}

	// The live one keeps working until the deadline...
	var msg nanoprotocol.Message
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := conn.ReadJSON(&msg); err == nil {
		// A ping frame is not a JSON message; ignore anything that decoded.
		_ = msg
	}

	// ...and is closed once it passes.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		if err := conn.ReadJSON(&msg); err != nil {
			break
		}
	}
	if s.tunnels.size() != 0 {
		t.Fatalf("%d tunnels survived the deadline", s.tunnels.size())
	}
	if err := s.acceptingTunnels(); err != nil {
		t.Fatalf("the drain stayed in effect: %v", err)
	}
}

// TestDrainEndpointReportsTheLiveConnections pins the control surface a rolling
// restart calls.
func TestDrainEndpointReportsTheLiveConnections(t *testing.T) {
	s := newTunnelTestServer(t)
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	conn := dialTunnel(t, srv, "user-1")
	defer func() { _ = conn.Close() }()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/internal/tunnels/drain",
		strings.NewReader(`{"deadline_seconds":1}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Draining    bool   `json:"draining"`
		Connections int    `json:"connections"`
		Deadline    string `json:"deadline"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Draining || body.Connections != 1 || body.Deadline == "" {
		t.Fatalf("body = %+v", body)
	}
}
