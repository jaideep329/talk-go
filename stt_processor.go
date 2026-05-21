package main

import (
	"context"
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

// sttDialURL is the Soniox websocket endpoint. Exposed as a package
// variable so tests can override it to point at an unreachable URL
// and avoid actually dialing Soniox.
var sttDialURL = "wss://stt-rt.soniox.com/transcribe-websocket"

type STTProcessor struct {
	*BaseProcessor
	taskCtx       *TaskContext
	websocketConn *websocket.Conn
	audioFrames   chan AudioFrame // ProcessFrame → writer goroutine
	connected     chan struct{}   // closed when websocketConn is established
	done          chan struct{}
	stopOnce      sync.Once
}

func NewSTTProcessor(taskCtx *TaskContext) *STTProcessor {
	p := &STTProcessor{
		taskCtx:     taskCtx,
		audioFrames: make(chan AudioFrame, 100),
		connected:   make(chan struct{}),
		done:        make(chan struct{}),
	}
	p.BaseProcessor = NewBaseProcessor("STT", p, taskCtx)
	return p
}

// connect dials Soniox in a retry loop and writes the initial config
// message. Returns true once a usable connection is established, false
// if shutdown was requested first.
func (s *STTProcessor) connect() bool {
	for {
		select {
		case <-s.done:
			return false
		case <-s.taskCtx.Ctx.Done():
			return false
		case <-s.ctx.Done():
			return false
		default:
		}
		conn, _, err := websocket.DefaultDialer.Dial(sttDialURL, nil)
		if err != nil {
			s.taskCtx.Logger.Printf("STT websocket connect failed: %v, retrying in 1s...", err)
			if !s.sleepOrExit(time.Second) {
				return false
			}
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
			s.taskCtx.Logger.Printf("STT config send failed: %v, retrying in 1s...", err)
			conn.Close()
			if !s.sleepOrExit(time.Second) {
				return false
			}
			continue
		}
		s.websocketConn = conn
		s.taskCtx.Logger.Println("STT websocket connected")
		return true
	}
}

// sleepOrExit sleeps for d, returning false if shutdown was requested
// before the sleep completed.
func (s *STTProcessor) sleepOrExit(d time.Duration) bool {
	select {
	case <-s.done:
		return false
	case <-s.taskCtx.Ctx.Done():
		return false
	case <-s.ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func (s *STTProcessor) Start(ctx context.Context) {
	s.BaseProcessor.Start(ctx)
	s.Go(s.runReader)
	s.Go(s.writeAudioWebsocket)
}

// runReader connects to Soniox, signals readiness, and then reads
// transcripts until shutdown. Spawned in a Go-tracked goroutine.
func (s *STTProcessor) runReader() {
	if !s.connect() {
		return
	}
	close(s.connected)
	s.readSTTWebsocket()
}

func (s *STTProcessor) stop() {
	s.stopOnce.Do(func() {
		s.taskCtx.Logger.Println("STTProcessor stopping websocket reader/writer")
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
			s.taskCtx.Logger.Println("STT reader exiting: processor ended")
			return
		default:
		}
		if s.taskCtx.Ctx.Err() != nil {
			s.taskCtx.Logger.Println("STT reader exiting: session closed")
			return
		}
		_, msg, err := s.websocketConn.ReadMessage()
		if err != nil {
			select {
			case <-s.done:
				s.taskCtx.Logger.Println("STT reader exiting: processor ended")
				return
			default:
			}
			if s.taskCtx.Ctx.Err() != nil {
				s.taskCtx.Logger.Println("STT reader exiting: session closed")
				return
			}
			s.taskCtx.Logger.Println("STT read error, reconnecting:", err)
			if !s.connect() {
				return
			}
			continue
		}
		var resp SonioxResponseMessage
		if err := json.Unmarshal(msg, &resp); err != nil {
			s.taskCtx.Logger.Println("STT json unmarshal error:", err)
			continue
		}
		responseID++
		s.taskCtx.Logger.Printf("STT response received: response_id=%d finished=%v tokens=%d\n", responseID, resp.Finished, len(resp.Tokens))

		for _, token := range resp.Tokens {
			s.taskCtx.Logger.Printf("STT token received: response_id=%d finished=%v is_final=%v text=%q\n", responseID, resp.Finished, token.IsFinal, token.Text)
			s.PushFrame(TranscriptFrame{Text: token.Text, IsFinal: token.IsFinal, ResponseID: responseID, Finished: resp.Finished}, Downstream)
		}
	}
}

func (s *STTProcessor) writeAudioWebsocket() {
	// Wait for the reader's connect() to succeed before sending audio.
	select {
	case <-s.connected:
	case <-s.done:
		return
	case <-s.taskCtx.Ctx.Done():
		return
	}
	for {
		select {
		case <-s.done:
			s.taskCtx.Logger.Println("STT writer exiting: processor ended")
			return
		case <-s.taskCtx.Ctx.Done():
			s.taskCtx.Logger.Println("STT writer exiting: session closed")
			s.websocketConn.Close()
			return
		case audioFrame := <-s.audioFrames:
			if err := s.websocketConn.WriteMessage(websocket.BinaryMessage, audioFrame.Data); err != nil {
				if s.taskCtx.Ctx.Err() != nil {
					return
				}
				s.taskCtx.Logger.Println("STT write error, skipping frame:", err)
				continue
			}
		}
	}
}

func (s *STTProcessor) ProcessFrame(ctx context.Context, frame Frame, dir Direction) {
	switch f := frame.(type) {
	case AudioFrame:
		select {
		case <-s.done:
		case s.audioFrames <- f:
		}
	case EndFrame:
		s.taskCtx.Logger.Printf("EndFrame at STTProcessor, forwarding downstream and stopping STT: reason=%q\n", f.Reason)
		s.PushFrame(f, dir)
		s.stop()
	default:
		s.PushFrame(frame, dir)
	}
}
