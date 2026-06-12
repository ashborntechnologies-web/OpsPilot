package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"
)

// Message is the envelope sent over WebSocket to the frontend.
type Message struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
}

// MessageHandler is implemented by the conversation service.
// Defined here to avoid a circular import (conversation imports ws).
type MessageHandler interface {
	ProcessMessage(ctx context.Context, projectID uuid.UUID, userID uuid.UUID, message string) (string, error)
}

// AuthFunc validates a bearer token for a specific project and returns the platform
// user ID. It must also verify the user owns projectID, returning an error otherwise,
// so a client cannot subscribe to another tenant's project stream. Injected at startup
// so the hub stays decoupled from the auth and models packages.
type AuthFunc func(ctx context.Context, token, projectID string) (uuid.UUID, error)

type client struct {
	conn *websocket.Conn
	// room is the broadcast key: a project ID for the chat/dashboard socket, or an
	// incident ID for a war-room socket. Broadcast(room, msg) reaches all clients here.
	room      string
	projectID string
	send      chan Message
	ctx       context.Context
	cancel    context.CancelFunc
	// limiter enforces per-connection message rate: 10 msg/min, burst 5.
	limiter *rate.Limiter
}

// Hub manages all active WebSocket connections, keyed by room (project ID or incident ID).
type Hub struct {
	mu            sync.RWMutex
	clients       map[string][]*client
	allowedOrigin string // FRONTEND_URL; empty = allow all (dev mode)
}

// RoomAuthFunc validates a bearer token for an arbitrary room ID (e.g. an incident ID)
// and returns the platform user ID, erroring if the user may not access that room.
type RoomAuthFunc func(ctx context.Context, token, roomID string) (uuid.UUID, error)

func NewHub(allowedOrigin string) *Hub {
	return &Hub{
		clients:       make(map[string][]*client),
		allowedOrigin: allowedOrigin,
	}
}

func (h *Hub) Run() {}

// HandleUpgrade upgrades an HTTP connection to WebSocket and starts the read/write loops.
// Authentication is performed via the first client message:
//
//	{"type":"auth","token":"<clerk_jwt>"}
//
// On success the server sends {"type":"auth_ok"} so the client knows the connection
// is live. The connection is closed immediately if the auth message is missing,
// malformed, or carries an invalid token.
func (h *Hub) HandleUpgrade(c *gin.Context, authFn AuthFunc, handler MessageHandler) {
	projectID := c.Param("projectId")
	pid, pidErr := uuid.Parse(projectID)

	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			if h.allowedOrigin == "" {
				return true // dev: no FRONTEND_URL set
			}
			return r.Header.Get("Origin") == h.allowedOrigin
		},
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		slog.Error(fmt.Sprintf("WebSocket upgrade error: %v", err))
		return
	}

	// ── First-message auth ──────────────────────────────────────────────────
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw, err := conn.ReadMessage()
	conn.SetReadDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return
	}

	var authMsg struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &authMsg); err != nil || authMsg.Type != "auth" || authMsg.Token == "" {
		conn.WriteMessage(websocket.TextMessage, mustMarshal(Message{Type: "error", Payload: "first message must be {type:auth,token:...}"}))
		conn.Close()
		return
	}

	userID, err := authFn(c.Request.Context(), authMsg.Token, projectID)
	if err != nil {
		conn.WriteMessage(websocket.TextMessage, mustMarshal(Message{Type: "error", Payload: "unauthorized"}))
		conn.Close()
		return
	}

	// ACK the auth so the client can safely set connected=true.
	conn.WriteMessage(websocket.TextMessage, mustMarshal(Message{Type: "auth_ok"}))
	// ────────────────────────────────────────────────────────────────────────

	ctx, cancel := context.WithCancel(context.Background())
	cl := &client{
		conn:      conn,
		room:      projectID,
		projectID: projectID,
		send:      make(chan Message, 64),
		ctx:       ctx,
		cancel:    cancel,
		limiter:   rate.NewLimiter(rate.Every(6*time.Second), 5), // 10 msg/min, burst 5
	}

	h.register(cl)
	defer h.unregister(cl)

	// Write goroutine — drains cl.send and writes to the connection
	go func() {
		for msg := range cl.send {
			data, _ := json.Marshal(msg)
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		}
	}()

	// Read loop
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var incoming struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(raw, &incoming); err != nil || incoming.Message == "" {
			continue
		}

		if !cl.limiter.Allow() {
			cl.safeSend(Message{Type: "error", Payload: "rate limit exceeded — please wait before sending another message"})
			continue
		}

		if pidErr != nil {
			cl.safeSend(Message{Type: "error", Payload: "invalid project ID"})
			continue
		}

		go func(msg string) {
			cl.safeSend(Message{Type: "thinking", Payload: ""})

			response, err := handler.ProcessMessage(cl.ctx, pid, userID, msg)
			if cl.ctx.Err() != nil {
				return
			}
			if err != nil {
				cl.safeSend(Message{Type: "error", Payload: err.Error()})
				return
			}
			// Use "response" to match the frontend WsMessage type switch.
			cl.safeSend(Message{Type: "response", Payload: response})
		}(incoming.Message)
	}
}

