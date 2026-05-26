package voicepipelinecore

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const botIdentity = "bot"

type dailyBridgeEvent struct {
	Event         string          `json:"event"`
	ParticipantID string          `json:"participant_id"`
	MeetingID     string          `json:"meeting_id"`
	Reason        string          `json:"reason"`
	Message       json.RawMessage `json:"message"`
	Data          string          `json:"data"`
	SampleRate    int             `json:"sample_rate"`
	Channels      int             `json:"channels"`
}

type dailyBridgeCommand struct {
	Type string      `json:"type"`
	Data interface{} `json:"data,omitempty"`
}

// DailyRoom owns the Daily media bridge process. Daily's official
// headless/server-side media SDK is Python, so Go keeps the pipeline and
// lifecycle while the bridge only joins Daily, relays PCM audio, and sends
// app messages.
type DailyRoom struct {
	roomURL     string
	roomName    string
	taskCtx     *TaskContext
	audioSource *AudioSourceProcessor
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	writeMu     sync.Mutex
	waitDone    chan struct{}
	closed      atomic.Bool
	joinOnce    sync.Once
	joinResult  chan error
}

func JoinDailyRoom(roomURL, token string, taskCtx *TaskContext, audioSource *AudioSourceProcessor) (*DailyRoom, error) {
	roomURL = strings.TrimSpace(roomURL)
	if roomURL == "" {
		return nil, errors.New("daily room url is required")
	}
	python := strings.TrimSpace(os.Getenv("DAILY_BRIDGE_PYTHON"))
	if python == "" {
		python = "python3"
	}
	script := strings.TrimSpace(os.Getenv("DAILY_BRIDGE_SCRIPT"))
	if script == "" {
		script = "daily_bridge.py"
	}
	if !filepath.IsAbs(script) {
		if wd, err := os.Getwd(); err == nil {
			script = filepath.Join(wd, script)
		}
	}

	cmd := exec.Command(python, script, "--room-url", roomURL, "--token", token, "--bot-name", "Chatbot")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("daily bridge stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("daily bridge stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("daily bridge stderr: %w", err)
	}

	room := &DailyRoom{
		roomURL:     roomURL,
		roomName:    dailyRoomNameFromURL(roomURL),
		taskCtx:     taskCtx,
		audioSource: audioSource,
		cmd:         cmd,
		stdin:       stdin,
		waitDone:    make(chan struct{}),
		joinResult:  make(chan error, 1),
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start daily bridge: %w", err)
	}
	room.goTracked(func() { room.readStdout(stdout) })
	room.goTracked(func() { room.readStderr(stderr) })
	room.goTracked(func() {
		err := cmd.Wait()
		if err != nil && !room.closed.Load() {
			room.log("Daily bridge exited: %v", err)
			room.finishJoin(fmt.Errorf("daily bridge exited before join: %w", err))
		}
		close(room.waitDone)
	})

	select {
	case err := <-room.joinResult:
		if err != nil {
			room.Disconnect()
			return nil, err
		}
	case <-time.After(20 * time.Second):
		room.Disconnect()
		return nil, errors.New("daily bridge join timed out")
	}
	return room, nil
}

func (r *DailyRoom) RoomURL() string {
	return r.roomURL
}

func (r *DailyRoom) RoomName() string {
	return r.roomName
}

func (r *DailyRoom) SendAppMessage(v interface{}) error {
	return r.writeCommand(dailyBridgeCommand{Type: "message", Data: v})
}

func (r *DailyRoom) WriteAudioPCM(pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	return r.writeCommand(dailyBridgeCommand{Type: "audio", Data: base64.StdEncoding.EncodeToString(pcm)})
}

func (r *DailyRoom) Disconnect() {
	if r == nil || !r.closed.CompareAndSwap(false, true) {
		return
	}
	_ = r.writeCommandAllowClosed(dailyBridgeCommand{Type: "leave"}, true)
	if r.stdin != nil {
		_ = r.stdin.Close()
	}
	select {
	case <-r.waitDone:
	case <-time.After(5 * time.Second):
		if r.cmd != nil && r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
		<-r.waitDone
	}
}

func (r *DailyRoom) writeCommand(cmd dailyBridgeCommand) error {
	return r.writeCommandAllowClosed(cmd, false)
}

