package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

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

	resp, err := s.forwardTunnelRequest(name, req, tunnelRequestTimeout)
	if err != nil {
		if errors.Is(err, errTunnelOffline) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "tunnel is offline"})
			return
		}
		slog.Warn("forward tunnel request failed", "tunnel", name, "request_id", req.RequestID, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "tunnel forward failed"})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
