package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/go-logr/stdr"
	protoLogger "github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

func main() {
	loadEnv(".env")
	appLog, _ := os.Create("app.log")
	log.SetOutput(io.MultiWriter(os.Stderr, appLog))
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	stdr.SetVerbosity(0)
	lksdk.SetLogger(protoLogger.LogRLogger(stdr.New(log.New(io.Discard, "", 0))))
	http.HandleFunc("/connect", handleConnect)
	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "livekit-client.html")
	})
	fmt.Println("HTTP server listening on :3000")
	if err := http.ListenAndServe(":3000", nil); err != nil {
		log.Fatal("HTTP server error:", err)
	}
}

func handleConnect(w http.ResponseWriter, r *http.Request) {
	roomName, _ := createSession()

	token, err := generateToken(roomName, "web-user")
	if err != nil {
		log.Printf("failed to create token: %v", err)
		http.Error(w, "failed to create token", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"server_url": os.Getenv("LIVEKIT_URL"),
		"token":      token,
		"room_name":  roomName,
	})
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	roomName := r.URL.Query().Get("room")
	session := getSession(roomName)
	if session == nil {
		http.Error(w, "unknown room", http.StatusNotFound)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	session.SessionCtx.UIEvents.AddClient(conn)
	// Keep connection alive — read until client disconnects
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			session.SessionCtx.UIEvents.RemoveClient(conn)
			session.Cancel()
			session.SessionCtx.Room.Disconnect()
			session.SessionCtx.Logger.Println("Session cleaned up: LiveKit disconnected, goroutines cancelled")
			removeSession(roomName)
			return
		}
	}
}

func loadEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatal("failed to read .env:", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			if err := os.Setenv(parts[0], parts[1]); err != nil {
				return
			}
		}
	}
}
