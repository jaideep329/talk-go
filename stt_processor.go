package main

import (
	"encoding/json"
	"os"
	"sync"
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
	done             chan struct{}
	stopOnce         sync.Once
}

func (s *STTProcessor) connect() bool {
	for {
		select {
		case <-s.done:
			return false
		case <-s.sessionCtx.Ctx.Done():
			return false
		default:
		}
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
		return true
	}
}

func NewSTTProcessor(sessionCtx *SessionContext) *STTProcessor {
	p := &STTProcessor{
		sessionCtx:       sessionCtx,
		transcriptFrames: make(chan TranscriptFrame, 100),
		audioFrames:      make(chan AudioFrame, 100),
		done:             make(chan struct{}),
	}
	p.connect()
	return p
}

func (s *STTProcessor) stop() {
	s.stopOnce.Do(func() {
		s.sessionCtx.Logger.Println("STTProcessor stopping websocket reader/writer")
		close(s.done)
		if s.websocketConn != nil {
			s.websocketConn.Close()
		}
	})
}

func (s *STTProcessor) readSTTWebsocket() {
	responseID := 0
	for {
		select {
		case <-s.done:
			s.sessionCtx.Logger.Println("STT reader exiting: processor ended")
			return
		default:
		}
		if s.sessionCtx.Ctx.Err() != nil {
			s.sessionCtx.Logger.Println("STT reader exiting: session closed")
			return
		}
		_, msg, err := s.websocketConn.ReadMessage()
		if err != nil {
			select {
			case <-s.done:
				s.sessionCtx.Logger.Println("STT reader exiting: processor ended")
				return
			default:
			}
			if s.sessionCtx.Ctx.Err() != nil {
				s.sessionCtx.Logger.Println("STT reader exiting: session closed")
				return
			}
			s.sessionCtx.Logger.Println("STT read error, reconnecting:", err)
			if !s.connect() {
				return
			}
			continue
		}
		var resp SonioxResponseMessage
		if err := json.Unmarshal(msg, &resp); err != nil {
			s.sessionCtx.Logger.Println("STT json unmarshal error:", err)
			continue
		}
		responseID++
		s.sessionCtx.Logger.Printf("STT response received: response_id=%d finished=%v tokens=%d\n", responseID, resp.Finished, len(resp.Tokens))

		for _, token := range resp.Tokens {
			s.sessionCtx.Logger.Printf("STT token received: response_id=%d finished=%v is_final=%v text=%q\n", responseID, resp.Finished, token.IsFinal, token.Text)
			select {
			case <-s.done:
				s.sessionCtx.Logger.Println("STT reader exiting: processor ended")
				return
			case s.transcriptFrames <- TranscriptFrame{Text: token.Text, IsFinal: token.IsFinal, ResponseID: responseID, Finished: resp.Finished}:
			}
		}
	}
}

func (s *STTProcessor) writeAudioWebsocket() {
	for {
		select {
		case <-s.done:
			s.sessionCtx.Logger.Println("STT writer exiting: processor ended")
			return
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
			ch.Send(frame, Downstream)
		case frame, ok := <-ch.Data:
			if !ok {
				return
			}
			switch f := frame.(type) {
			case AudioFrame:
				select {
				case <-p.done:
					return
				case p.audioFrames <- f:
				}
			case EndFrame:
				p.sessionCtx.Logger.Printf("EndFrame at STTProcessor data path, forwarding downstream and stopping STT: reason=%q\n", f.Reason)
				ch.Send(f, Downstream)
				p.stop()
				return
			default:
				p.sessionCtx.Logger.Printf("STT received unexpected frame: %T\n", f)
			}
		case frame := <-p.transcriptFrames:
			ch.Send(frame, Downstream)
		}
	}
}
