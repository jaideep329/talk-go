package main

import (
	"encoding/json"
	"log"
	"os"

	"github.com/gorilla/websocket"
)

var sttConn *websocket.Conn

type SonioxToken struct {
	Text    string `json:"text"`
	IsFinal bool   `json:"is_final"`
}

type SonioxResponseMessage struct {
	Tokens   []SonioxToken `json:"tokens"`
	Finished bool          `json:"finished"`
}

func initializeSonioxWebsocket() {
	conn, _, err := websocket.DefaultDialer.Dial("wss://stt-rt.soniox.com/transcribe-websocket", nil)
	if err != nil {
		log.Fatal("websocket connect failed:", err)
	}
	config := map[string]interface{}{
		"api_key":                   os.Getenv("SONIOX_API_KEY"),
		"model":                     "stt-rt-v4",
		"audio_format":              "s16le",
		"sample_rate":               16000,
		"num_channels":              1,
		"language_hints":            []string{"en"},
		"enable_endpoint_detection": true,
		"max_endpoint_delay_ms":     300,
	}
	if err := conn.WriteJSON(config); err != nil {
		log.Fatal("failed to send config:", err)
	}

	sttConn = conn
}

func readSTTWebsocketLoop() {
	var transcript string
	for {
		_, msg, err := sttConn.ReadMessage()
		if err != nil {
			log.Println("read error:", err)
			return
		}
		var resp SonioxResponseMessage
		if err := json.Unmarshal(msg, &resp); err != nil {
			log.Println("json unmarshal error:", err)
			continue
		}
		for _, token := range resp.Tokens {
			if token.IsFinal {
				if token.Text == "<end>" {
					println("[You]: " + transcript)
					callLLM(transcript)
					transcript = ""
				} else {
					transcript += token.Text
				}

			}
		}
	}
}
