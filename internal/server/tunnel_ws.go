package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
	"sync"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanorunner/internal/metrics"
	"gitee.com/kobeyoung81/nanorunner/internal/store"
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
	// maxPendingTunnelRequests bounds the requests in flight on one connection.
	// A container bridges one request at a time, so a deeper queue only adds
	// latency before the timeout; past this the caller is better off with an
	// immediate 429 than with a request that will time out a minute later.
	maxPendingTunnelRequests = 32
	// maxTunnelBodyBytes is where the JSON tunnel stops being the right carrier.
	// Base64 in a WebSocket frame costs a third more bytes and holds the whole
	// body in memory on both ends; a 2 MiB cut-off is the documented boundary.
	maxTunnelBodyBytes = 2 << 20
	// tunnelHolderTTL is how long a cross-replica lookup is trusted. A container
	// that just reconnected elsewhere is reachable again after this at the latest;
	// the cost of a stale entry is one failed forward, not a lost message.
	tunnelHolderTTL = 5 * time.Second
	// defaultDrainDeadline is how long a drain keeps existing connections. It is
	// the proxy's grace period: long enough for an in-flight file operation,
	// short enough that a rollout is not held up by a quiet container.
	defaultDrainDeadline = 60 * time.Second
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

// errTunnelBusy means the connection already has maxPendingTunnelRequests in
// flight. The HTTP layer answers 429: the container is online, just not keeping
// up, and retrying shortly is the right thing for the caller to do.
var errTunnelBusy = errors.New("too many requests in flight on this tunnel")

// errTunnelDraining means this replica is rolling over: the tunnel is still
// reachable, but a *new* connection would be pinned to a process about to go.
var errTunnelDraining = errors.New("this replica is draining tunnels")

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

// addPending registers a waiter for one request, or refuses when the connection
// already carries its allowed number of them.
func (c *hypervisorConn) addPending(id string) (chan *nanoprotocol.TunnelResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) >= maxPendingTunnelRequests {
		return nil, errTunnelBusy
	}
	ch := make(chan *nanoprotocol.TunnelResponse, 1)
	c.pending[id] = ch
	return ch, nil
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

// size is what the active-tunnel gauge and the drain report both show.
func (r *tunnelRegistry) size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.conns)
}

// closeAll tears every connection down. Draining calls it at the deadline: the
// containers reconnect to whichever replica is still accepting them.
func (r *tunnelRegistry) closeAll() int {
	r.mu.Lock()
	conns := make([]*hypervisorConn, 0, len(r.conns))
	for name, c := range r.conns {
		conns = append(conns, c)
		delete(r.conns, name)
	}
	r.mu.Unlock()
	for _, c := range conns {
		c.failAll()
		_ = c.conn.Close()
	}
	return len(conns)
}

