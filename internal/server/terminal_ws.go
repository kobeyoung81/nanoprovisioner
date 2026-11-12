package server

import (
	"encoding/base64"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"github.com/gorilla/websocket"
)

const (
	terminalBridgePingInterval = 45 * time.Second
	terminalBridgePongWait     = 75 * time.Second
)

// terminalFrame is the JSON envelope exchanged on the gateway <-> provisioner
// terminal bridge WebSocket. Data carries base64-encoded bytes in both
// directions, so a malformed frame drops one frame instead of the bridge.
type terminalFrame struct {
	Type       string `json:"type"` // bind|input|resize|close | output|closed|error
	TerminalID string `json:"terminal_id"`
	Data       string `json:"data,omitempty"`
	Cols       int    `json:"cols,omitempty"`
	Rows       int    `json:"rows,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message,omitempty"`
}

// terminalSubRegistry tracks, per terminal_id, which gateway bridges are
// attached. It is per replica: the tunnel that serves the PTY lives here too, so
// the fan-out never has to cross a process boundary (stage 6 forwards at the
// tunnel level, not at the frame level).
type terminalSubRegistry struct {
	mu   sync.RWMutex
	subs map[string]map[*terminalBridge]struct{}
}

func newTerminalSubRegistry() *terminalSubRegistry {
	return &terminalSubRegistry{subs: make(map[string]map[*terminalBridge]struct{})}
}

func (r *terminalSubRegistry) add(terminalID string, b *terminalBridge) {
	r.mu.Lock()
	defer r.mu.Unlock()
	set, ok := r.subs[terminalID]
	if !ok {
		set = make(map[*terminalBridge]struct{})
		r.subs[terminalID] = set
	}
	set[b] = struct{}{}
}

func (r *terminalSubRegistry) list(terminalID string) []*terminalBridge {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*terminalBridge, 0, len(r.subs[terminalID]))
	for b := range r.subs[terminalID] {
		out = append(out, b)
	}
	return out
}

func (r *terminalSubRegistry) removeAll(terminalID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.subs, terminalID)
}

// removeBridge drops every subscription held by a bridge (on disconnect).
func (r *terminalSubRegistry) removeBridge(b *terminalBridge) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for terminalID, set := range r.subs {
		if _, ok := set[b]; ok {
			delete(set, b)
			if len(set) == 0 {
				delete(r.subs, terminalID)
			}
		}
	}
}

// terminalBridge is one gateway <-> provisioner connection carrying terminal io
// for a single user+agent. It is a dumb relay: frames become tunnel_stream_*
// on the user's hypervisor tunnel and back.
type terminalBridge struct {
	userID  string
	agentID string
	conn    *websocket.Conn
	writeMu sync.Mutex
	server  *Server
}

func (b *terminalBridge) write(v any) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	return writeJSONScoped(b.conn, tunnelWriteTimeout, v)
}

func (b *terminalBridge) sendTunnel(msgType string, payload any) {
	hc, ok := b.server.tunnels.lookup("hypervisor-" + b.userID)
	if !ok {
		return // container unreachable; frames drop until it reconnects
	}
	msg, err := nanoprotocol.NewMessage(msgType, payload)
	if err != nil {
		return
	}
	_ = hc.writeJSON(msg)
}

// handleTerminalBridgeWS accepts a gateway connection for terminal io and
// bridges it to the user's hypervisor tunnel. The gateway already validated the
// JWT; ownership is re-checked here because this endpoint's only guard is the
// shared token.
func (s *Server) handleTerminalBridgeWS(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	agentID := r.URL.Query().Get("agent_id")
	if userID == "" || agentID == "" {
		http.Error(w, "user_id and agent_id are required", http.StatusBadRequest)
		return
	}

	owned, err := s.portal.AgentOwnedBy(r.Context(), userID, agentID)
	if err != nil {
		slog.Error("terminal ownership check failed", "user_id", userID, "agent_id", agentID, "error", err)
		http.Error(w, `{"error":"agent lookup failed"}`, http.StatusInternalServerError)
		return
	}
	if !owned {
		http.Error(w, `{"error":"access denied"}`, http.StatusForbidden)
		return
	}
	// No tunnel on this replica means the container is not connected here. The
	// gateway treats 404 as "try the other endpoint" during the dual-track
	// window, so this check has to happen before the upgrade.
	if _, ok := s.tunnels.lookup("hypervisor-" + userID); !ok {
		http.Error(w, `{"error":"tunnel is not registered"}`, http.StatusNotFound)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	conn.SetReadLimit(1 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(terminalBridgePongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(terminalBridgePongWait))
	})

	bridge := &terminalBridge{userID: userID, agentID: agentID, conn: conn, server: s}
	defer s.terminalSubs.removeBridge(bridge)

	done := make(chan struct{})
	defer close(done)
	go s.terminalBridgeKeepalive(conn, done)

	slog.Info("terminal bridge connected", "user_id", userID, "agent_id", agentID)

	for {
		var frame terminalFrame
		if err := conn.ReadJSON(&frame); err != nil {
			return
		}
		switch frame.Type {
		case "bind":
			s.terminalSubs.add(frame.TerminalID, bridge)
		case "input":
			data, err := base64.StdEncoding.DecodeString(frame.Data)
			if err != nil {
				slog.Warn("terminal input frame has invalid base64", "user_id", userID, "terminal_id", frame.TerminalID, "error", err)
				continue
			}
			bridge.sendTunnel(nanoprotocol.TypeTunnelStreamInput, nanoprotocol.TunnelStreamInput{
				StreamID: frame.TerminalID,
				Data:     data,
			})
		case "resize":
			if frame.Cols < 2 || frame.Rows < 1 || frame.Cols > 1000 || frame.Rows > 500 {
				slog.Warn("terminal resize frame out of bounds", "user_id", userID, "terminal_id", frame.TerminalID, "cols", frame.Cols, "rows", frame.Rows)
				continue
			}
			bridge.sendTunnel(nanoprotocol.TypeTunnelStreamResize, nanoprotocol.TunnelStreamResize{
				StreamID: frame.TerminalID,
				Cols:     frame.Cols,
				Rows:     frame.Rows,
			})
		case "close":
			bridge.sendTunnel(nanoprotocol.TypeTunnelStreamClose, nanoprotocol.TunnelStreamClose{
				StreamID: frame.TerminalID,
			})
		default:
			slog.Warn("unknown terminal frame", "type", frame.Type)
		}
	}
}

func (s *Server) terminalBridgeKeepalive(conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(terminalBridgePingInterval)
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

// routeTerminalOutput fans PTY output from a hypervisor tunnel out to every
// bridge subscribed to that terminal.
func (s *Server) routeTerminalOutput(terminalID string, data []byte) {
	bridges := s.terminalSubs.list(terminalID)
	if len(bridges) == 0 {
		return
	}
	for _, b := range bridges {
		_ = b.write(terminalFrame{Type: "output", TerminalID: terminalID, Data: base64.StdEncoding.EncodeToString(data)})
	}
}

// routeTerminalClosed notifies subscribers a terminal ended and clears the
// subscription so a dead terminal cannot accumulate.
func (s *Server) routeTerminalClosed(terminalID, reason string) {
	bridges := s.terminalSubs.list(terminalID)
	s.terminalSubs.removeAll(terminalID)
	if len(bridges) == 0 {
		return
	}
	for _, b := range bridges {
		_ = b.write(terminalFrame{Type: "closed", TerminalID: terminalID, Reason: reason})
	}
}
