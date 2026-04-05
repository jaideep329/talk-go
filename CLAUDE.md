# talk-go — Project Context for Claude Code

## About the User

Jaideep is an experienced Python backend engineer learning Go by building real projects. He understands concurrency concepts (threading, asyncio) but is new to Go syntax, channels, goroutines, and Go idioms. He maps Go concepts to Python equivalents to build mental models.

## How to Collaborate

- **Guide step by step, don't dump full code.** Explain the "why" alongside small code snippets. Let him write the code himself unless he explicitly asks you to make changes directly.
- **When he says "make the changes directly" or similar, do it.** Otherwise default to guiding.
- **Explain Go concepts by mapping to Python equivalents** (e.g., channels = queue.Queue, goroutines = lightweight threads, defer = try/finally, interfaces = duck typing but explicit).
- **Keep responses concise.** He can read code — don't summarize what you just did.
- **Don't over-engineer.** He will push back if something is unnecessarily complex. Prefer the simplest solution first.
- **When debugging, check `app.log` first** — all `log.*` output goes there. `fmt.Print*` goes to stdout/terminal. Logs go to both `app.log` and terminal via `io.MultiWriter`.
- **After major design decisions or discussions, always update CLAUDE.md and memory.** Don't wait to be asked — if a design pattern, architecture choice, or strategy was decided in conversation, persist it immediately.

## Project Overview

A real-time voice assistant pipeline built in Go, using LiveKit for browser-based audio I/O:

```
Browser Mic → LiveKit Room → Go Bot (Opus decode → PCM) → Soniox STT → OpenAI LLM → Cartesia TTS → (PCM → Opus encode) → LiveKit Room → Browser Speaker
```

### Architecture

The Go server joins a LiveKit room as a "bot" participant. A browser client (`livekit-client.html`) joins the same room. The bot subscribes to the user's audio track, decodes Opus to PCM, sends it to Soniox for speech-to-text. When Soniox detects an endpoint (`<end>` token), the transcript is sent to OpenAI's streaming LLM. LLM tokens are accumulated into sentences (split on `.?!`), and each sentence is sent to Cartesia TTS. Cartesia returns raw PCM audio which is encoded to Opus and published back to the LiveKit room for the browser to play.

### Current State

The pipeline is **complete and working end-to-end** with barge-in, word timestamp tracking, multi-session support, websocket reconnection, a live UI, and session cleanup. Each `/connect` request creates an independent session.

#### Pipeline files:

| File | Purpose |
|------|---------|
| `main.go` | Entry point. Loads `.env`, configures logging (both terminal + `app.log`), silences LiveKit/pion logs, serves HTTP (`:3000`). `/connect` creates sessions, `/ws` streams UI events |
| `pipeline.go` | `Pipeline` struct with `Run()`. `SessionContext` (single dependency for all processors: Logger, Room, UIEvents, Ctx). `Session` struct for cleanup. `createSession()` wires everything |
| `frame.go` | Frame types: `Frame` interface with `IsSystem()` (not yet used). Concrete: `AudioFrame`, `TextFrame`, `TranscriptFrame`, `LLMResponseStartFrame`, `LLMResponseEndFrame`, `WordTimestampFrame`, `TTSDoneFrame`, `EndFrame`, `InterruptFrame` (unused) |
| `frame_processor.go` | `FrameProcessor` interface: `Process(in <-chan Frame, out chan<- Frame)` |
| `turn_context.go` | `TurnContext` — shared across LLM, TTS, PlaybackSink. Per-turn `context.WithCancel` for interrupt broadcast. Word accumulation (words appended as played). `PlaybackDone` channel for commit timing |
| `ui_events.go` | `UIEvent` struct, `UIEventSender` — per-session WebSocket broadcaster to frontend |
| `livekit_room.go` | `joinRoom(roomName, audioSource)` — bot joins LiveKit room. `generateToken()` creates JWT tokens |
| `livekit-client.html` | Browser client: Pico CSS + Alpine.js. Dark theme. Live transcript (user speaking), committed turns (user + assistant), latency badges, connect/disconnect/mute controls |
| `audio_source_processor.go` | Source processor. `SetTrack(track)` starts reader goroutine. Decodes Opus→PCM (16kHz mono) |
| `stt_processor.go` | Sends PCM to Soniox websocket, emits `TranscriptFrame`s. Builds live transcript for UI (finals accumulate, non-finals replace per response). Auto-reconnects |
| `llm_processor.go` | Accumulates final tokens, triggers `runLLM` on `<end>`. Streams OpenAI (gpt-4.1). Barge-in detection. Concatenates consecutive user messages when no assistant response between them. Commits spoken text on `PlaybackDone` or barge-in |
| `tts_processor.go` | Aggregates sentences, sends to Cartesia. PCM→Opus encoding. Interleaves `WordTimestampFrame`s with `AudioFrame`s based on audio time pushed (words emitted at correct playback position). Auto-reconnects |
| `playback_sink_processor.go` | Publishes LiveKit audio track. 20ms ticker pacing. On `WordTimestampFrame`: appends words to TurnContext + sends live `assistant_speaking` UI event. On `TTSDoneFrame`: marks playback done |