// handleHypervisorTunnelWS accepts a container's persistent WebSocket,
// registers it as "hypervisor-<userID>", and services tunnel_request /
// tunnel_response frames. The protocol is the one the bus already speaks, so
// agents only changed the address they dial.
func (s *Server) handleHypervisorTunnelWS(w http.ResponseWriter, r *http.Request) {
	// Refuse before the upgrade, so a draining replica answers HTTP 503 and the
	// container's reconnect loop moves on to a replica that is staying.
	if err := s.acceptingTunnels(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
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
	s.holderCache.invalidate(name)
	metrics.ActiveTunnels.Set(float64(s.tunnels.size()))
	defer func() {
		// Only the connection that is still registered removes the row; a
		// reconnect has already upserted it and its heartbeat must survive.
		if s.tunnels.unregister(name, hc) {
			s.forgetTunnel(name)
			s.holderCache.invalidate(name)
		}
		metrics.ActiveTunnels.Set(float64(s.tunnels.size()))
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
	if err := s.store.UpsertTunnel(name, userID, s.runnerID, s.advertiseURL); err != nil {
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
		// Another replica may hold this connection: the registry row names the
		// runner and, when it advertises one, the URL that reaches it.
		if resp, err := s.forwardToOtherRunner(name, req, timeout); err == nil {
			return resp, nil
		} else if !errors.Is(err, errTunnelOffline) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", errTunnelOffline, name)
	}

	ch, err := hc.addPending(req.RequestID)
	if err != nil {
		return nil, err
	}
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

// forwardToOtherRunner relays a request to the replica that holds the tunnel.
// errTunnelOffline means no replica holds it (or this one does, and forwarding
// to itself would loop).
func (s *Server) forwardToOtherRunner(name string, req nanoprotocol.TunnelRequest, timeout time.Duration) (*nanoprotocol.TunnelResponse, error) {
	if s.store == nil {
		return nil, errTunnelOffline
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	tunnel, err := s.tunnelHolder(name)
	if err != nil {
		return nil, fmt.Errorf("read tunnel holder: %w", err)
	}
	if tunnel == nil || tunnel.RunnerURL == "" || tunnel.RunnerID == s.runnerID || tunnel.RunnerURL == s.advertiseURL {
		return nil, errTunnelOffline
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	url := tunnel.RunnerURL + "/api/v1/internal/tunnels/" + neturl.PathEscape(name) + "/request"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+s.token)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("forward to runner %s: %w", tunnel.RunnerID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errTunnelOffline
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("runner %s answered %d", tunnel.RunnerID, resp.StatusCode)
	}
	var out nanoprotocol.TunnelResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode forwarded response: %w", err)
	}
	return &out, nil
}

// newTunnelRequestID gives every forwarded call its own correlation id.
func newTunnelRequestID() string { return uuid.NewString() }

// tunnelHolder reads the cross-replica registry through a short-lived cache. Each
// forward to another replica would otherwise add a database read, and a busy
// platform does not change which replica holds a container between two calls a
// millisecond apart. Five seconds is short enough that a reconnect elsewhere wins
// at most one retry.
func (s *Server) tunnelHolder(name string) (*store.Tunnel, error) {
	if tunnel, ok := s.holderCache.get(name); ok {
		return tunnel, nil
	}
	tunnel, err := s.store.Tunnel(name)
	if err != nil {
		return nil, err
	}
	s.holderCache.put(name, tunnel)
	return tunnel, nil
}

// tunnelHolderCache is the five-second memory behind tunnelHolder.
type tunnelHolderCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]holderEntry
}

type holderEntry struct {
	holder  *store.Tunnel // nil is a real answer: no replica holds it
	expires time.Time
}

func newTunnelHolderCache(ttl time.Duration) *tunnelHolderCache {
	return &tunnelHolderCache{ttl: ttl, now: time.Now, entries: map[string]holderEntry{}}
}

func (c *tunnelHolderCache) get(name string) (*store.Tunnel, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[name]
	if !ok || c.now().After(entry.expires) {
		delete(c.entries, name)
		return nil, false
	}
	return entry.holder, true
}

func (c *tunnelHolderCache) put(name string, holder *store.Tunnel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[name] = holderEntry{holder: holder, expires: c.now().Add(c.ttl)}
}

// invalidate drops one name: a connection for it just came or went here, so the
// cached answer is known to be about the old state.
func (c *tunnelHolderCache) invalidate(name string) {
	c.mu.Lock()
	delete(c.entries, name)
	c.mu.Unlock()
}

// drainState is this replica's rolling-restart switch.
type drainState struct {
	mu      sync.Mutex
	until   time.Time
	enabled bool
}

// acceptingTunnels refuses a new connection while a drain is in effect.
func (s *Server) acceptingTunnels() error {
	s.drain.mu.Lock()
	defer s.drain.mu.Unlock()
	if !s.drain.enabled {
		return nil
	}
	if time.Now().After(s.drain.until) {
		s.drain.enabled = false
		return nil
	}
	return errTunnelDraining
}

// StartDrain stops accepting new tunnels and closes the live ones at the
// deadline. The reverse proxy takes this replica out of rotation, containers
// reconnect elsewhere, and this process can finish its work and exit.
func (s *Server) StartDrain(deadline time.Duration) (live int, until time.Time) {
	live = s.tunnels.size()
	s.drain.mu.Lock()
	s.drain.enabled = true
	s.drain.until = time.Now().Add(deadline)
	until = s.drain.until
	s.drain.mu.Unlock()

	go func() {
		timer := time.NewTimer(deadline)
		defer timer.Stop()
		<-timer.C
		s.drain.mu.Lock()
		expired := s.drain.enabled && !time.Now().Before(s.drain.until)
		s.drain.enabled = false
		s.drain.mu.Unlock()
		if !expired {
			// A later drain (or none) owns the state now: do not touch it.
			return
		}
		closed := s.tunnels.closeAll()
		metrics.ActiveTunnels.Set(float64(s.tunnels.size()))
		slog.Info("tunnel drain deadline reached", "connections_closed", closed)
	}()
	return live, until
}
