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
| `pipeline.go` | `Pipeline` struct with `Run()` — creates per-processor data/system channels, injects `Send` function for routing. `SessionContext` (single dependency for all processors: Logger, Room, UIEvents, Ctx). `Session` struct for cleanup. `createSession()` wires everything |
| `frame.go` | Frame types: `Frame` interface with `IsSystem()` for routing priority. Concrete: `AudioFrame`, `TextFrame`, `TranscriptFrame`, `LLMResponseStartFrame` (carries `StartedAt` timestamp), `LLMResponseEndFrame`, `WordTimestampFrame`, `TTSDoneFrame`, `EndFrame`, `InterruptFrame`. System frames: `InterruptFrame`, `EndFrame` |
| `frame_processor.go` | `Direction` (Downstream/Upstream), `ProcessorChannels` (Data, System channels + Send function), `FrameProcessor` interface: `Process(ch ProcessorChannels)` |
| `ui_events.go` | `UIEvent` struct, `UIEventSender` — per-session WebSocket broadcaster to frontend |
| `livekit_room.go` | `joinRoom(roomName, audioSource)` — bot joins LiveKit room. `generateToken()` creates JWT tokens |
| `livekit-client.html` | Browser client: Pico CSS + Alpine.js. Dark theme. Live transcript (user speaking), committed turns (user + assistant), latency badges, connect/disconnect/mute controls |
| `audio_source_processor.go` | Source processor. `SetTrack(track)` starts reader goroutine. Decodes Opus→PCM (16kHz mono) |
| `stt_processor.go` | Sends PCM to Soniox websocket, emits `TranscriptFrame`s. Builds live transcript for UI (finals accumulate, non-finals replace per response). Auto-reconnects |
| `llm_processor.go` | Accumulates final tokens, triggers `runLLM` on `<end>`. Streams OpenAI (gpt-4.1). Barge-in detection (sends `InterruptFrame` downstream). Concatenates consecutive user messages when no assistant response between them. Accumulates words internally from upstream `WordTimestampFrame`s. Commits spoken text on upstream `TTSDoneFrame` or barge-in |
| `tts_processor.go` | Aggregates sentences, sends to Cartesia. PCM→Opus encoding. Interleaves `WordTimestampFrame`s with `AudioFrame`s based on audio time pushed (words emitted at correct playback position). Handles `InterruptFrame` on system channel. Forwards upstream frames (WordTimestamp, TTSDone) from PlaybackSink to LLM. Auto-reconnects |
| `playback_sink_processor.go` | Publishes LiveKit audio track. 20ms ticker pacing. On `WordTimestampFrame`: sends `assistant_speaking` UI event + forwards upstream to LLM. On `TTSDoneFrame`: forwards upstream to LLM for commit. Handles `InterruptFrame` on system channel |

#### Pipeline wiring:
```go
AudioSourceProcessor → STTProcessor → LLMProcessor → TTSProcessor → PlaybackSinkProcessor
```

### Processor Initialization Pattern

All processors take only `sessionCtx *SessionContext`. No shared per-turn state — all cross-processor communication happens via frames.

```go
NewAudioSourceProcessor(sessionCtx)
NewSTTProcessor(sessionCtx)
NewLLMProcessor(sessionCtx)
NewTTSProcessor(sessionCtx)
NewPlaybackSinkProcessor(sessionCtx)
```

`SessionContext` is the single session-level dependency containing Logger, Room, UIEvents, and Ctx (for session cancellation).

### Key Design Decisions

**Pipecat-style frame-only communication (no shared state):** All cross-processor communication happens via frames flowing through channels. No shared mutable objects between processors. Each processor is self-contained — receives frames on its channels, sends frames via its `Send` function.

**ProcessorChannels pattern:** Every processor receives a `ProcessorChannels` struct with two input channels (Data, System) and a `Send` function. The pipeline creates these and injects routing logic:
```go
type ProcessorChannels struct {
    Data   <-chan Frame    // normal priority input (from both upstream and downstream neighbors)
    System <-chan Frame    // high priority input (InterruptFrame, EndFrame) — checked first
    Send   func(frame Frame, dir Direction)  // routes to next/previous processor
}
```
- `Send(frame, Downstream)` → routes to next processor's System channel (if `frame.IsSystem()`) or Data channel
- `Send(frame, Upstream)` → routes to previous processor's Data channel

