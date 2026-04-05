package main

import (
	"context"
	"encoding/json"
	"log"
	"os"

	"github.com/gorilla/websocket"
)

type SonioxToken struct {
	Text    string `json:"text"`
	IsFinal bool   `json:"is_final"`
}

type SonioxResponseMessage struct {
	Tokens   []SonioxToken `json:"tokens"`
	Finished bool          `json:"finished"`
}

type STTProcessor struct {
	websocketConn    *websocket.Conn
	transcriptFrames chan TranscriptFrame
	audioFrames      chan AudioFrame
}

func NewSTTProcessor() *STTProcessor {
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

	return &STTProcessor{
		websocketConn:    conn,
		transcriptFrames: make(chan TranscriptFrame, 100), // Buffered channel for transcript frames
		audioFrames:      make(chan AudioFrame, 100),      // Buffered channel for audio frames
	}
}

func (s *STTProcessor) readSTTWebsocket() {
	for {
		_, msg, err := s.websocketConn.ReadMessage()
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
			s.transcriptFrames <- TranscriptFrame{Text: token.Text, IsFinal: token.IsFinal}
		}
	}
}

func (s *STTProcessor) writeAudioWebsocket() {
	for {
		audioFrame := <-s.audioFrames
		if err := s.websocketConn.WriteMessage(websocket.BinaryMessage, audioFrame.Data); err != nil {
			log.Println("stt write error:", err)
			return
		}
	}
}

func (p *STTProcessor) Process(ctx context.Context, in <-chan Frame, out chan<- Frame) {
	go p.readSTTWebsocket()
	go p.writeAudioWebsocket()
	for {
		select {
		case frame := <-in:
			audioFrame, ok := frame.(AudioFrame)
			if !ok {
				log.Println("STTProcessor received non-audio frame")
				continue
			}
			p.audioFrames <- audioFrame
		case frame := <-p.transcriptFrames:
			out <- frame
		case <-ctx.Done():
			return
		}
	}

}
