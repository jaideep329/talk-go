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
	stdr.SetVerbosity(0)
	lksdk.SetLogger(protoLogger.LogRLogger(stdr.New(log.New(io.Discard, "", 0))))
	http.HandleFunc("/connect", handleConnect)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "livekit-client.html")
	})
	fmt.Println("HTTP server listening on :3000")
	if err := http.ListenAndServe(":3000", nil); err != nil {
		log.Fatal("HTTP server error:", err)
	}
}

func handleConnect(w http.ResponseWriter, r *http.Request) {
	initPipeline()

	roomName := "default-room"
	identity := "web-user"

	if r.Method == http.MethodPost {
		var req struct {
			RoomName string `json:"room_name"`
			Identity string `json:"identity"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			if req.RoomName != "" {
				roomName = req.RoomName
			}
			if req.Identity != "" {
				identity = req.Identity
			}
		}
	}

	token, err := generateToken(roomName, identity)
	if err != nil {
		log.Printf("failed to create token: %v", err)
		http.Error(w, "failed to create token", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"server_url": os.Getenv("LIVEKIT_URL"),
		"token":      token,
	})
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
