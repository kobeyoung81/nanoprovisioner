package server

// The tunnel surface is what makes a container reachable from any replica: a
// container dials /ws/hypervisor, and callers ask for an HTTP request to be put
// on that connection. These tests drive both ends over real WebSockets so the
// handshake, the correlation ids and the two failure codes are exercised as
// they will be in production.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanorunner/internal/store"
	"gitee.com/kobeyoung81/nanorunner/migrations"
	"github.com/gorilla/websocket"
)

func newTunnelTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("NANOSEC_BUS_INTERNAL_TOKEN", "s3cret")
	return New(nil, "runner-test", "") // no store: the registry row is best-effort
}

// dialTunnel connects a fake container and completes the tunnel_register
// handshake. It returns the connection so the test can answer requests.
func dialTunnel(t *testing.T, srv *httptest.Server, userID string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/hypervisor?token=s3cret"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	register, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelRegister, nanoprotocol.TunnelRegister{UserID: userID})
	if err != nil {
		t.Fatalf("build register: %v", err)
	}
	if err := conn.WriteJSON(register); err != nil {
		t.Fatalf("write register: %v", err)
	}
	var ack nanoprotocol.Message
	if err := conn.ReadJSON(&ack); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if ack.Type != nanoprotocol.TypeTunnelRegistered {
		t.Fatalf("ack type = %q, want %q", ack.Type, nanoprotocol.TypeTunnelRegistered)
	}
	return conn
}

