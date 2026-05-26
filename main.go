package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/stdr"
	"github.com/jaideep329/talk-go/disha"
	"github.com/jaideep329/talk-go/voicepipelinecore"
	protoLogger "github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

var (
	sessions   = map[string]*voicepipelinecore.PipelineTask{}
	sessionsMu sync.Mutex
	dishaDeps  disha.Deps
)

func main() {
	loadEnv(".env")
	appLog, _ := os.Create("app.log")
	log.SetOutput(io.MultiWriter(os.Stderr, appLog))
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	stdr.SetVerbosity(0)
	lksdk.SetLogger(protoLogger.LogRLogger(stdr.New(log.New(io.Discard, "", 0))))
	dishaDeps = newDishaDeps()
	defer closeDishaDeps(dishaDeps)
	http.HandleFunc("/connect", handleConnect)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "livekit-client.html")
	})
	log.Println("HTTP server listening on :3000")
	if err := http.ListenAndServe(":3000", nil); err != nil {
		log.Fatal("HTTP server error:", err)
	}
}

type connectRequest struct {
	ConversationID string `json:"conversation_id"`
	BotType        string `json:"bot_type"`
}

func handleConnect(w http.ResponseWriter, r *http.Request) {
	req := readConnectRequest(r)
	// A call outlives the HTTP request that created it. If the task is
	// derived from r.Context(), the pipeline is cancelled as soon as
	// /connect returns to Disha's Create Room request.
	task, err := buildConnectTask(context.Background(), req)
	if err != nil {
		log.Printf("failed to create task: %v", err)
		status := http.StatusInternalServerError
		if req.ConversationID != "" {
			status = http.StatusBadGateway
		}
		http.Error(w, "failed to create task", status)
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

func readConnectRequest(r *http.Request) connectRequest {
	req := connectRequest{
		ConversationID: strings.TrimSpace(r.URL.Query().Get("conversation_id")),
		BotType:        strings.TrimSpace(r.URL.Query().Get("bot_type")),
	}
	if r.Body != nil {
		var body connectRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			if body.ConversationID != "" {
				req.ConversationID = strings.TrimSpace(body.ConversationID)
			}
			if body.BotType != "" {
				req.BotType = strings.TrimSpace(body.BotType)
			}
		}
	}
	return req
}

func buildConnectTask(ctx context.Context, req connectRequest) (*voicepipelinecore.PipelineTask, error) {
	if req.ConversationID == "" {
		logger := log.New(log.Writer(), "[room] ", log.Flags())
		return voicepipelinecore.NewTask(context.Background(), voicepipelinecore.TaskOptions{
			Logger: logger,
		})
	}
	botType := req.BotType
	if botType == "" {
		botType = disha.SalesCallBotType
	}
	bot, err := disha.NewBot(botType)
	if err != nil {
		return nil, err
	}
	return disha.NewBotTask(ctx, bot, req.ConversationID, dishaDeps)
}

func newDishaDeps() disha.Deps {
	logger := log.New(log.Writer(), "[disha] ", log.Flags())
	return disha.Deps{
		Logger: logger,
		Redis: disha.NewRedisClient(
			os.Getenv("DISHA_REDIS_URL"),
			os.Getenv("DISHA_REDIS_PASSWORD"),
			redisDBFromEnv(),
			logger,
		),
		API: disha.NewAPIClient(os.Getenv("DISHA_API_URL"), 10*time.Second, logger),
	}
}

func closeDishaDeps(deps disha.Deps) {
	if deps.Redis != nil {
		if err := deps.Redis.Close(); err != nil {
			log.Printf("failed to close Disha Redis client: %v", err)
		}
	}
}

func redisDBFromEnv() int {
	raw := strings.TrimSpace(os.Getenv("REDIS_DB"))
	if raw == "" {
		return 0
	}
	db, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("invalid REDIS_DB=%q, using 0", raw)
		return 0
	}
	return db
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
