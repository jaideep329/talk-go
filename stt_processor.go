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
// variable so tests can override it to point at an unreachable URL.
var sttDialURL = "wss://stt-rt.soniox.com/transcribe-websocket"

// STTProcessor uses a single cancellation signal — the embedded
// BaseProcessor.ctx (referred to via s.ctx). Three things shut it down:
//
//   - s.Stop() called by ProcessFrame(EndFrame) when EndFrame propagates
//     through this processor.
//   - s.Stop() called by Pipeline.Stop() from PipelineTask.completeEnd.
//   - taskCtx.Ctx cancellation (transitively cancels s.ctx, since s.ctx
//     was derived from taskCtx.Ctx in NewBaseProcessor).
//
// Stop closes the websocket — which is what unblocks the reader's
// blocking ReadMessage — and cancels b.ctx, which unblocks the writer's
// select. Idempotent via closeOnce + BaseProcessor's atomic cancelling
// flag.
type STTProcessor struct {
	*BaseProcessor
	taskCtx       *TaskContext
	websocketConn *websocket.Conn
	audioFrames   chan AudioFrame
	connected     chan struct{} // closed when the websocket is established
	closeOnce     sync.Once
}

func NewSTTProcessor(taskCtx *TaskContext) *STTProcessor {
	p := &STTProcessor{
		taskCtx:     taskCtx,
		audioFrames: make(chan AudioFrame, 100),
		connected:   make(chan struct{}),
	}
	p.BaseProcessor = NewBaseProcessor("STT", p, taskCtx)
	return p
}

// Stop cancels b.ctx first (so the reader's post-ReadMessage ctx check
// fires before it tries to reconnect) and then closes the websocket
// (so the blocked ReadMessage returns). Order matters.
func (s *STTProcessor) Stop() {
	s.BaseProcessor.Stop()
	s.closeOnce.Do(func() {
		if s.websocketConn != nil {
			s.websocketConn.Close()
		}
	})
}

func (s *STTProcessor) Start(ctx context.Context) {
	s.BaseProcessor.Start(ctx)
	s.Go(s.runReader)
	s.Go(s.runWriter)
}

func sttConfigPayload() map[string]interface{} {
	return map[string]interface{}{
		"api_key":                   os.Getenv("SONIOX_API_KEY"),
		"model":                     "stt-rt-v4",
		"audio_format":              "s16le",
		"sample_rate":               16000,
		"num_channels":              1,
		"language_hints":            []string{"hi"},
		"enable_endpoint_detection": true,
		"max_endpoint_delay_ms":     300,
	}
}

// connect dials Soniox in an interruptible retry loop. Returns true
// once the connection is established and configured, false if b.ctx is
// cancelled first.
func (s *STTProcessor) connect() bool {
	for {
		if s.ctx.Err() != nil {
			return false
		}
		conn, _, err := websocket.DefaultDialer.Dial(sttDialURL, nil)
		if err == nil {
			err = conn.WriteJSON(sttConfigPayload())
			if err == nil {
				s.websocketConn = conn
				s.taskCtx.Logger.Println("STT websocket connected")
				return true
			}
			conn.Close()
		}
		s.taskCtx.Logger.Printf("STT connect failed: %v, retrying in 1s...", err)
		select {
		case <-time.After(time.Second):
		case <-s.ctx.Done():
			return false
		}
	}
}

func (s *STTProcessor) runReader() {
	if !s.connect() {
		return
	}
	close(s.connected)
	s.read()
}

func (s *STTProcessor) read() {
	responseID := 0
	for {
		if s.ctx.Err() != nil {
			s.taskCtx.Logger.Println("STT reader exiting")
			return
		}
		_, msg, err := s.websocketConn.ReadMessage()
		if err != nil {
			if s.ctx.Err() != nil {
				s.taskCtx.Logger.Println("STT reader exiting")
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
		for _, tok := range resp.Tokens {
			s.taskCtx.Logger.Printf("STT token received: response_id=%d finished=%v is_final=%v text=%q\n", responseID, resp.Finished, tok.IsFinal, tok.Text)
			s.PushFrame(NewTranscriptFrame(tok.Text, tok.IsFinal, responseID, resp.Finished), Downstream)
		}
	}
}

func (s *STTProcessor) runWriter() {
	select {
	case <-s.connected:
	case <-s.ctx.Done():
		return
	}
	for {
		select {
		case <-s.ctx.Done():
			return
		case audio := <-s.audioFrames:
			if err := s.websocketConn.WriteMessage(websocket.BinaryMessage, audio.Data); err != nil {
				if s.ctx.Err() != nil {
					return
				}
				s.taskCtx.Logger.Println("STT write error, skipping frame:", err)
			}
		}
	}
}

func (s *STTProcessor) ProcessFrame(ctx context.Context, frame Frame, dir Direction) {
	switch f := frame.(type) {
	case AudioFrame:
		select {
		case <-s.ctx.Done():
		case s.audioFrames <- f:
		}
	case EndFrame:
		s.taskCtx.Logger.Printf("EndFrame at STTProcessor: reason=%q\n", f.Reason)
		s.PushFrame(f, dir)
		s.Stop()
	default:
		s.PushFrame(frame, dir)
	}
}
