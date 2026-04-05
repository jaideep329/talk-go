package main

import (
	"encoding/json"
	"log"
	"os"
	"time"

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
	logger           *log.Logger
	websocketConn    *websocket.Conn
	transcriptFrames chan TranscriptFrame
	audioFrames      chan AudioFrame
	sessionCtx       *SessionContext
}

func (s *STTProcessor) connect() {
	for {
		conn, _, err := websocket.DefaultDialer.Dial("wss://stt-rt.soniox.com/transcribe-websocket", nil)
		if err != nil {
			s.logger.Printf("STT websocket connect failed: %v, retrying in 1s...", err)
			time.Sleep(time.Second)
			continue
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
			s.logger.Printf("STT config send failed: %v, retrying in 1s...", err)
			conn.Close()
			time.Sleep(time.Second)
			continue
		}
		s.websocketConn = conn
		s.logger.Println("STT websocket connected")
		return
	}
}

func NewSTTProcessor(logger *log.Logger, sessionCtx *SessionContext) *STTProcessor {
	p := &STTProcessor{
		logger:           logger,
		transcriptFrames: make(chan TranscriptFrame, 100),
		audioFrames:      make(chan AudioFrame, 100),
		sessionCtx:       sessionCtx,
	}
	p.connect()
	return p
}

func (s *STTProcessor) readSTTWebsocket() {
	for {
		if s.sessionCtx.Ctx.Err() != nil {
			s.logger.Println("STT reader exiting: session closed")
			return
		}
		_, msg, err := s.websocketConn.ReadMessage()
		if err != nil {
			if s.sessionCtx.Ctx.Err() != nil {
				s.logger.Println("STT reader exiting: session closed")
				return
			}
			s.logger.Println("STT read error, reconnecting:", err)
			s.connect()
			continue
		}
		var resp SonioxResponseMessage
		if err := json.Unmarshal(msg, &resp); err != nil {
			s.logger.Println("STT json unmarshal error:", err)
			continue
		}
		for _, token := range resp.Tokens {
			s.transcriptFrames <- TranscriptFrame{Text: token.Text, IsFinal: token.IsFinal}
		}
	}
}

func (s *STTProcessor) writeAudioWebsocket() {
	for {
		select {
		case <-s.sessionCtx.Ctx.Done():
			s.logger.Println("STT writer exiting: session closed")
			s.websocketConn.Close()
			return
		case audioFrame := <-s.audioFrames:
			if err := s.websocketConn.WriteMessage(websocket.BinaryMessage, audioFrame.Data); err != nil {
				if s.sessionCtx.Ctx.Err() != nil {
					return
				}
				s.logger.Println("STT write error, skipping frame:", err)
				continue
			}
		}
	}
}

func (p *STTProcessor) Process(in <-chan Frame, out chan<- Frame) {
	go p.readSTTWebsocket()
	go p.writeAudioWebsocket()
	for {
		select {
		case frame, ok := <-in:
			if !ok {
				return
			}
			switch f := frame.(type) {
			case AudioFrame:
				p.audioFrames <- f
			case EndFrame:
				return
			default:
				p.logger.Printf("STT received unexpected frame: %T\n", frame)
			}
		case frame := <-p.transcriptFrames:
			out <- frame
		}
	}
}