**System channel priority (nested select trick):** Processors that handle InterruptFrame use a nested select: outer select checks System channel first with a `default` fallback to an inner select that checks both System and Data. This ensures system frames (interrupts) bypass any buffered data frames.

**SessionContext as single dependency:** All session-level concerns (logging, LiveKit room, UI events, cancellation) live in one struct. Eliminates per-processor parameter sprawl.

**Bidirectional frame flow:** Frames flow downstream (AudioSource → PlaybackSink) and upstream (PlaybackSink → LLM). PlaybackSink sends `WordTimestampFrame` and `TTSDoneFrame` upstream. TTS forwards upstream frames it doesn't own. LLM accumulates words internally and commits on `TTSDoneFrame` or barge-in.

**InterruptFrame for barge-in:** LLM detects barge-in (receives TranscriptFrame while streaming), cancels its HTTP request, and sends `InterruptFrame` downstream. TTS receives it on system channel (priority), cancels Cartesia context, drains buffered audio, resets state, and forwards downstream. PlaybackSink receives it and stops playback. No shared cancel context needed.

**LLMResponseStartFrame carries timestamp:** `StartedAt time.Time` is set by LLM when the turn begins. PlaybackSink uses it for accurate E2E latency measurement (measures from LLM turn start to first audio frame played).

**Word timestamp interleaving in TTS:** TTS tracks `audioTimePushed` (seconds) and buffers `pendingWords` from Cartesia timestamp messages. After each opus frame is pushed, words whose `start <= audioTimePushed` are emitted as `WordTimestampFrame`. This means words arrive at PlaybackSink in the correct playback order — no time-based calculation needed on the receiving end.

**Commit timing:** Assistant text is committed to LLM history when `TTSDoneFrame` flows upstream from PlaybackSink through TTS to LLM (normal completion) or on barge-in (partial text from internally accumulated words). NOT deferred to next turn.

**User message concatenation:** If the user pauses mid-thought (Soniox fires `<end>`) and the LLM gets barged in before responding, the next user utterance is concatenated with the previous one in the messages array rather than creating separate entries.

**Live transcript (Soniox):** STT processor builds live transcript per response — final tokens accumulate, non-final tokens replace on each response (Soniox semantics). Sent as `live_transcript` UI event. Cleared on `<end>`.

**UI shows only committed state:** User turns appear on `user_transcript` (final). Assistant turns appear on `committed_assistant` (after playback done or barge-in). Live assistant speech shown via `assistant_speaking` events from PlaybackSink as words are played.

**Session cleanup:** When UI WebSocket disconnects, session context is cancelled (stops STT/TTS background goroutines), LiveKit room is disconnected, session is removed from map.

**Cartesia TTS context strategy:** Single `context_id` per LLM turn. All sentences sent with `"continue": true` and `"add_timestamps": true`. Final flush with `"continue": false`. On interrupt, sends `{"cancel": true}`.

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

### Debugging / Profiling

- `net/http/pprof` is imported in `main.go` — heap profiles available at `http://localhost:3000/debug/pprof/heap`
- Typical memory: ~38-41 MB RSS for a single session (8 MB heap, rest is Go runtime + CGO/libopus + WebRTC + TLS)
- All pipeline-level allocations are negligible — memory is dominated by LiveKit/pion/protobuf/opus internals

### Known Issues / Gotchas

- Audio can sound choppy if Opus frames aren't paced at 20ms intervals — use `time.Ticker`, not `time.Sleep`
- The go.mod module name is still `awesomeProject` (from the original learning project)
- Upstream frames (WordTimestampFrame, TTSDoneFrame) pass through TTS as a forwarding hop — TTS receives them on its Data channel from PlaybackSink and forwards upstream to LLM
