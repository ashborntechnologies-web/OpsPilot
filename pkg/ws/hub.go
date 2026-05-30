package ws

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/convdeploy/platform/pkg/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
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

type client struct {
	conn      *websocket.Conn
	projectID string
	send      chan Message
	ctx       context.Context
	cancel    context.CancelFunc
}

// Hub manages all active WebSocket connections, keyed by projectID.
type Hub struct {
	mu      sync.RWMutex
	clients map[string][]*client
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // TODO: validate origin in production
	},
}

func NewHub() *Hub {
	return &Hub{clients: make(map[string][]*client)}
}

func (h *Hub) Run() {}

// HandleUpgrade upgrades an HTTP connection to WebSocket and starts the read/write loops.
func (h *Hub) HandleUpgrade(c *gin.Context, handler MessageHandler) {
	projectID := c.Param("projectId")

	userID, _ := middleware.GetUserID(c)

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	cl := &client{
		conn:      conn,
		projectID: projectID,
		send:      make(chan Message, 64),
		ctx:       ctx,
		cancel:    cancel,
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

	// Read loop — one goroutine per connected client
	pid, pidErr := uuid.Parse(projectID)

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

		if pidErr != nil {
			cl.safeSend(Message{Type: "error", Payload: "invalid project ID"})
			continue
		}

		// Process in a goroutine so the read loop stays unblocked for future messages
		go func(msg string) {
			cl.safeSend(Message{Type: "thinking", Payload: ""})

			response, err := handler.ProcessMessage(cl.ctx, pid, userID, msg)
			if cl.ctx.Err() != nil {
				return // connection dropped while processing — discard
			}
			if err != nil {
				cl.safeSend(Message{Type: "error", Payload: err.Error()})
				return
			}
			cl.safeSend(Message{Type: "assistant_message", Payload: response})
		}(incoming.Message)
	}
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
	h.clients[cl.projectID] = append(h.clients[cl.projectID], cl)
}

func (h *Hub) unregister(cl *client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	cl.cancel() // signal in-flight goroutines to stop

	clients := h.clients[cl.projectID]
	for i, c := range clients {
		if c == cl {
			h.clients[cl.projectID] = append(clients[:i], clients[i+1:]...)
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
