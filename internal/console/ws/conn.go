package ws

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// writeWait bounds one frame write. A stuck TCP write must not pin a client
	// goroutine forever.
	writeWait = 10 * time.Second
	// pongWait is how long a client may stay silent. Two missed pings end it.
	pongWait = 60 * time.Second
	// pingPeriod is the server-side keepalive cadence from ADR-003; it must stay
	// comfortably below pongWait.
	pingPeriod = 30 * time.Second
	// maxMessageSize caps an inbound frame. Clients only ever send
	// subscribe/unsubscribe, so anything larger is a bug or an attack, and the
	// connection is closed instead of the frame being allocated.
	maxMessageSize = 4 << 10
)

// Buffer sizes for the upgrade. Outbound frames (full matrix snapshots) are much
// larger than inbound ones (two short strings).
const (
	readBufferSize  = 1024
	writeBufferSize = 4096
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  readBufferSize,
	WriteBufferSize: writeBufferSize,
	CheckOrigin:     checkOrigin,
}

// checkOrigin is the socket's CSRF defence.
func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// ServeWS upgrades one HTTP request with NO per-topic authorization.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	h.ServeWSAuthorized(w, r, nil)
}

// ServeWSAuthorized upgrades one HTTP request to the multiplexed WebSocket protocol and runs its
// two pumps; it is what lets ONE hub serve two sockets with different topic sets.
func (h *Hub) ServeWSAuthorized(w http.ResponseWriter, r *http.Request, authorize TopicAuthorizer) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written the HTTP error response (400 for a
		// non-WebSocket request, 403 for a rejected origin).
		slog.Debug("websocket upgrade rejected", //nolint:gosec // G706: RemoteAddr is filled in by net/http from the accepted socket, not by the client
			"error", err, "remote", r.RemoteAddr)
		return
	}

	c := h.register(authorize)
	slog.Debug("websocket client connected", "clients", h.ClientCount())

	// Teardown is symmetric in both directions; that also covers register refusing a client on an
	// already-stopped hub.
	go h.writePump(c, conn)
	h.readPump(c, conn)

	h.unregister(c)
	slog.Debug("websocket client disconnected", "clients", h.ClientCount())
}

// readPump consumes client frames until the socket fails. It owns the read
// deadline: every pong pushes it out by pongWait, so a client that stops
// answering pings is reaped rather than held forever.
func (h *Hub) readPump(c *client, conn *websocket.Conn) {
	conn.SetReadLimit(maxMessageSize)
	if err := conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		return
	}
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				slog.Debug("websocket read ended", "error", err)
			}
			return
		}

		var msg ClientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			// Malformed input is answered, not fatal: the socket carries N topics
			// and one bad frame must not cost a browser the other N-1.
			h.sendError(c, "", `malformed client message; expected {"action","topic","lastSeq"}`)
			continue
		}
		h.handleClientMessage(c, msg)
	}
}

// writePump is the only goroutine that ever writes to conn (gorilla permits one
// concurrent writer). It closes the socket on the way out, which is what unblocks
// the read pump.
func (h *Hub) writePump(c *client, conn *websocket.Conn) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		_ = conn.Close()
	}()

	for {
		select {
		case env := <-c.send:
			if err := conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				return
			}
			if err := conn.WriteJSON(env); err != nil {
				slog.Debug("websocket write failed", "topic", env.Topic, "error", err)
				return
			}
		case <-ticker.C:
			if err := conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				return
			}
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.done:
			// Best-effort close frame so the browser logs a clean close instead
			// of a reset; the deferred Close tears the socket down either way.
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			_ = conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseGoingAway, "server closing connection"))
			return
		}
	}
}
