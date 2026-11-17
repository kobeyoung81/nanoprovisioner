// Package server is the provisioner's internal HTTP API.
//
// The queue is this service's, so the console's container actions (enqueue,
// dispose, restart, reprovision) arrive here. The bus keeps its user-facing
// routes and forwards them: callers did not have to learn a new address, and the
// bus stays the one door the console knows.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gitee.com/kobeyoung81/nanorunner/internal/config"
	"gitee.com/kobeyoung81/nanorunner/internal/metrics"
	"gitee.com/kobeyoung81/nanorunner/internal/portalclient"
	"gitee.com/kobeyoung81/nanorunner/internal/store"
	"github.com/gorilla/websocket"
)

// Server serves the internal API and the two WebSocket surfaces containers and
// the terminal need.
type Server struct {
	store *store.Store
	srv   *http.Server
	token string
	// runnerID is stamped on tunnel rows so stage 6 can forward a request to the
	// replica that holds the connection.
	runnerID string
	// advertiseURL lets a replica forward a tunnel request to the one holding
	// the connection (empty = local only).
	advertiseURL string
	portal       *portalclient.Client
	upgrader     websocket.Upgrader
	tunnels      *tunnelRegistry
	terminalSubs *terminalSubRegistry
	// holderCache is the five-second memory of who holds a tunnel on another
	// replica; drain is the rolling-restart switch.
	holderCache *tunnelHolderCache
	drain       *drainState
}

