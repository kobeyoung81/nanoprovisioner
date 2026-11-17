package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"gitee.com/kobeyoung81/nanorunner/internal/metrics"

	"gitee.com/kobeyoung81/nanoprotocol"
)

// forwardTunnelRequestHTTP is the internal surface the bus (and, once it exists,
// the workspace module and the portal) uses to reach a container: the caller
// describes an HTTP request, this service puts it on the right tunnel and hands
// back the response.
//
// The two failures are deliberately distinct:
//   - 404 means no tunnel for that name is registered here — the container is not
//     online (or, after stage 6, lives on another replica);
//   - 502 means it is registered but the forward failed (write error, timeout,
//     or the tunnel closed mid-request).
func (s *Server) forwardTunnelRequestHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tunnel name is required"})
		return
	}
	var req nanoprotocol.TunnelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.Method == "" || req.Path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "method and path are required"})
		return
	}
	if req.RequestID == "" {
		req.RequestID = newTunnelRequestID()
	}
	metrics.TunnelBodyBytes.Observe(float64(len(req.Body)))
	if len(req.Body) > maxTunnelInlineBody {
		// This endpoint answers with one JSON message, so a large body would be
		// base64'd twice and held whole at both ends. Point the caller at the
		// streamed endpoint instead of failing with a generic 502.
		metrics.TunnelRequests.WithLabelValues("too_large").Inc()
		writeRelayError(w, http.StatusRequestEntityTooLarge, "too_large",
			"the request body is larger than the inline tunnel limit; use the streamed /proxy endpoint")
		return
	}

	start := time.Now()
	resp, err := s.forwardTunnelRequest(name, req, tunnelRequestTimeout)
	metrics.TunnelRequestSeconds.Observe(time.Since(start).Seconds())
	if err != nil {
		if errors.Is(err, errTunnelBusy) {
			writeTunnelError(w, err)
			return
		}
		slog.Warn("forward tunnel request failed", "tunnel", name, "request_id", req.RequestID, "error", err)
		writeTunnelError(w, err)
		return
	}
	metrics.TunnelRequests.WithLabelValues("ok").Inc()
	// This endpoint keeps its JSON envelope: callers older than the streamed
	// path still decode TunnelResponse from it.
	writeJSON(w, http.StatusOK, resp)
}

// restDrainTunnels is the rolling-restart switch: stop accepting new tunnels,
// keep the live ones until the deadline, then close them. A reverse proxy takes
// this replica out of rotation first, so the containers reconnect to a replica
// that is staying.
//
//	POST /api/v1/internal/tunnels/drain {"deadline_seconds": 60}
func (s *Server) restDrainTunnels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeadlineSeconds int `json:"deadline_seconds"`
	}
	// An empty body is a 60-second drain; a bad body is the caller's mistake.
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
	}
	deadline := time.Duration(body.DeadlineSeconds) * time.Second
	if deadline <= 0 {
		deadline = defaultDrainDeadline
	}
	live, until := s.StartDrain(deadline)
	slog.Info("tunnel drain started", "connections", live, "deadline", until)
	writeJSON(w, http.StatusOK, map[string]any{
		"draining":    true,
		"connections": live,
		"deadline":    until.UTC().Format(time.RFC3339),
	})
}