// HandleIncidentUpgrade upgrades a war-room WebSocket keyed by incident ID. Auth is the
// same first-message handshake as HandleUpgrade, but validated against the incident's org
// (authFn). The room is broadcast-only — engineers post updates over HTTP, not the socket
// — so the read loop only drains incoming frames to detect disconnect.
func (h *Hub) HandleIncidentUpgrade(c *gin.Context, authFn RoomAuthFunc) {
	incidentID := c.Param("incidentId")

	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return h.allowedOrigin == "" || r.Header.Get("Origin") == h.allowedOrigin
		},
	}
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		slog.Error(fmt.Sprintf("WebSocket upgrade error: %v", err))
		return
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw, err := conn.ReadMessage()
	conn.SetReadDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return
	}
	var authMsg struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &authMsg); err != nil || authMsg.Type != "auth" || authMsg.Token == "" {
		conn.WriteMessage(websocket.TextMessage, mustMarshal(Message{Type: "error", Payload: "first message must be {type:auth,token:...}"}))
		conn.Close()
		return
	}
	if _, err := authFn(c.Request.Context(), authMsg.Token, incidentID); err != nil {
		conn.WriteMessage(websocket.TextMessage, mustMarshal(Message{Type: "error", Payload: "unauthorized"}))
		conn.Close()
		return
	}
	conn.WriteMessage(websocket.TextMessage, mustMarshal(Message{Type: "auth_ok"}))

	ctx, cancel := context.WithCancel(context.Background())
	cl := &client{
		conn:    conn,
		room:    incidentID,
		send:    make(chan Message, 64),
		ctx:     ctx,
		cancel:  cancel,
		limiter: rate.NewLimiter(rate.Every(6*time.Second), 5),
	}
	h.register(cl)
	defer h.unregister(cl)

	go func() {
		for msg := range cl.send {
			data, _ := json.Marshal(msg)
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		}
	}()

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
}

func mustMarshal(m Message) []byte {
	b, _ := json.Marshal(m)
	return b
}

// Broadcast sends a message to all clients subscribed to a projectID.
func (h *Hub) Broadcast(projectID string, msg Message) {
	h.mu.RLock()
	clients := h.clients[projectID]
	h.mu.RUnlock()

	for _, cl := range clients {
		cl.safeSend(msg)
	}
}

func (h *Hub) register(cl *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[cl.room] = append(h.clients[cl.room], cl)
}

func (h *Hub) unregister(cl *client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	cl.cancel()

	clients := h.clients[cl.room]
	for i, c := range clients {
		if c == cl {
			h.clients[cl.room] = append(clients[:i], clients[i+1:]...)
			break
		}
	}
	close(cl.send)
	cl.conn.Close()
}

// safeSend sends to the client's channel without panicking if it's closed.
func (cl *client) safeSend(msg Message) {
	defer func() { recover() }()
	select {
	case cl.send <- msg:
	default: // buffer full — drop
	}
}