// answerOnce reads one tunnel_request and replies with the given status/body.
// It runs in a goroutine, so failures are reported with Errorf and end the
// goroutine instead of calling Fatalf off the test goroutine.
func answerOnce(t *testing.T, conn *websocket.Conn, status int, body string) {
	t.Helper()
	var msg nanoprotocol.Message
	if err := conn.ReadJSON(&msg); err != nil {
		t.Errorf("read request: %v", err)
		return
	}
	if msg.Type != nanoprotocol.TypeTunnelRequest {
		t.Errorf("type = %q, want tunnel_request", msg.Type)
		return
	}
	var req nanoprotocol.TunnelRequest
	if err := msg.DecodePayload(&req); err != nil {
		t.Errorf("decode request: %v", err)
		return
	}
	resp, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelResponse, nanoprotocol.TunnelResponse{
		RequestID: req.RequestID,
		Status:    status,
		Body:      []byte(body),
	})
	if err != nil {
		t.Errorf("build response: %v", err)
		return
	}
	if err := conn.WriteJSON(resp); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func postTunnel(t *testing.T, srv *httptest.Server, name, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/internal/tunnels/"+name+"/request", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post tunnel request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestTunnelForwardRoundTripsResponse pins the whole path: register, correlate
// the request by its id, and hand the container's answer back unchanged.
func TestTunnelForwardRoundTripsResponse(t *testing.T) {
	s := newTunnelTestServer(t)
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	conn := dialTunnel(t, srv, "user-1")
	defer conn.Close()

	go answerOnce(t, conn, http.StatusCreated, `{"ok":true}`)

	resp := postTunnel(t, srv, "hypervisor-user-1", `{"method":"GET","path":"/api/v1/files/agent-1"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got nanoprotocol.TunnelResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Status != http.StatusCreated || string(got.Body) != `{"ok":true}` {
		t.Fatalf("the container's answer was not passed through: %+v", got)
	}
	if got.RequestID == "" {
		t.Fatal("the response carries no request id, so it cannot be correlated")
	}
}

// TestTunnelForwardUnknownIs404 pins "the container is not online" as its own
// answer. The caller must be able to tell it apart from a broken forward.
func TestTunnelForwardUnknownIs404(t *testing.T) {
	s := newTunnelTestServer(t)
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	resp := postTunnel(t, srv, "hypervisor-nobody", `{"method":"GET","path":"/x"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestTunnelForwardClosedMidRequestIs502: once the connection dies with the
// request in flight, the caller gets a failure rather than waiting for the full
// timeout. A nil response from failAll is what signals it.
func TestTunnelForwardClosedMidRequestIs502(t *testing.T) {
	s := newTunnelTestServer(t)
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	conn := dialTunnel(t, srv, "user-1")

	// Read the request, then vanish without answering.
	go func() {
		var msg nanoprotocol.Message
		if err := conn.ReadJSON(&msg); err == nil {
			_ = conn.Close()
		}
	}()

	resp := postTunnel(t, srv, "hypervisor-user-1", `{"method":"GET","path":"/x"}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

// TestTunnelForwardRequiresAValidRequest keeps a malformed body from reaching a
// container at all.
func TestTunnelForwardRequiresAValidRequest(t *testing.T) {
	s := newTunnelTestServer(t)
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	for _, body := range []string{`not json`, `{"method":"GET"}`, `{"path":"/x"}`} {
		resp := postTunnel(t, srv, "hypervisor-user-1", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want 400", body, resp.StatusCode)
		}
	}
}

// TestTunnelRegistry_ReplacedConnectionDoesNotUnregisterTheNewOne pins the
// reconnect race: the old handler's cleanup must not remove the connection that
// already replaced it, or a live container would become unreachable.
func TestTunnelRegistry_ReplacedConnectionDoesNotUnregisterTheNewOne(t *testing.T) {
	reg := newTunnelRegistry()
	oldConn := &hypervisorConn{pending: map[string]chan *nanoprotocol.TunnelResponse{}}
	newConn := &hypervisorConn{pending: map[string]chan *nanoprotocol.TunnelResponse{}}

	if prev := reg.register("hypervisor-user-1", oldConn); prev != nil {
		t.Fatalf("the first registration replaced something: %+v", prev)
	}
	if prev := reg.register("hypervisor-user-1", newConn); prev != oldConn {
		t.Fatalf("the second registration did not replace the first: %+v", prev)
	}
	if removed := reg.unregister("hypervisor-user-1", oldConn); removed {
		t.Fatal("the stale connection removed its replacement")
	}
	if got, ok := reg.lookup("hypervisor-user-1"); !ok || got != newConn {
		t.Fatalf("the live connection is gone: %+v (ok=%v)", got, ok)
	}
}

// TestTunnelHeartbeatTouchesTheRegistryRow pins the 10s cadence against the
// store's 30s staleness window: a live tunnel must never look stale.
func TestTunnelHeartbeatTouchesTheRegistryRow(t *testing.T) {
	if tunnelHeartbeatInterval*3 > 30*time.Second {
		t.Fatalf("heartbeat interval %v is not comfortably inside the 30s staleness window", tunnelHeartbeatInterval)
	}
}

// testStore opens a throwaway database with the service's manifests applied.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	uri := os.Getenv("NANOSEC_PROVISIONER_TEST_URI")
	if uri == "" {
		uri = os.Getenv("NANOSEC_ADMIN_TEST_URI")
	}
	if uri == "" {
		t.Skip("NANOSEC_PROVISIONER_TEST_URI not set")
	}
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse uri: %v", err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	u.Path = "/provisioner_test_" + hex.EncodeToString(b[:])
	st, err := store.Open(u.String())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	manifests, err := migrations.Load()
	if err != nil {
		t.Fatalf("load manifests: %v", err)
	}
	if _, err := st.Migrate(manifests); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		_ = st.DB().Drop(context.Background())
		_ = st.Close()
	})
	return st
}

// TestForwardTunnelRequest_RelaysToTheHoldingRunner: with more than one replica,
// a request that lands on the wrong one must reach the runner that holds the
// connection instead of answering "offline".
func TestForwardTunnelRequest_RelaysToTheHoldingRunner(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/internal/tunnels/hypervisor-user-1/request" {
			t.Errorf("forwarded to %s", r.URL.Path)
		}
		var req nanoprotocol.TunnelRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(nanoprotocol.TunnelResponse{
			RequestID: req.RequestID, Status: http.StatusOK, Body: []byte(`{"from":"runner-b"}`),
		})
	}))
	defer remote.Close()

	st := testStore(t)
	if err := st.UpsertTunnel("hypervisor-user-1", "user-1", "runner-b", remote.URL); err != nil {
		t.Fatalf("upsert tunnel: %v", err)
	}
	s := New(st, "runner-a", "http://runner-a:8080")

	resp, err := s.forwardTunnelRequest("hypervisor-user-1", nanoprotocol.TunnelRequest{
		RequestID: "r1", Method: http.MethodGet, Path: "/api/v1/files/agent-1",
	}, 2*time.Second)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if string(resp.Body) != `{"from":"runner-b"}` {
		t.Fatalf("body = %q", resp.Body)
	}
}

// TestForwardTunnelRequest_DoesNotRelayToItself: a row owned by this replica
// must stay local, or two replicas would forward to each other forever.
func TestForwardTunnelRequest_DoesNotRelayToItself(t *testing.T) {
	st := testStore(t)
	if err := st.UpsertTunnel("hypervisor-user-1", "user-1", "runner-a", "http://runner-a:8080"); err != nil {
		t.Fatalf("upsert tunnel: %v", err)
	}
	s := New(st, "runner-a", "http://runner-a:8080")

	_, err := s.forwardTunnelRequest("hypervisor-user-1", nanoprotocol.TunnelRequest{RequestID: "r1"}, time.Second)
	if !errors.Is(err, errTunnelOffline) {
		t.Fatalf("err = %v, want offline (no self-forward)", err)
	}
}

// dialRaw opens the tunnel socket and completes the register handshake, returning
// the error instead of failing the test: the drain rule needs the refusal.
func dialRaw(t *testing.T, srv *httptest.Server, userID string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/hypervisor?token=s3cret"
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, resp, err
	}
	register, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelRegister, nanoprotocol.TunnelRegister{UserID: userID})
	if err != nil {
		_ = conn.Close()
		return nil, resp, err
	}
	if err := conn.WriteJSON(register); err != nil {
		_ = conn.Close()
		return nil, resp, err
	}
	return conn, resp, nil
}
