package server

// The streaming half of the tunnel. A body larger than
// nanoprotocol.TunnelInlineBodyBytes travels as chunk frames so neither end ever
// holds a whole file: the size of a download stops being the size of a
// WebSocket frame. Small bodies keep the one-message path, which is both the
// common case and the compatibility path for containers that predate the
// feature.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"gitee.com/kobeyoung81/nanorunner/internal/metrics"
)

const (
	// tunnelFirstByteTimeout bounds the wait for the container's response head.
	// It replaces the old whole-request timeout: a large file on a slow link is
	// legitimate, a container that never answers is not.
	tunnelFirstByteTimeout = 60 * time.Second
	// tunnelIdleTimeout is the longest gap allowed between two chunks.
	tunnelIdleTimeout = 60 * time.Second
)

// proxyTunnelRequestHTTP is the streamed counterpart of
// forwardTunnelRequestHTTP. The caller names the method and path in the query
// (url.Values on the caller side encodes them, so a path with its own query
// string survives) and the request body becomes the container's request body.
//
//	POST /api/v1/internal/tunnels/{name}/proxy?method=GET&path=%2Fapi%2Fv1%2F...
func (s *Server) proxyTunnelRequestHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tunnel name is required"})
		return
	}
	method := r.URL.Query().Get("method")
	target := r.URL.Query().Get("path")
	if method == "" || target == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "method and path are required"})
		return
	}

	// Whether the body is streamed is a property of the body; whether the answer
	// is depends on the container's response, which this side cannot know in
	// advance. So a request with no body (a download) still goes through the
	// streaming handler: the file is on the way back.
	if hc, ok := s.tunnels.lookup(name); ok && !hc.supportsBodyStream() {
		// A pre-streaming container: keep the old one-message behaviour, which
		// still serves small files and fails the same way it always did on large
		// ones. The caller learns the container needs a restart from /proxy's
		// answer when it asks for something bigger than an inline body.
		data, err := io.ReadAll(io.LimitReader(r.Body, nanoprotocol.TunnelInlineBodyBytes+1))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read the request body"})
			return
		}
		if len(data) > nanoprotocol.TunnelInlineBodyBytes {
			metrics.TunnelRequests.WithLabelValues("too_large").Inc()
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
				"error": "this container's image predates large-file transfer; restart it and retry",
			})
			return
		}
		s.forwardInlineBody(w, r, name, method, target, data)
		return
	}
	s.forwardChunkedBody(w, r, name, method, target, r.Body, r.ContentLength)
}

// forwardInlineBody keeps the one-message path: a small request is one frame
// and one answer, exactly as before the streaming feature existed.
func (s *Server) forwardInlineBody(w http.ResponseWriter, r *http.Request, name, method, target string, body []byte) {
	req := nanoprotocol.TunnelRequest{
		RequestID: newTunnelRequestID(),
		Method:    method,
		Path:      target,
		Headers:   forwardableHeaders(r.Header),
		Body:      body,
	}
	metrics.TunnelBodyBytes.Observe(float64(len(body)))
	resp, err := s.forwardTunnelRequest(name, req, tunnelRequestTimeout)
	if err != nil {
		writeTunnelError(w, err)
		return
	}
	metrics.TunnelRequests.WithLabelValues("ok").Inc()
	writeTunnelResponse(w, resp)
}