#### Pipeline wiring:
```go
AudioSourceProcessor → STTProcessor → LLMProcessor → TTSProcessor → PlaybackSinkProcessor
```

### Processor Initialization Pattern

All processors take `sessionCtx *SessionContext` as their first argument. Processors that need per-turn state also take `turnCtx *TurnContext`:

```go
NewAudioSourceProcessor(sessionCtx)
NewSTTProcessor(sessionCtx)
NewLLMProcessor(sessionCtx, turnCtx)
NewTTSProcessor(sessionCtx, turnCtx)
NewPlaybackSinkProcessor(sessionCtx, turnCtx)
```

`SessionContext` is the single session-level dependency containing Logger, Room, UIEvents, and Ctx (for session cancellation).

### Key Design Decisions

**SessionContext as single dependency:** All session-level concerns (logging, LiveKit room, UI events, cancellation) live in one struct. Eliminates per-processor parameter sprawl.

**TurnContext for cross-processor state (temporary — planned for removal):** Currently handles interrupt broadcast (`Cancel`/`Done`), word accumulation (`AppendWords`), and playback completion (`MarkPlaybackDone`/`PlaybackDone`). Will be replaced by frame-based communication in the upcoming refactor.

**Word timestamp interleaving in TTS:** TTS tracks `audioTimePushed` (seconds) and buffers `pendingWords` from Cartesia timestamp messages. After each opus frame is pushed, words whose `start <= audioTimePushed` are emitted as `WordTimestampFrame`. This means words arrive at PlaybackSink in the correct playback order — no time-based calculation needed on the receiving end.

**Commit timing:** Assistant text is committed to LLM history when `TTSDoneFrame` reaches PlaybackSink (normal completion) or on barge-in (partial text via `SpokenSoFar()`). NOT deferred to next turn.

**User message concatenation:** If the user pauses mid-thought (Soniox fires `<end>`) and the LLM gets barged in before responding, the next user utterance is concatenated with the previous one in the messages array rather than creating separate entries.

**Live transcript (Soniox):** STT processor builds live transcript per response — final tokens accumulate, non-final tokens replace on each response (Soniox semantics). Sent as `live_transcript` UI event. Cleared on `<end>`.

**UI shows only committed state:** User turns appear on `user_transcript` (final). Assistant turns appear on `committed_assistant` (after playback done or barge-in). Live assistant speech shown via `assistant_speaking` events from PlaybackSink as words are played.

**Session cleanup:** When UI WebSocket disconnects, session context is cancelled (stops STT/TTS background goroutines), LiveKit room is disconnected, session is removed from map.

**Cartesia TTS context strategy:** Single `context_id` per LLM turn. All sentences sent with `"continue": true` and `"add_timestamps": true`. Final flush with `"continue": false`. On interrupt, sends `{"cancel": true}`.

### Planned Refactor: Pipecat-Style Frame Communication

The next major change eliminates `TurnContext` and adopts Pipecat's frame-only communication model:

**New ProcessorChannels (replaces current `Process(in, out)`):**
```go
type ProcessorChannels struct {
    Data   <-chan Frame                       // normal priority input
    System <-chan Frame                       // high priority input (checked first)
    Send   func(frame Frame, dir Direction)   // output anything, any direction
}
```