func (r *DailyRoom) writeCommandAllowClosed(cmd dailyBridgeCommand, allowClosed bool) error {
	if r == nil || r.stdin == nil || r.closed.Load() {
		if !allowClosed {
			return nil
		}
	}
	if r == nil || r.stdin == nil {
		return nil
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	return json.NewEncoder(r.stdin).Encode(cmd)
}

func (r *DailyRoom) readStdout(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var event dailyBridgeEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			r.log("Daily bridge event decode error: %v", err)
			continue
		}
		r.handleEvent(event)
	}
	if err := scanner.Err(); err != nil && !r.closed.Load() {
		r.log("Daily bridge stdout error: %v", err)
	}
}

func (r *DailyRoom) readStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		r.log("Daily bridge: %s", scanner.Text())
	}
}

func (r *DailyRoom) handleEvent(event dailyBridgeEvent) {
	switch event.Event {
	case "joined":
		r.log("[%s] Bot joined Daily room meeting_id=%s participant_id=%s", r.roomName, event.MeetingID, event.ParticipantID)
		if r.taskCtx != nil && r.taskCtx.callEvents != nil {
			r.taskCtx.callEvents.fireBotJoined(time.Now())
		}
		r.finishJoin(nil)
	case "participant_joined":
		r.markUserJoined(event.ParticipantID)
	case "participant_left":
		r.markUserLeft(event.ParticipantID)
		r.log("[%s] Daily participant %q left, requesting EndFrame: %s", r.roomName, event.ParticipantID, event.Reason)
		r.endTask(EndReasonClientDisconnect)
	case "audio":
		raw, err := base64.StdEncoding.DecodeString(event.Data)
		if err != nil {
			r.log("Daily bridge audio decode error: %v", err)
			return
		}
		r.audioSource.PushPCM(raw, event.SampleRate, event.Channels)
	case "app_message":
		r.handleAppMessage(event.Message)
	case "left":
		r.log("[%s] Daily bridge left room", r.roomName)
	case "error":
		var message string
		if len(event.Message) > 0 {
			_ = json.Unmarshal(event.Message, &message)
		}
		if message == "" {
			message = "daily bridge error"
		}
		r.log("[%s] %s", r.roomName, message)
		r.finishJoin(errors.New(message))
	}
}

func (r *DailyRoom) handleAppMessage(raw json.RawMessage) {
	var msg struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		r.log("[%s] Daily app message malformed: %v", r.roomName, err)
		return
	}
	if msg.Type == "end_call" {
		r.log("[%s] UI requested end_call via Daily app message", r.roomName)
		r.endTask(EndReasonClientDisconnect)
	}
}

func (r *DailyRoom) markUserJoined(participantID string) {
	if participantID == "" || participantID == botIdentity {
		return
	}
	at := time.Now()
	r.log("[%s] Daily participant %q joined", r.roomName, participantID)
	if r.taskCtx == nil {
		return
	}
	if r.taskCtx.callStats != nil {
		r.taskCtx.callStats.MarkUserJoined(at)
	}
	if r.taskCtx.callEvents != nil {
		r.taskCtx.callEvents.fireUserJoined(at)
	}
}

func (r *DailyRoom) markUserLeft(participantID string) {
	if participantID == "" || participantID == botIdentity || r.taskCtx == nil || r.taskCtx.callStats == nil {
		return
	}
	r.taskCtx.callStats.MarkUserLeft(time.Now())
}

func (r *DailyRoom) endTask(reason EndReason) {
	if r.taskCtx != nil && r.taskCtx.EndTask != nil {
		r.taskCtx.EndTask(reason)
	}
}

func (r *DailyRoom) finishJoin(err error) {
	r.joinOnce.Do(func() {
		r.joinResult <- err
	})
}

func (r *DailyRoom) goTracked(fn func()) {
	if r.taskCtx != nil && r.taskCtx.wg != nil {
		r.taskCtx.wg.Add(1)
		go func() {
			defer r.taskCtx.wg.Done()
			fn()
		}()
		return
	}
	go fn()
}

func (r *DailyRoom) log(format string, args ...interface{}) {
	if r.taskCtx != nil && r.taskCtx.Logger != nil {
		r.taskCtx.Logger.Printf(format, args...)
	}
}

func dailyRoomNameFromURL(roomURL string) string {
	roomURL = strings.TrimRight(roomURL, "/")
	idx := strings.LastIndex(roomURL, "/")
	if idx < 0 || idx == len(roomURL)-1 {
		return roomURL
	}
	return roomURL[idx+1:]
}
