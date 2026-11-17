package server

// The streamed tunnel: a body larger than one frame is carried as chunks, and
// the size of a file stops being the size of a WebSocket message. These tests
// drive the real WebSocket both ways so the frame limit, the feature
// negotiation and the cancel path are exercised as they will be in production.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gitee.com/kobeyoung81/nanoprotocol"
	"github.com/gorilla/websocket"
)

// dialTunnelWithFeatures completes the handshake as a container that advertises
// the chunked-body extension.
func dialTunnelWithFeatures(t *testing.T, srv *httptest.Server, userID string, features ...string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/hypervisor?token=s3cret"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	register, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelRegister, nanoprotocol.TunnelRegister{
		UserID:   userID,
		Features: features,
	})
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
	return conn
}

func proxyURL(srv *httptest.Server, name, method, path string) string {
	query := url.Values{}
	query.Set("method", method)
	query.Set("path", path)
	return srv.URL + "/api/v1/internal/tunnels/" + name + "/proxy?" + query.Encode()
}

// answerStream reads one tunnel_request and answers with a chunked body.
func answerStream(t *testing.T, conn *websocket.Conn, status int, payload []byte, chunkSize int) {
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
	// A download has no body, so the request itself is inline; the answer is
	// what gets chunked.
	head, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelResponse, nanoprotocol.TunnelResponse{
		RequestID:  req.RequestID,
		Status:     status,
		Streamed:   true,
		BodyLength: int64(len(payload)),
	})
	if err != nil {
		t.Errorf("build head: %v", err)
		return
	}
	if err := conn.WriteJSON(head); err != nil {
		t.Errorf("write head: %v", err)
		return
	}
	seq := 0
	for offset := 0; offset < len(payload); offset += chunkSize {
		end := offset + chunkSize
		if end > len(payload) {
			end = len(payload)
		}
		chunk, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelResponseBody, nanoprotocol.TunnelBodyChunk{
			RequestID: req.RequestID,
			Seq:       seq,
			Data:      payload[offset:end],
			Final:     end == len(payload),
		})
		if err != nil {
			t.Errorf("build chunk: %v", err)
			return
		}
		if err := conn.WriteJSON(chunk); err != nil {
			t.Errorf("write chunk: %v", err)
			return
		}
		seq++
	}
}

