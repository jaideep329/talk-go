package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/livekit/protocol/auth"
)

type tokenRequest struct {
	RoomName string `json:"room_name"`
	Identity string `json:"identity"`
}

type tokenResponse struct {
	ServerURL string `json:"server_url"`
	Token     string `json:"token"`
}

func handleConnect(w http.ResponseWriter, r *http.Request) {
	initPipeline()

	req := tokenRequest{}

	if r.Method == http.MethodPost {
		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil {
			return
		}
		if req.RoomName == "" {
			req.RoomName = "default-room"
		}
		if req.Identity == "" {
			req.Identity = "web-user"
		}
	}

	apiKey := os.Getenv("LIVEKIT_API_KEY")
	apiSecret := os.Getenv("LIVEKIT_API_SECRET")
	serverURL := os.Getenv("LIVEKIT_URL")

	at := auth.NewAccessToken(apiKey, apiSecret)
	grant := &auth.VideoGrant{
		RoomJoin: true,
		Room:     req.RoomName,
	}
	at.SetVideoGrant(grant).
		SetIdentity(req.Identity).
		SetValidFor(10 * time.Minute)

	token, err := at.ToJWT()
	if err != nil {
		log.Printf("failed to create token: %v", err)
		http.Error(w, "failed to create token", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tokenResponse{
		ServerURL: serverURL,
		Token:     token,
	})
}
