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
	"time"

	"gitee.com/kobeyoung81/nanorunner/internal/config"
	"gitee.com/kobeyoung81/nanorunner/internal/store"
)

// Server serves the internal API.
type Server struct {
	store *store.Store
	srv   *http.Server
	token string
}

// New builds the API around the queue.
func New(st *store.Store) *Server {
	s := &Server{store: st, token: config.InternalToken()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/internal/container-requests", s.enqueueForAgent)
	mux.HandleFunc("POST /api/v1/internal/containers", s.enqueue)
	mux.HandleFunc("POST /api/v1/internal/containers/{userID}/dispose", s.dispose)
	mux.HandleFunc("POST /api/v1/internal/containers/{userID}/restart", s.restart)
	mux.HandleFunc("POST /api/v1/internal/containers/{userID}/reprovision", s.reprovision)
	s.srv = &http.Server{Addr: config.HTTPAddr(), Handler: s.requireToken(mux), ReadHeaderTimeout: 10 * time.Second}
	return s
}

// ListenAndServe blocks until the listener closes.
func (s *Server) ListenAndServe() error { return s.srv.ListenAndServe() }

// Shutdown stops the listener.
func (s *Server) Shutdown() error { return s.srv.Close() }

// requireToken refuses every call unless it carries the deployment's shared
// token; without one configured it fails closed rather than opening up.
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "internal token is not configured"})
			return
		}
		presented := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if len(presented) <= len(prefix) || presented[:len(prefix)] != prefix ||
			subtle.ConstantTimeCompare([]byte(presented[len(prefix):]), []byte(s.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
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
