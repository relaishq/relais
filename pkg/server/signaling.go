package server

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/relais/pkg/webrtc"
)

// SignalingServer handles WebRTC signaling
type SignalingServer struct {
	upgrader   websocket.Upgrader
	sessionMgr *SessionManager
	webrtcMgr  *webrtc.PionAdapter
	clients    sync.Map
}

// NewSignalingServer creates a new signaling server
func NewSignalingServer(sessionMgr *SessionManager, webrtcMgr *webrtc.PionAdapter) *SignalingServer {
	return &SignalingServer{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true // In production, implement proper origin checks
			},
		},
		sessionMgr: sessionMgr,
		webrtcMgr:  webrtcMgr,
	}
}

// HandleWebSocket upgrades HTTP connection to WebSocket
func (s *SignalingServer) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, "Could not upgrade connection", http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	s.handleConnection(conn)
}

func (s *SignalingServer) handleConnection(conn *websocket.Conn) {
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var msg struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}

		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		response := s.handleSignalingMessage(msg)
		if err := conn.WriteMessage(messageType, response); err != nil {
			return
		}
	}
}

func (s *SignalingServer) handleSignalingMessage(msg struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
},
) []byte {
	// Handle different message types (offer, answer, ice candidate)
	// Implementation details...
	return nil
}