// forwardChunkedBody streams the request body up and the response body down.
func (s *Server) forwardChunkedBody(w http.ResponseWriter, r *http.Request, name, method, target string, body io.Reader, length int64) {
	hc, ok := s.tunnels.lookup(name)
	if !ok {
		if s.forwardChunkedToOtherRunner(w, r, name, method, target, body, length) {
			return
		}
		metrics.TunnelRequests.WithLabelValues("offline").Inc()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "tunnel is offline"})
		return
	}
	if !hc.supportsBodyStream() {
		// Sending chunks to a pre-streaming hypervisor would silently lose the
		// body; a 413 that says what to do is better than a 502 that does not.
		metrics.TunnelRequests.WithLabelValues("too_large").Inc()
		writeRelayError(w, http.StatusRequestEntityTooLarge, "too_large",
			"this container's image predates large-file transfer; restart it and retry")
		return
	}

	reqID := newTunnelRequestID()
	pending, err := hc.addPending(reqID)
	if err != nil {
		if errors.Is(err, errTunnelBusy) {
			metrics.TunnelRequests.WithLabelValues("busy").Inc()
			writeJSON(w, http.StatusTooManyRequests, map[string]string{
				"error": "too many requests in flight on this container; retry shortly",
			})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer hc.removePending(reqID)

	metrics.TunnelStreamsActive.Inc()
	defer metrics.TunnelStreamsActive.Dec()

	req := nanoprotocol.TunnelRequest{
		RequestID: reqID,
		Method:    method,
		Path:      target,
		Headers:   forwardableHeaders(r.Header),
	}
	var toStream io.Reader
	if length >= 0 && length <= nanoprotocol.TunnelInlineBodyBytes {
		data, err := io.ReadAll(io.LimitReader(body, nanoprotocol.TunnelInlineBodyBytes+1))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read the request body"})
			return
		}
		if len(data) <= nanoprotocol.TunnelInlineBodyBytes {
			req.Body = data
		} else {
			// The caller under-declared its length: send what was read, then the
			// rest, as one stream.
			req.Streamed = true
			req.BodyLength = -1
			toStream = io.MultiReader(bytes.NewReader(data), body)
		}
	} else {
		req.Streamed = true
		req.BodyLength = length
		toStream = body
	}

	msg, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelRequest, req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if err := hc.writeJSON(msg); err != nil {
		metrics.TunnelRequests.WithLabelValues("failed").Inc()
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "tunnel forward failed"})
		return
	}

	if toStream != nil {
		pumpCtx, stopPump := context.WithCancel(r.Context())
		defer stopPump()
		go func() { _ = pumpRequestBody(pumpCtx, hc, reqID, toStream) }()
	}

	// The head arrives once the container accepted the request and got its
	// local answer's status; the body then follows as chunks.
	var head *nanoprotocol.TunnelResponse
	select {
	case resp, open := <-pending.head:
		if !open || resp == nil {
			metrics.TunnelRequests.WithLabelValues("failed").Inc()
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "tunnel closed before the response"})
			return
		}
		head = resp
	case <-time.After(tunnelFirstByteTimeout):
		s.cancelTunnel(hc, reqID)
		metrics.TunnelRequests.WithLabelValues("timeout").Inc()
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "tunnel request timed out"})
		return
	case <-r.Context().Done():
		s.cancelTunnel(hc, reqID)
		return
	}

	if !head.Streamed {
		// The container answered with everything inline (a small body, or an
		// error page). Nothing more is coming.
		metrics.TunnelRequests.WithLabelValues("ok").Inc()
		writeTunnelResponse(w, head)
		return
	}

	copyTunnelHeaders(w.Header(), head.Headers)
	if head.BodyLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(head.BodyLength, 10))
	}
	w.WriteHeader(head.Status)
	flusher, _ := w.(http.Flusher)

	timer := time.NewTimer(tunnelIdleTimeout)
	defer timer.Stop()
	for {
		select {
		case chunk, open := <-pending.chunks:
			if !open {
				// The connection dropped mid-body; the status line is already
				// out, so the only honest signal left is a short body.
				slog.Warn("tunnel closed mid-stream", "tunnel", name, "request_id", reqID)
				return
			}
			if chunk.Error != "" {
				slog.Warn("tunnel stream failed", "tunnel", name, "request_id", reqID, "error", chunk.Error)
				return
			}
			if len(chunk.Data) > 0 {
				if _, err := w.Write(chunk.Data); err != nil {
					s.cancelTunnel(hc, reqID)
					return
				}
				metrics.TunnelStreamBytes.Add(float64(len(chunk.Data)))
				if flusher != nil {
					flusher.Flush()
				}
			}
			if chunk.Final {
				metrics.TunnelRequests.WithLabelValues("ok").Inc()
				return
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(tunnelIdleTimeout)
		case <-timer.C:
			s.cancelTunnel(hc, reqID)
			metrics.TunnelRequests.WithLabelValues("timeout").Inc()
			return
		case <-r.Context().Done():
			s.cancelTunnel(hc, reqID)
			return
		}
	}
}

// pumpRequestBody reads the caller's body and writes it out as chunks. A read
// error terminates the stream with a reason so the container stops waiting.
func pumpRequestBody(ctx context.Context, hc *hypervisorConn, requestID string, body io.Reader) error {
	buf := make([]byte, nanoprotocol.TunnelChunkBytes)
	seq := 0
	for {
		if err := ctx.Err(); err != nil {
			writeTunnelChunk(hc, nanoprotocol.TunnelBodyChunk{RequestID: requestID, Seq: seq, Error: "the caller stopped sending"})
			return err
		}
		n, err := body.Read(buf)
		if n > 0 {
			if werr := writeTunnelChunk(hc, nanoprotocol.TunnelBodyChunk{
				RequestID: requestID,
				Seq:       seq,
				Data:      buf[:n],
			}); werr != nil {
				return werr
			}
			seq++
		}
		if errors.Is(err, io.EOF) {
			return writeTunnelChunk(hc, nanoprotocol.TunnelBodyChunk{RequestID: requestID, Seq: seq, Final: true})
		}
		if err != nil {
			writeTunnelChunk(hc, nanoprotocol.TunnelBodyChunk{RequestID: requestID, Seq: seq, Error: err.Error()})
			return err
		}
	}
}