// TestStreamedDownloadCarriesMoreThanOneFrame is the regression for the bug
// that started this: a 900 KiB body is 1.2 MB once base64'd, which the old
// one-frame tunnel could not carry, and the connection died with a 502.
func TestStreamedDownloadCarriesMoreThanOneFrame(t *testing.T) {
	s := newTunnelTestServer(t)
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	conn := dialTunnelWithFeatures(t, srv, "user-1", nanoprotocol.TunnelFeatureBodyStream)
	defer conn.Close()

	payload := bytes.Repeat([]byte("0123456789abcdef"), 900<<10/16)
	go answerStream(t, conn, http.StatusOK, payload, 200<<10)

	req, err := http.NewRequest(http.MethodPost, proxyURL(srv, "hypervisor-user-1", http.MethodGet, "/api/v1/files/a/big"), nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got bytes.Buffer
	if _, err := got.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got.Len() != len(payload) {
		t.Fatalf("body = %d bytes, want %d", got.Len(), len(payload))
	}
	if sha256.Sum256(got.Bytes()) != sha256.Sum256(payload) {
		t.Fatal("the streamed body differs from what the container sent")
	}
}

// TestStreamedUploadRefusesAnOldContainer: sending chunks to a hypervisor that
// never advertised the extension would lose the body silently, so a body past
// the inline cut-off is refused with a message that names the fix.
func TestStreamedUploadRefusesAnOldContainer(t *testing.T) {
	s := newTunnelTestServer(t)
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	conn := dialTunnel(t, srv, "user-2")
	defer conn.Close()

	payload := bytes.Repeat([]byte("x"), nanoprotocol.TunnelInlineBodyBytes+1)
	req, err := http.NewRequest(http.MethodPost, proxyURL(srv, "hypervisor-user-2", http.MethodPut, "/api/v1/files/a/big"), bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

// TestStreamedUploadCarriesTheWholeBody: the request direction is the same
// mechanism, and the container must receive every byte in order.
func TestStreamedUploadCarriesTheWholeBody(t *testing.T) {
	s := newTunnelTestServer(t)
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	conn := dialTunnelWithFeatures(t, srv, "user-3", nanoprotocol.TunnelFeatureBodyStream)
	defer conn.Close()

	payload := bytes.Repeat([]byte("upload"), (1<<20)/6)
	received := make(chan []byte, 1)
	go func() {
		var msg nanoprotocol.Message
		if err := conn.ReadJSON(&msg); err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		var req nanoprotocol.TunnelRequest
		if err := msg.DecodePayload(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		var body bytes.Buffer
		for {
			if err := conn.ReadJSON(&msg); err != nil {
				t.Errorf("read chunk: %v", err)
				return
			}
			if msg.Type != nanoprotocol.TypeTunnelRequestBody {
				t.Errorf("type = %q, want tunnel_request_body", msg.Type)
				return
			}
			var chunk nanoprotocol.TunnelBodyChunk
			if err := msg.DecodePayload(&chunk); err != nil {
				t.Errorf("decode chunk: %v", err)
				return
			}
			body.Write(chunk.Data)
			if chunk.Error != "" {
				t.Errorf("stream error: %s", chunk.Error)
				return
			}
			if chunk.Final {
				break
			}
		}
		received <- body.Bytes()

		resp, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelResponse, nanoprotocol.TunnelResponse{
			RequestID: req.RequestID,
			Status:    http.StatusOK,
			Body:      []byte(`{"status":"ok"}`),
		})
		if err != nil {
			return
		}
		_ = conn.WriteJSON(resp)
	}()

	req, err := http.NewRequest(http.MethodPost, proxyURL(srv, "hypervisor-user-3", http.MethodPut, "/api/v1/files/a/big"), bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	select {
	case got := <-received:
		if !bytes.Equal(got, payload) {
			t.Fatalf("container received %d bytes, want %d", len(got), len(payload))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the container never reported the body")
	}
}

// TestCallerLeavingCancelsTheContainer: a browser that stops a download must not
// leave the container reading a file nobody waits for.
func TestCallerLeavingCancelsTheContainer(t *testing.T) {
	s := newTunnelTestServer(t)
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	conn := dialTunnelWithFeatures(t, srv, "user-4", nanoprotocol.TunnelFeatureBodyStream)
	defer conn.Close()

	cancelled := make(chan string, 1)
	go func() {
		var msg nanoprotocol.Message
		if err := conn.ReadJSON(&msg); err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		var req nanoprotocol.TunnelRequest
		if err := msg.DecodePayload(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		head, err := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelResponse, nanoprotocol.TunnelResponse{
			RequestID: req.RequestID, Status: http.StatusOK, Streamed: true, BodyLength: 10 << 20,
		})
		if err != nil {
			return
		}
		if err := conn.WriteJSON(head); err != nil {
			t.Errorf("write head: %v", err)
			return
		}
		// Send one chunk, then stall: the caller gives up waiting.
		chunk, _ := nanoprotocol.NewMessage(nanoprotocol.TypeTunnelResponseBody, nanoprotocol.TunnelBodyChunk{
			RequestID: req.RequestID, Seq: 0, Data: []byte("first"),
		})
		if err := conn.WriteJSON(chunk); err != nil {
			return
		}
		for {
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			if msg.Type != nanoprotocol.TypeTunnelCancel {
				continue
			}
			var cancelledMsg nanoprotocol.TunnelCancel
			if err := msg.DecodePayload(&cancelledMsg); err != nil {
				return
			}
			cancelled <- cancelledMsg.RequestID
			return
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyURL(srv, "hypervisor-user-4", http.MethodGet, "/api/v1/files/a/big"), nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	// Read the status and the first chunk, then walk away.
	buf := make([]byte, 5)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	cancel()
	_ = resp.Body.Close()

	select {
	case id := <-cancelled:
		if id == "" {
			t.Fatal("the cancel carried no request id")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the container was never told to stop")
	}
}
