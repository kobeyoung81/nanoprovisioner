package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	tunnelPingInterval = 45 * time.Second
	tunnelPongWait     = 75 * time.Second
	// tunnelRequestTimeout bounds how long a forwarded request waits for the
	// container's answer. The local bridge itself allows 60s, so waiting at
	// least as long prevents a slow workspace response from being cut off.
	tunnelRequestTimeout = 60 * time.Second
	// tunnelHeartbeatInterval is how often a live connection refreshes its
	// registry row. It stays well inside store.TunnelStaleTimeout (30s) so a
	// healthy tunnel is never pruned; it is separate from the runner heartbeat
	// because the two answer different questions ("is this container reachable"
	// vs "is this host alive").
	tunnelHeartbeatInterval = 10 * time.Second
	tunnelReadLimit         = 1 << 20
	// tunnelHandshakeTimeout bounds the wait for the first tunnel_register frame.
	tunnelHandshakeTimeout = 10 * time.Second
)

// tunnelWriteTimeout bounds a single frame write (provisioner -> container).
// Without it a half-dead tunnel could block the forward forever, never reaching
// the request timeout. It is a variable so tests can shrink it.
var tunnelWriteTimeout = 10 * time.Second

// errTunnelOffline means no connection for that name is registered on this
// replica. The HTTP layer translates it to 404 ("the container is not online"),
// which is different from 502 ("it is online but the forward failed").
var errTunnelOffline = errors.New("tunnel is not registered on this replica")

// hypervisorConn is one container's persistent WebSocket back to the
// provisioner, plus its in-flight request map.
type hypervisorConn struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan *nanoprotocol.TunnelResponse
}

func newHypervisorConn(conn *websocket.Conn) *hypervisorConn {
	return &hypervisorConn{conn: conn, pending: make(map[string]chan *nanoprotocol.TunnelResponse)}
}

// writeJSONScoped writes v with a bounded, self-clearing write deadline so a
// stalled container cannot block this service and no deadline leaks into later
// writes.
func writeJSONScoped(conn *websocket.Conn, timeout time.Duration, v any) error {
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	err := conn.WriteJSON(v)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

func (c *hypervisorConn) writeJSON(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeJSONScoped(c.conn, tunnelWriteTimeout, v)
}

func (c *hypervisorConn) addPending(id string) chan *nanoprotocol.TunnelResponse {
	ch := make(chan *nanoprotocol.TunnelResponse, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	return ch
}

func (c *hypervisorConn) removePending(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *hypervisorConn) resolvePending(resp *nanoprotocol.TunnelResponse) bool {
	c.mu.Lock()
	ch, ok := c.pending[resp.RequestID]
	if ok {
		delete(c.pending, resp.RequestID)
	}
	c.mu.Unlock()
	if ok {
		ch <- resp
		close(ch)
	}
	return ok
}

// failAll unblocks every waiter when the connection dies; they see a nil
// response and report "tunnel closed".
func (c *hypervisorConn) failAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
}

// tunnelRegistry maps tunnel names ("hypervisor-<userID>") to the live
// connection that serves them. It is per replica by design: stage 6 adds
// cross-replica forwarding through provisioner_tunnels.runner_id.
type tunnelRegistry struct {
	mu    sync.RWMutex
	conns map[string]*hypervisorConn
}

func newTunnelRegistry() *tunnelRegistry {
	return &tunnelRegistry{conns: make(map[string]*hypervisorConn)}
}

// register installs c for name and returns the connection it replaced, if any.
func (r *tunnelRegistry) register(name string, c *hypervisorConn) *hypervisorConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.conns[name]
	r.conns[name] = c
	return prev
}

// unregister removes c only if it is still the registered connection, so a
// reconnect that already replaced it is not torn down by the old handler. The
// bool reports whether this call removed anything.
func (r *tunnelRegistry) unregister(name string, c *hypervisorConn) bool {
	r.mu.Lock()
	removed := r.conns[name] == c
	if removed {
		delete(r.conns, name)
	}
	r.mu.Unlock()
	if removed {
		c.failAll()
	}
	return removed
}

func (r *tunnelRegistry) lookup(name string) (*hypervisorConn, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.conns[name]
	return c, ok
}

