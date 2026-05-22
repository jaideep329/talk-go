package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/go-logr/stdr"
	"github.com/jaideep329/talk-go/voicepipelinecore"
	protoLogger "github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

var (
	sessions   = map[string]*voicepipelinecore.PipelineTask{}
	sessionsMu sync.Mutex
)

func main() {
	loadEnv(".env")
	appLog, _ := os.Create("app.log")
	log.SetOutput(io.MultiWriter(os.Stderr, appLog))
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	stdr.SetVerbosity(0)
	lksdk.SetLogger(protoLogger.LogRLogger(stdr.New(log.New(io.Discard, "", 0))))
	http.HandleFunc("/connect", handleConnect)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "livekit-client.html")
	})
	log.Println("HTTP server listening on :3000")
	if err := http.ListenAndServe(":3000", nil); err != nil {
		log.Fatal("HTTP server error:", err)
	}
}

func handleConnect(w http.ResponseWriter, r *http.Request) {
	logger := log.New(log.Writer(), "[room] ", log.Flags())
	task, err := voicepipelinecore.NewTask(context.Background(), voicepipelinecore.TaskOptions{
		Logger: logger,
	})
	if err != nil {
		log.Printf("failed to create task: %v", err)
		http.Error(w, "failed to create task", http.StatusInternalServerError)
		return
	}

	task.OnCleanup = func() {
		sessionsMu.Lock()
		delete(sessions, task.RoomName)
		sessionsMu.Unlock()
	}

	sessionsMu.Lock()
	sessions[task.RoomName] = task
	sessionsMu.Unlock()

	task.Start()

	token, err := voicepipelinecore.GenerateToken(task.RoomName, "web-user")
	if err != nil {
		log.Printf("failed to create token: %v", err)
		task.End(voicepipelinecore.EndReasonError)
		http.Error(w, "failed to create token", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"server_url": os.Getenv("LIVEKIT_URL"),
		"token":      token,
		"room_name":  task.RoomName,
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