**Key changes:**
- **Two input channels with priority:** System channel checked first in select (nested select trick). `InterruptionFrame` goes on system channel — bypasses buffered audio. Eliminates need for `TurnContext.Cancel()/Done()`.
- **Upstream frame flow:** `Send(frame, Upstream)` sends frames backwards through pipeline. PlaybackSink sends `WordTimestampFrame` and `TTSDoneFrame` upstream to LLM. Eliminates need for `TurnContext.AppendWords()` and `PlaybackDone()`.
- **`Send` function injected by pipeline:** Routes based on `frame.IsSystem()` and direction. Processor doesn't think about output channels.
- **Frame type hierarchy:** `IsSystem()` on Frame interface determines routing priority. `InterruptionFrame`, `EndFrame` are system frames.
- **LLM as aggregator:** LLM accumulates words from upstream `WordTimestampFrame`s internally. Commits on `InterruptionFrame` or upstream `TTSDoneFrame`. No shared word tracking object.

This enables: function calls (upstream result flow), custom processors (just implement `Process(ProcessorChannels)`), and dynamic pipeline composition without editing core infrastructure.

### Key Technical Details

- **Audio formats**: Soniox expects s16le/16kHz/mono. Cartesia outputs s16le/24kHz/mono. LiveKit/WebRTC uses Opus at 48kHz clock rate (always signaled as stereo in SDP).
- **Opus decode**: `opus.NewDecoder(16000, 1)` — decodes directly to 16kHz for Soniox.
- **Opus encode**: `opus.NewEncoder(24000, 1, opus.AppVoIP)` — encodes 24kHz Cartesia PCM. Frame size: 480 samples (20ms at 24kHz) = 960 bytes.
- **LiveKit track publish**: `ClockRate: 48000, Channels: 2` in RTP capability (WebRTC requirement), even though actual audio is 24kHz mono.
- **Pacing**: Opus frames must be sent at real-time pace (one 20ms frame per tick) or the browser jitter buffer overflows and drops audio.
- **Websocket idle timeouts**: Soniox and Cartesia close connections after ~15-20s of no data. Pipeline is initialized lazily on client connect.
- **Track re-subscription**: LiveKit fires `onTrackSubscribed` multiple times during renegotiation. `AudioSourceProcessor.SetTrack()` handles this cleanly without pipeline teardown.
- **Concurrent websocket writes**: gorilla/websocket panics on concurrent writes. Ensure only one goroutine writes to each websocket at a time.

### Frontend (livekit-client.html)

Single HTML file using Pico CSS (dark theme) + Alpine.js (reactivity). No build step.

- **Live transcript bar:** Shows what user is saying (from `live_transcript` events) and what assistant is speaking (from `assistant_speaking` events). Clears on finalization.
- **Committed turns:** User turns appear on `user_transcript` event. Assistant turns appear on `committed_assistant` event with latency badge.
- **Latency:** Buffered as `pendingLatency`, attached to assistant turn when committed.
- **LiveKit objects stored outside Alpine proxy** (`_room`, `_ws`) to avoid `structuredClone` errors.
- **LiveKit SDK lazy-loaded** via dynamic `import()` on first connect.

### Environment Variables (.env)

```
SONIOX_API_KEY=...
OPENAI_API_KEY=...
CARTESIA_API_KEY=...
LIVEKIT_API_KEY=...
LIVEKIT_API_SECRET=...
LIVEKIT_URL=wss://...
```

### Dependencies

- `github.com/gorilla/websocket` — websocket client for Soniox and Cartesia
- `github.com/hraban/opus` — CGO wrapper around libopus (requires `brew install opus opusfile pkg-config`)
- `github.com/livekit/server-sdk-go/v2` — LiveKit server SDK for joining rooms as a bot
- `github.com/livekit/protocol/auth` — LiveKit JWT token generation
- `github.com/pion/webrtc/v4` — WebRTC types (`TrackRemote`, `RTPCodecCapability`)
- `github.com/go-logr/stdr` — Used to silence LiveKit/pion debug logs

### Build & Run

```bash
brew install opus opusfile pkg-config  # one-time
go build ./...
./awesomeProject  # or: go run .
# Open http://localhost:3000, click Connect
```

### Known Issues / Gotchas

- Audio can sound choppy if Opus frames aren't paced at 20ms intervals — use `time.Ticker`, not `time.Sleep`
- The go.mod module name is still `awesomeProject` (from the original learning project)
- `InterruptFrame` in `frame.go` is unused (replaced by `TurnContext`) — will be repurposed in the refactor
