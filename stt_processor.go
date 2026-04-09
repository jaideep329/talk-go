package main

import (
	"encoding/json"
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
	sessionCtx       *SessionContext
	websocketConn    *websocket.Conn
	transcriptFrames chan TranscriptFrame
	audioFrames      chan AudioFrame
	finalText        string // accumulated final tokens for live display
}

func (s *STTProcessor) connect() {
	for {
		conn, _, err := websocket.DefaultDialer.Dial("wss://stt-rt.soniox.com/transcribe-websocket", nil)
		if err != nil {
			s.sessionCtx.Logger.Printf("STT websocket connect failed: %v, retrying in 1s...", err)
			time.Sleep(time.Second)
			continue
		}
		config := map[string]interface{}{
			"api_key":                   os.Getenv("SONIOX_API_KEY"),
			"model":                     "stt-rt-v4",
			"audio_format":              "s16le",
			"sample_rate":               16000,
			"num_channels":              1,
			"language_hints":            []string{"hi"},
			"enable_endpoint_detection": true,
			"max_endpoint_delay_ms":     300,
		}
		if err := conn.WriteJSON(config); err != nil {
			s.sessionCtx.Logger.Printf("STT config send failed: %v, retrying in 1s...", err)
			conn.Close()
			time.Sleep(time.Second)
			continue
		}
		s.websocketConn = conn
		s.sessionCtx.Logger.Println("STT websocket connected")
		return
	}
}

func NewSTTProcessor(sessionCtx *SessionContext) *STTProcessor {
	p := &STTProcessor{
		sessionCtx:       sessionCtx,
		transcriptFrames: make(chan TranscriptFrame, 100),
		audioFrames:      make(chan AudioFrame, 100),
	}
	p.connect()
	return p
}

func (s *STTProcessor) readSTTWebsocket() {
	for {
		if s.sessionCtx.Ctx.Err() != nil {
			s.sessionCtx.Logger.Println("STT reader exiting: session closed")
			return
		}
		_, msg, err := s.websocketConn.ReadMessage()
		if err != nil {
			if s.sessionCtx.Ctx.Err() != nil {
				s.sessionCtx.Logger.Println("STT reader exiting: session closed")
				return
			}
			s.sessionCtx.Logger.Println("STT read error, reconnecting:", err)
			s.connect()
			continue
		}
		var resp SonioxResponseMessage
		if err := json.Unmarshal(msg, &resp); err != nil {
			s.sessionCtx.Logger.Println("STT json unmarshal error:", err)
			continue
		}

		// Build live transcript: finals accumulate, non-finals replace each response
		var nonFinalText string
		var hasEnd bool
		for _, token := range resp.Tokens {
			if token.IsFinal {
				if token.Text == "<end>" {
					hasEnd = true
				} else {
					s.finalText += token.Text
				}
			} else {
				nonFinalText += token.Text
			}
			s.transcriptFrames <- TranscriptFrame{Text: token.Text, IsFinal: token.IsFinal}
		}

		if hasEnd {
			s.sessionCtx.UIEvents.Send(UIEvent{Type: LiveTranscript, Data: map[string]interface{}{"text": ""}})
			s.finalText = ""
		} else if s.finalText != "" || nonFinalText != "" {
			s.sessionCtx.UIEvents.Send(UIEvent{Type: LiveTranscript, Data: map[string]interface{}{"text": s.finalText + nonFinalText}})
		}
	}
}

func (s *STTProcessor) writeAudioWebsocket() {
	for {
		select {
		case <-s.sessionCtx.Ctx.Done():
			s.sessionCtx.Logger.Println("STT writer exiting: session closed")
			s.websocketConn.Close()
			return
		case audioFrame := <-s.audioFrames:
			if err := s.websocketConn.WriteMessage(websocket.BinaryMessage, audioFrame.Data); err != nil {
				if s.sessionCtx.Ctx.Err() != nil {
					return
				}
				s.sessionCtx.Logger.Println("STT write error, skipping frame:", err)
				continue
			}
		}
	}
}

func (p *STTProcessor) Process(ch ProcessorChannels) {
	go p.readSTTWebsocket()
	go p.writeAudioWebsocket()
	for {
		select {
		case frame := <-ch.System:
			switch frame.(type) {
			case EndFrame:
				ch.Send(frame, Downstream)
				return
			}
		case frame, ok := <-ch.Data:
			if !ok {
				return
			}
			switch f := frame.(type) {
			case AudioFrame:
				p.audioFrames <- f
			default:
				p.sessionCtx.Logger.Printf("STT received unexpected frame: %T\n", f)
			}
		case frame := <-p.transcriptFrames:
			ch.Send(frame, Downstream)
		}
	}
}