// handleHypervisorTunnelWS accepts a container's persistent WebSocket,
// registers it as "hypervisor-<userID>", and services tunnel_request /
// tunnel_response frames. The protocol is the one the bus already speaks, so
// agents only changed the address they dial.
func (s *Server) handleHypervisorTunnelWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(tunnelReadLimit)

	_ = conn.SetReadDeadline(time.Now().Add(tunnelHandshakeTimeout))
	var reg nanoprotocol.Message
	if err := conn.ReadJSON(&reg); err != nil {
		return
	}
	if reg.Type != nanoprotocol.TypeTunnelRegister {
		_ = conn.WriteJSON(nanoprotocol.NewErrorMessage("expected tunnel_register"))
		return
	}
	var payload nanoprotocol.TunnelRegister
	if err := reg.DecodePayload(&payload); err != nil || payload.UserID == "" {
		_ = conn.WriteJSON(nanoprotocol.NewErrorMessage("invalid tunnel_register"))
		return
	}

	name := "hypervisor-" + payload.UserID
	hc := newHypervisorConn(conn)
	if prev := s.tunnels.register(name, hc); prev != nil {
		// A reconnect replaced a half-dead connection: its requests are gone
		// anyway, so unblock them rather than leave them waiting for a timeout.
		prev.failAll()
	}
	s.rememberTunnel(name, payload.UserID)
	defer func() {
		// Only the connection that is still registered removes the row; a
		// reconnect has already upserted it and its heartbeat must survive.
		if s.tunnels.unregister(name, hc) {
			s.forgetTunnel(name)
		}
	}()

	_ = conn.SetReadDeadline(time.Now().Add(tunnelPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(tunnelPongWait))
	})

	registered, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelRegistered, nanoprotocol.TunnelRegistered{})
	if err != nil {
		return
	}
	if err := hc.writeJSON(registered); err != nil {
		return
	}
	slog.Info("hypervisor tunnel registered", "user_id", payload.UserID, "tunnel", name, "runner_id", s.runnerID)

	done := make(chan struct{})
	defer close(done)
	go s.tunnelKeepalive(conn, done)
	go s.tunnelHeartbeat(name, done)

	for {
		var msg nanoprotocol.Message
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg.Type {
		case nanoprotocol.TypeTunnelResponse:
			var resp nanoprotocol.TunnelResponse
			if err := msg.DecodePayload(&resp); err != nil {
				continue
			}
			hc.resolvePending(&resp)
		case nanoprotocol.TypeTunnelStreamOutput:
			var p nanoprotocol.TunnelStreamOutput
			if err := msg.DecodePayload(&p); err != nil {
				continue
			}
			s.routeTerminalOutput(p.StreamID, p.Data)
		case nanoprotocol.TypeTunnelStreamClosed:
			var p nanoprotocol.TunnelStreamClosed
			if err := msg.DecodePayload(&p); err != nil {
				continue
			}
			s.routeTerminalClosed(p.StreamID, p.Reason)
		}
	}
}

// tunnelKeepalive pings the container so idle tunnels survive intermediaries.
func (s *Server) tunnelKeepalive(conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(tunnelPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				_ = conn.Close()
				return
			}
		}
	}
}

// tunnelHeartbeat keeps the registry row fresh for as long as the connection
// lives. A failure is logged and ignored: forwarding inside this replica does
// not depend on the row.
func (s *Server) tunnelHeartbeat(name string, done <-chan struct{}) {
	ticker := time.NewTicker(tunnelHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if s.store == nil {
				return
			}
			if err := s.store.TouchTunnel(name); err != nil {
				slog.Warn("tunnel heartbeat failed", "tunnel", name, "error", err)
			}
		}
	}
}

func (s *Server) rememberTunnel(name, userID string) {
	if s.store == nil {
		return
	}
	if err := s.store.UpsertTunnel(name, userID, s.runnerID); err != nil {
		slog.Warn("record tunnel failed", "tunnel", name, "error", err)
	}
}

func (s *Server) forgetTunnel(name string) {
	if s.store == nil {
		return
	}
	if err := s.store.DeleteTunnel(name); err != nil {
		slog.Warn("remove tunnel failed", "tunnel", name, "error", err)
	}
}

// forwardTunnelRequest sends a tunnel_request to the named connection and
// blocks for the matching tunnel_response. errTunnelOffline is returned when no
// connection for that name lives here.
func (s *Server) forwardTunnelRequest(name string, req nanoprotocol.TunnelRequest, timeout time.Duration) (*nanoprotocol.TunnelResponse, error) {
	hc, ok := s.tunnels.lookup(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", errTunnelOffline, name)
	}

	ch := hc.addPending(req.RequestID)
	defer hc.removePending(req.RequestID)

	msg, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelRequest, req)
	if err != nil {
		return nil, err
	}
	if err := hc.writeJSON(msg); err != nil {
		return nil, fmt.Errorf("write tunnel request: %w", err)
	}

	select {
	case resp := <-ch:
		if resp == nil {
			return nil, errors.New("tunnel closed before the response")
		}
		return resp, nil
	case <-time.After(timeout):
		return nil, errors.New("tunnel request timed out")
	}
}

// newTunnelRequestID gives every forwarded call its own correlation id.
func newTunnelRequestID() string { return uuid.NewString() }