// New builds the API around the queue. runnerID identifies this replica in the
// tunnel registry.
func New(st *store.Store, runnerID, advertiseURL string) *Server {
	s := &Server{
		store:        st,
		token:        config.InternalToken(),
		runnerID:     runnerID,
		advertiseURL: advertiseURL,
		portal:       portalclient.New(config.PortalURL(), config.InternalToken()),
		upgrader:     websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		tunnels:      newTunnelRegistry(),
		terminalSubs: newTerminalSubRegistry(),
		holderCache:  newTunnelHolderCache(tunnelHolderTTL),
		drain:        &drainState{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": Version})
	})
	// Tunnels used to terminate at the bus, whose in-memory registry meant a
	// second replica could not reach a container connected to the first. They
	// terminate here now: the container's reachability is provisioning's fact.
	mux.HandleFunc("GET /ws/hypervisor", s.handleHypervisorTunnelWS)
	mux.HandleFunc("GET /ws/terminal", s.handleTerminalBridgeWS)
	mux.HandleFunc("POST /api/v1/internal/tunnels/{name}/request", s.forwardTunnelRequestHTTP)
	// The streamed path: bodies above the inline cut-off travel as chunks.
	mux.HandleFunc("POST /api/v1/internal/tunnels/{name}/proxy", s.proxyTunnelRequestHTTP)
	mux.HandleFunc("POST /api/v1/internal/tunnels/drain", s.restDrainTunnels)
	// The runner registry is this service's too: it registers itself and reports
	// its heartbeat, and the console prunes dead rows through the bus.
	mux.HandleFunc("POST /api/v1/internal/runners", s.registerRunner)
	mux.HandleFunc("DELETE /api/v1/internal/runners/{id}", s.deleteRunner)
	mux.HandleFunc("POST /api/v1/internal/container-requests", s.enqueueForAgent)
	mux.HandleFunc("POST /api/v1/internal/containers", s.enqueue)
	mux.HandleFunc("POST /api/v1/internal/containers/{userID}/dispose", s.dispose)
	mux.HandleFunc("POST /api/v1/internal/containers/{userID}/restart", s.restart)
	mux.HandleFunc("POST /api/v1/internal/containers/{userID}/reprovision", s.reprovision)

	// The console's reads: this module owns the queue and the runner registry.
	mux.HandleFunc("GET /api/v1/internal/console/runners", s.restConsoleRunners)
	mux.HandleFunc("GET /api/v1/internal/console/runners/counts", s.restConsoleRunnerCounts)
	mux.HandleFunc("GET /api/v1/internal/console/containers", s.restConsoleContainers)
	mux.HandleFunc("GET /api/v1/internal/console/containers/counts", s.restConsoleContainerCounts)
	// Operational numbers, anonymous: a scraper is infrastructure, not a caller.
	mux.Handle("GET /metrics", metrics.Handler())
	s.srv = &http.Server{
		Addr:              config.HTTPAddr(),
		Handler:           metrics.Middleware(s.requireToken(mux)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// ListenAndServe blocks until the listener closes.
func (s *Server) ListenAndServe() error { return s.srv.ListenAndServe() }

// Shutdown stops the listener.
func (s *Server) Shutdown() error { return s.srv.Close() }

// requireToken refuses every call unless it carries the deployment's shared
// token; without one configured it fails closed rather than opening up. The
// token may also arrive as ?token= because a WebSocket handshake built from a
// URL has no header to carry it (the gateway dials the terminal that way).
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A probe carries no token; liveness must not depend on one.
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		if s.token == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "internal token is not configured"})
			return
		}
		presented := r.Header.Get("Authorization")
		const prefix = "Bearer "
		token := ""
		if len(presented) > len(prefix) && strings.EqualFold(presented[:len(prefix)], prefix) {
			token = strings.TrimSpace(presented[len(prefix):])
		} else {
			token = strings.TrimSpace(r.URL.Query().Get("token"))
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) registerRunner(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RunnerID   string `json:"runner_id"`
		Hostname   string `json:"hostname"`
		DockerHost string `json:"docker_host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if body.RunnerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "runner_id is required"})
		return
	}
	if err := s.store.RegisterRunner(body.RunnerID, body.Hostname, body.DockerHost); err != nil {
		slog.Error("register runner", "runner_id", body.RunnerID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"runner_id": body.RunnerID, "status": "online"})
}

func (s *Server) deleteRunner(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "runner id is required"})
		return
	}
	if err := s.store.DeleteRunner(id); err != nil {
		slog.Error("delete runner", "runner_id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) enqueueForAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserID  string `json:"user_id"`
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if body.UserID == "" || body.AgentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user_id and agent_id are required"})
		return
	}
	if err := s.store.EnsureContainerRequest(body.UserID, body.AgentID); err != nil {
		slog.Error("enqueue container request", "user_id", body.UserID, "agent_id", body.AgentID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "requested"})
}

func (s *Server) enqueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserID    string `json:"user_id"`
		BindingID int64  `json:"binding_id"`
		AgentID   string `json:"agent_id"`
		Level     string `json:"level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if body.UserID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user_id is required"})
		return
	}
	if err := s.store.CreateContainerRequest(body.UserID, body.BindingID, body.AgentID, body.Level); err != nil {
		if errors.Is(err, store.ErrContainerAlreadyRequested) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "container already requested"})
			return
		}
		slog.Error("create container request", "user_id", body.UserID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "requested"})
}

func (s *Server) dispose(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DisposeContainerRequest(r.PathValue("userID"), 0); err != nil {
		s.writeStoreError(w, err, "dispose container request")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disposing"})
}

func (s *Server) restart(w http.ResponseWriter, r *http.Request) {
	if err := s.store.RestartContainerRequest(r.PathValue("userID")); err != nil {
		s.writeStoreError(w, err, "restart container request")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "restarting"})
}

func (s *Server) reprovision(w http.ResponseWriter, r *http.Request) {
	if err := s.store.ReprovisionContainerRequest(r.PathValue("userID")); err != nil {
		s.writeStoreError(w, err, "reprovision container request")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
}

// writeStoreError maps "nothing matched" onto 404; anything else is a server
// failure, logged with the action it belonged to.
func (s *Server) writeStoreError(w http.ResponseWriter, err error, action string) {
	if errors.Is(err, store.ErrContainerNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	slog.Error(action, "error", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
