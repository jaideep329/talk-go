package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

type UIEventType string

const (
	LiveTranscript     UIEventType = "live_transcript"
	UserTranscript     UIEventType = "user_transcript"
	CommittedAssistant UIEventType = "committed_assistant"
	AssistantSpeaking  UIEventType = "assistant_speaking"
	Latency            UIEventType = "latency"
)

// UIEvent is a typed message sent to the frontend over WebSocket.
// Type is an enum for compile-time safety. Data holds arbitrary payload.
type UIEvent struct {
	Type UIEventType            `json:"type"`
	Data map[string]interface{} `json:"data"`
}

// UIEventSender broadcasts UIEvents to connected WebSocket clients.
type UIEventSender struct {
	mu      sync.Mutex
	clients map[*websocket.Conn]bool
	logger  *log.Logger
}

func NewUIEventSender(logger *log.Logger) *UIEventSender {
	return &UIEventSender{
		clients: make(map[*websocket.Conn]bool),
		logger:  logger,
	}
}

func (s *UIEventSender) AddClient(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[conn] = true
}

func (s *UIEventSender) RemoveClient(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, conn)
	conn.Close()
}

// Send broadcasts a UIEvent to all connected clients.
func (s *UIEventSender) Send(event UIEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		s.logger.Println("UIEvent marshal error:", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for conn := range s.clients {
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			s.logger.Println("UIEvent write error:", err)
			delete(s.clients, conn)
			conn.Close()
		}
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}
