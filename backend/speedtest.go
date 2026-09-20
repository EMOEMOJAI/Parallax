package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

func (s *Server) handleSpeedTestWS(w http.ResponseWriter, r *http.Request) {
	// Same single normalization point as /ws/client: a browser-supplied key
	// arrives as the "lg.bearer" subprotocol and is folded into Authorization
	// before any decision is taken.
	normalizeWSCredential(r)

	// Public sessions can't run the bandwidth test — it is unmetered egress
	// from the server itself. Refused before the upgrade so no connection is
	// established at all.
	if requestIsPublic(r) {
		writeJSONError(w, "not available in public mode", 403)
		return
	}
	// Apply same client auth as /ws/client if CLIENT_API_KEY is set. Unlike
	// /ws/client this stays an HTTP 401: the speed-test socket is opened by an
	// explicit user action, not a background reconnect loop, so there is no
	// retry storm to defuse and the panel can report the status directly.
	if !s.authClient(r) {
		http.Error(w, "unauthorized", 401)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, wsBearerResponseHeader(r))
	if err != nil {
		return
	}
	if !s.trackWebSocket(conn) {
		return
	}
	defer s.untrackWebSocket(conn)
	defer conn.Close()

	conn.SetReadLimit(16 * 1024 * 1024)

	// Set a read deadline to detect abandoned speed test connections
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		// Reset deadline on each message
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		var req struct {
			Action   string `json:"action"`
			Duration int    `json:"duration"`
		}
		if err := json.Unmarshal(msg, &req); err != nil {
			continue
		}

		switch req.Action {
		case "ping":
			s.handleSpeedTestPing(conn)
		case "download":
			s.handleSpeedTestDownload(conn, req.Duration)
		case "upload_start":
			s.handleSpeedTestUpload(conn)
		}
	}
}

func (s *Server) handleSpeedTestPing(conn *websocket.Conn) {
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	conn.WriteMessage(websocket.TextMessage, []byte(`{"action":"pong"}`))
	conn.SetWriteDeadline(time.Time{})
}

// speedTestChunk is a pre-allocated 1MB buffer reused across all download tests
// to avoid repeated heap allocation.
var speedTestChunk = func() []byte {
	b := make([]byte, 1024*1024)
	for i := range b {
		b[i] = byte(i % 256)
	}
	return b
}()

func (s *Server) handleSpeedTestDownload(conn *websocket.Conn, duration int) {
	if duration <= 0 || duration > 15 {
		duration = 5
	}

	conn.SetWriteDeadline(time.Now().Add(speedTestWriteTimeout))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"action":"download_start"}`)); err != nil {
		conn.SetWriteDeadline(time.Time{})
		return
	}
	conn.SetWriteDeadline(time.Time{})

	deadline := time.Now().Add(time.Duration(duration) * time.Second)
	totalBytes := 0
	const maxDownloadBytes = 100 * 1024 * 1024 // 100MB cap
	for time.Now().Before(deadline) && totalBytes < maxDownloadBytes {
		// Set write deadline to prevent blocking on slow clients
		conn.SetWriteDeadline(time.Now().Add(speedTestWriteTimeout))
		if err := conn.WriteMessage(websocket.BinaryMessage, speedTestChunk); err != nil {
			return
		}
		totalBytes += len(speedTestChunk)
	}
	// Set a deadline for the final result message too, to avoid blocking on stalled clients
	conn.SetWriteDeadline(time.Now().Add(speedTestWriteTimeout))

	result, _ := json.Marshal(map[string]any{
		"action":     "download_done",
		"totalBytes": totalBytes,
	})
	conn.WriteMessage(websocket.TextMessage, result)
	conn.SetWriteDeadline(time.Time{}) // clear deadline
	// Restore read deadline for the outer handleSpeedTestWS loop — the previous
	// read deadline may have expired while the server was busy writing chunks.
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
}

func (s *Server) handleSpeedTestUpload(conn *websocket.Conn) {
	totalBytes := 0
	const maxUploadBytes = 100 * 1024 * 1024 // 100MB cap to prevent abuse
	start := time.Now()
	// capElapsed records the time at which the cap was hit, so the speed
	// calculation excludes drain time (waiting for upload_done after cap).
	var capElapsed time.Duration
	// Set a read deadline to prevent indefinite blocking
	conn.SetReadDeadline(time.Now().Add(uploadPhaseTimeout))
	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if msgType == websocket.BinaryMessage {
			totalBytes += len(data)
			if totalBytes > maxUploadBytes {
				totalBytes = maxUploadBytes // clamp to cap for accurate calculation
				capElapsed = time.Since(start)
				// Cap reached — drain remaining messages until upload_done or timeout.
				// Reset read deadline since the original may be nearly expired.
				conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				for {
					mt, d, err := conn.ReadMessage()
					if err != nil {
						return
					}
					if mt == websocket.TextMessage {
						var sig struct {
							Action string `json:"action"`
						}
						if json.Unmarshal(d, &sig) == nil && sig.Action == "upload_done" {
							break
						}
					}
					// Discard binary data beyond the cap
				}
				break
			}
		} else {
			// Validate that the text message is the expected upload_done signal
			var sig struct {
				Action string `json:"action"`
			}
			if err := json.Unmarshal(data, &sig); err != nil || sig.Action != "upload_done" {
				continue // Ignore unexpected text messages
			}
			break
		}
	}

	conn.SetReadDeadline(time.Time{}) // clear deadline
	elapsed := capElapsed.Seconds()
	if elapsed <= 0 {
		// No cap was hit — use wall-clock time
		elapsed = time.Since(start).Seconds()
	}
	if elapsed <= 0 {
		elapsed = 0.001
	}
	speedMbps := float64(totalBytes) * 8 / elapsed / 1_000_000
	result, _ := json.Marshal(map[string]any{
		"action":     "upload_done",
		"totalBytes": totalBytes,
		"elapsed":    elapsed,
		"speedMbps":  speedMbps,
	})
	conn.SetWriteDeadline(time.Now().Add(speedTestWriteTimeout))
	conn.WriteMessage(websocket.TextMessage, result)
	conn.SetWriteDeadline(time.Time{})
	// Restore read deadline for the outer handleSpeedTestWS loop so it doesn't
	// block forever if the client goes silent after the upload phase.
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
}
