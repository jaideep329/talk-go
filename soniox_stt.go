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
		"model":                     "stt-rt-preview",
		"audio_format":              "s16le",
		"sample_rate":               16000,
		"num_channels":              1,
		"language_hints":            []string{"en"},
		"enable_endpoint_detection": true,
	}
	if err := conn.WriteJSON(config); err != nil {
		log.Fatal("failed to send config:", err)
	}

	sttConn = conn
}

func readSTTWebsocketLoop(done chan struct{}) {
	defer close(done)
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

func writeAudioInToSTTWebsocket(done chan struct{}) {
	buf := make([]byte, 3200) // 100ms of audio at 16kHz, 16-bit mono
	for {
		n, err := audioInputStream.Read(buf)
		if err != nil {
			log.Println("audio stream ended:", err)
			break
		}
		chunk := buf[:n]
		if err := sttConn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
			log.Println("write error:", err)
			break
		}
	}
	sttConn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	<-done
}