func writeTunnelChunk(hc *hypervisorConn, chunk nanoprotocol.TunnelBodyChunk) error {
	msg, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelRequestBody, chunk)
	if err != nil {
		return err
	}
	return hc.writeJSON(msg)
}

// cancelTunnel tells the container to stop working on a request nobody waits
// for. A failure is only logged: the request is already over.
func (s *Server) cancelTunnel(hc *hypervisorConn, requestID string) {
	metrics.TunnelCancelled.Inc()
	msg, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelCancel, nanoprotocol.TunnelCancel{RequestID: requestID})
	if err != nil {
		return
	}
	if err := hc.writeJSON(msg); err != nil {
		slog.Debug("cancel a tunnel request", "request_id", requestID, "error", err)
	}
}

// forwardChunkedToOtherRunner relays the streamed request to the replica that
// holds the connection. It reports false when no other replica can serve it.
func (s *Server) forwardChunkedToOtherRunner(w http.ResponseWriter, r *http.Request, name, method, target string, body io.Reader, length int64) bool {
	if s.store == nil {
		return false
	}
	tunnel, err := s.tunnelHolder(name)
	if err != nil || tunnel == nil || tunnel.RunnerURL == "" || tunnel.RunnerID == s.runnerID || tunnel.RunnerURL == s.advertiseURL {
		return false
	}
	query := url.Values{}
	query.Set("method", method)
	query.Set("path", target)
	endpoint := tunnel.RunnerURL + "/api/v1/internal/tunnels/" + url.PathEscape(name) + "/proxy?" + query.Encode()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, body)
	if err != nil {
		return false
	}
	req.ContentLength = length
	for key, values := range forwardableHeaders(r.Header) {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := tunnelRelayClient.Do(req)
	if err != nil {
		slog.Warn("relay a streamed tunnel request", "tunnel", name, "error", err)
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return false
	}
	copyTunnelHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return true
}

// tunnelRelayClient has no whole-request timeout: a large relayed download is
// legitimate, and the caller's context still ends it when the browser leaves.
var tunnelRelayClient = &http.Client{
	Transport: &http.Transport{ResponseHeaderTimeout: tunnelFirstByteTimeout},
}

// forwardableHeaders copies the caller's end-to-end headers. Hop-by-hop ones and
// the length belong to the transport, not to the container request.
func forwardableHeaders(in http.Header) map[string][]string {
	out := make(map[string][]string, len(in))
	for key, values := range in {
		if hopByHopHeader(key) || strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "Cookie") {
			continue
		}
		out[key] = append([]string(nil), values...)
	}
	return out
}

func hopByHopHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length", "Host":
		return true
	}
	return false
}

// writeTunnelResponse mirrors an inline tunnel response onto the caller.
func writeTunnelResponse(w http.ResponseWriter, resp *nanoprotocol.TunnelResponse) {
	copyTunnelHeaders(w.Header(), resp.Headers)
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

func copyTunnelHeaders(dst http.Header, src map[string][]string) {
	for key, values := range src {
		if hopByHopHeader(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// relayErrorHeader marks an answer this relay produced itself, so the caller does
// not mistake it for the container's status (a container 404 means "no such
// file", a relay 404 means "no such container").
const relayErrorHeader = "X-Tunnel-Relay-Error"

func writeRelayError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set(relayErrorHeader, kind)
	writeJSON(w, status, map[string]string{"error": message})
}

// writeTunnelError keeps the two failures distinct for the caller: 404 means no
// container is online, 502 means it is online but the forward failed.
func writeTunnelError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errTunnelOffline):
		metrics.TunnelRequests.WithLabelValues("offline").Inc()
		writeRelayError(w, http.StatusNotFound, "offline", "tunnel is offline")
	case errors.Is(err, errTunnelBusy):
		metrics.TunnelRequests.WithLabelValues("busy").Inc()
		writeRelayError(w, http.StatusTooManyRequests, "busy",
			"too many requests in flight on this container; retry shortly")
	default:
		outcome := "failed"
		if strings.Contains(err.Error(), "timed out") {
			outcome = "timeout"
		}
		metrics.TunnelRequests.WithLabelValues(outcome).Inc()
		slog.Warn("forward tunnel request failed", "error", err)
		writeRelayError(w, http.StatusBadGateway, "failed", "tunnel forward failed")
	}
}
