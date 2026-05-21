# talk-go — Project Context

## About the User

Jaideep is an experienced Python backend engineer learning Go by building real projects. He understands concurrency concepts (threading, asyncio) but is new to Go syntax, channels, goroutines, and Go idioms. He maps Go concepts to Python equivalents to build mental models.

## How to Collaborate

- **Guide step by step, don't dump full code.** Explain the "why" alongside small code snippets. Let him write the code himself unless he explicitly asks you to make changes directly.
- **When he says "make the changes directly" or similar, do it.** Otherwise default to guiding.
- **Explain Go concepts by mapping to Python equivalents** (e.g., channels = queue.Queue, goroutines = lightweight threads, defer = try/finally, interfaces = duck typing but explicit).
- **Keep responses concise.** He can read code — don't summarize what you just did.
- **Don't over-engineer.** He will push back if something is unnecessarily complex. Prefer the simplest solution first.
- **When debugging, check `app.log` first** — all `log.*` output goes there. `fmt.Print*` goes to stdout/terminal. Logs go to both `app.log` and terminal via `io.MultiWriter`.
- **After major design decisions or discussions, always update AGENTS.md.** Don't wait to be asked — if a design pattern, architecture choice, or strategy was decided in conversation, persist it immediately.
- **Use Pipecat as the reference for core calling-system details before implementing.** For call lifecycle, turn-taking, interruption, transport events, idle handling, frame semantics, or shutdown behavior, inspect the local Pipecat source first, mention the relevant Pipecat file/pattern in the reasoning, then implement the simplest equivalent in this Go codebase. Prefer `/Users/jaideepsingh/Projects/disha-backend/.venv/lib/python3.11/site-packages/pipecat` (`pipecat-ai==0.0.108`) and Disha's existing bot code in `/Users/jaideepsingh/Projects/disha-backend/bots` before falling back to public docs.

## Project Overview

A real-time voice assistant pipeline built in Go, using LiveKit for browser-based audio I/O:

```
Browser Mic → LiveKit Room → Go Bot (Opus decode → PCM) → Soniox STT → OpenAI LLM → Cartesia TTS → (PCM → Opus encode) → LiveKit Room → Browser Speaker
```

### Architecture

The Go server joins a LiveKit room as a "bot" participant. A browser client (`livekit-client.html`) joins the same room. The bot subscribes to the user's audio track, decodes Opus to PCM, sends it to Soniox for speech-to-text. When Soniox detects an endpoint (`<end>` token), the transcript is sent to OpenAI's streaming LLM. LLM tokens are accumulated into sentences (split on `.?!`), and each sentence is sent to Cartesia TTS. Cartesia returns raw PCM audio which is encoded to Opus and published back to the LiveKit room for the browser to play.

### Current State

The pipeline is **complete and working end-to-end** with barge-in (min-word detection), word timestamp tracking, multi-session support, websocket reconnection, a live UI, session cleanup, user idle detection, metrics framework, and a context aggregator that separates conversation orchestration from LLM execution. Each `/connect` request creates an independent session.

#### Pipeline files:

| File | Purpose |
|------|---------|
| `main.go` | Entry point. Loads `.env`, configures logging (both terminal + `app.log`), silences LiveKit/pion logs, serves HTTP (`:3000`). `/connect` creates sessions, `/ws` streams UI events. Imports `net/http/pprof` for profiling |
| `pipeline.go` | `Pipeline` struct with `Run()` — creates per-processor data/system channels, injects `Send` function for routing. Intercepts `MetricsFrame` in Send and routes to metrics handler. `SessionContext` (single dependency for all processors: Logger, Room, UIEvents, Ctx). `Session` struct for EndFrame-driven cleanup. `createSession()` wires everything |
| `pipeline_edges.go` | Pipecat-style edge processors: `PipelineSourceProcessor` queues external lifecycle frames into the pipeline; `PipelineSinkProcessor` handles terminal frames reaching the end of the pipeline |
| `frame.go` | Frame types: `Frame` interface with `IsSystem()` for routing priority. Data/control frames: `AudioFrame`, `TextFrame`, `TranscriptFrame`, `EndFrame`, `LLMResponseStartFrame` (carries `StartedAt`), `LLMResponseEndFrame`, `WordTimestampFrame`, `TTSDoneFrame`, `TTSSpeakFrame`, `BotStartedSpeakingFrame`, `BotStoppedSpeakingFrame`, `LLMMessagesFrame`. System frames: `InterruptFrame`; `MetricsFrame` is system but intercepted by pipeline routing |
| `frame_processor.go` | `Direction` (Downstream/Upstream), `ProcessorChannels` (Data, System channels + Send function), `FrameProcessor` interface: `Process(ch ProcessorChannels)` |
| `metrics.go` | `MetricLabel` constants, `MetricsData` struct, `MetricsFrame` (system frame, intercepted by pipeline), `ProcessorMetrics` helper with `Start`/`StartAt`/`Stop`/`Reset` (thread-safe timer map) |
| `ui_events.go` | `UIEventType` enum, `UIEvent` struct (`Type` + `Data map[string]interface{}`), `UIEventSender` — per-session WebSocket broadcaster to frontend |
| `livekit_room.go` | `joinRoom(roomName, audioSource)` — bot joins LiveKit room. `generateToken()` creates JWT tokens |
| `livekit-client.html` | Browser client: Pico CSS + Alpine.js. Dark theme. Live transcript, committed turns, multiple metrics badges per turn, connect/disconnect/mute controls |
| `audio_source_processor.go` | Source processor. `SetTrack(track)` starts reader goroutine. Decodes Opus→PCM (16kHz mono) |
| `stt_processor.go` | Sends PCM to Soniox websocket, emits raw `TranscriptFrame`s with `ResponseID` and response-level `Finished` metadata. Logs each STT response and token. Auto-reconnects |
| `user_idle_processor.go` | Sits between STT and ContextAggregator. Tracks user activity (TranscriptFrame) and bot completion (BotStoppedSpeakingFrame). Injects `TTSSpeakFrame` after 15s of silence. Max 2 idle prompts per silence period |
| `context_aggregator.go` | Owns conversation context (messages, one shared interim transcript snapshot, final transcript, barge-in, word tracking, commit). Accumulates final STT tokens across final response chunks until `<end>`, then sends `LLMMessagesFrame` to LLM. Min-word barge-in and frontend live transcript both use the same latest non-final snapshot. Discards below-threshold speech during bot talking. Defers to `BotStoppedSpeakingFrame` for state tracking |
| `talktime_monitoring_processor.go` | Sits between ContextAggregator and LLM. Starts max talk-time timer, interrupts active bot work on timeout, then sends final `TTSSpeakFrame` followed by graceful `EndFrame` |
| `llm_processor.go` | Lean: receives `LLMMessagesFrame` → calls OpenAI (gpt-4.1) → streams response. Handles `InterruptFrame` on system channel (cancels HTTP). Forwards all upstream frames to aggregator. Metrics (TTFB, processing) |
| `tts_processor.go` | Aggregates sentences, sends to Cartesia. The websocket reader only parses Cartesia messages and enqueues typed TTS events; the processor goroutine owns PCM buffering, PCM→Opus encoding, word timestamp interleaving, metrics, and downstream frame emission. Handles `InterruptFrame` on system channel. Handles `TTSSpeakFrame` for standalone synthesis. Defers `EndFrame` while synthesis is in progress so generated audio and `TTSDoneFrame` stay ordered before shutdown, with a bounded timeout if Cartesia never sends done. Context-id validation via `atomic.Value` prevents stale audio from cancelled contexts. Auto-reconnects |
| `playback_sink_processor.go` | Publishes LiveKit audio track. 20ms ticker pacing. Emits `BotStartedSpeakingFrame` (first audio) and `BotStoppedSpeakingFrame` (after TTSDoneFrame) upstream. Handles `TTSSpeakFrame` (resets interrupted state). When queued playback reaches `EndFrame`, writes a short silence tail before forwarding it, matching Pipecat's output transport end-silence pattern. Metrics (E2E latency) |

#### Pipeline wiring:
```go
PipelineSourceProcessor → AudioSourceProcessor → STTProcessor → UserIdleProcessor → ContextAggregator → TalkTimeMonitoringProcessor → LLMProcessor → TTSProcessor → PlaybackSinkProcessor → PipelineSinkProcessor
```

### Processor Initialization Pattern

All processors take only `sessionCtx *SessionContext`. No shared per-turn state — all cross-processor communication happens via frames.

```go
NewAudioSourceProcessor(sessionCtx)
NewSTTProcessor(sessionCtx)
NewUserIdleProcessor(sessionCtx)
NewContextAggregator(sessionCtx)
NewTalkTimeMonitoringProcessor(sessionCtx)
NewLLMProcessor(sessionCtx)
NewTTSProcessor(sessionCtx)
NewPlaybackSinkProcessor(sessionCtx)
```

`SessionContext` is the single session-level dependency containing Logger, Room, UIEvents, and Ctx (for session cancellation).

### Key Design Decisions

**Pipecat-style frame-only communication (no shared state):** All cross-processor communication happens via frames flowing through channels. No shared mutable objects between processors. Each processor is self-contained — receives frames on its channels, sends frames via its `Send` function.

**Pipecat-style pipeline edges:** External lifecycle frames enter through `PipelineSourceProcessor`, not by writing directly to another processor's channel and not through ad-hoc channels on `Pipeline`. Terminal lifecycle frames are observed by `PipelineSinkProcessor`. This mirrors Pipecat's source/sink boundary while keeping the Go implementation small.

**ProcessorChannels pattern:** Every processor receives a `ProcessorChannels` struct with two input channels (Data, System) and a `Send` function. The pipeline creates these and injects routing logic:
```go
type ProcessorChannels struct {
    Data   <-chan Frame    // normal priority input (from both upstream and downstream neighbors)
    System <-chan Frame    // high priority input (InterruptFrame) — checked first
    Send   func(frame Frame, dir Direction)  // routes to next/previous processor
}
```
- `Send(frame, Downstream)` → routes to next processor's System channel (if `frame.IsSystem()`) or Data channel
- `Send(frame, Upstream)` → routes to previous processor's Data channel
- `MetricsFrame` is intercepted by Send and routed to the pipeline's metrics handler (never reaches processors)

**System channel priority (nested select trick):** Processors that handle InterruptFrame use a nested select: outer select checks System channel first with a `default` fallback to an inner select that checks both System and Data. This ensures system frames (interrupts) bypass any buffered data frames.

**EndFrame-driven call shutdown:** Call end follows Pipecat's graceful `EndFrame` pattern. External handlers (UI WebSocket disconnects, frontend "end_call" messages, future transport lifecycle events) should call `Session.End(reason)` only; they must not directly cancel the session, disconnect LiveKit, or remove the session. `EndFrame` is not a system frame: it travels through the normal data path so processors see shutdown in pipeline order. Processors should not add system-channel `EndFrame` handlers; if an `EndFrame` appears on a system channel, fix the sender/routing bug instead of supporting it defensively. `Session.End()` queues the frame on `PipelineSourceProcessor`; TTS defers `EndFrame` while it is still generating the current utterance; `PlaybackSinkProcessor` drains queued playback, then writes a short silence tail before forwarding `EndFrame`, matching Pipecat's `BaseOutputTransport.MediaSender` pattern where `EndFrame` is queued behind audio and final silence is sent before stopping; `PipelineSinkProcessor` invokes session cleanup, which sends `call_ended`, cancels session context, disconnects the LiveKit bot room, and removes the session. Resource closures inside processors are allowed only as part of handling `EndFrame`.

**ContextAggregator owns conversation state:** Separated from LLM processor. Owns messages array, live transcript aggregation, final transcript accumulation, barge-in detection, word tracking, and commit logic. LLM processor is lean — just receives `LLMMessagesFrame`, calls the API, streams responses, handles cancellation.

**Min-word barge-in (Pipecat-style):** STT stays dumb and forwards raw Soniox token frames with response-level metadata. Soniox interim responses are transcript snapshots, not deltas: each non-final response repeats the current hypothesis, so ContextAggregator replaces the previous non-final transcript with the latest response's non-final tokens. Final tokens are deltas and may arrive across multiple final response chunks before `<end>`, so ContextAggregator accumulates final tokens until the end token instead of resetting by response ID. One shared `interimTranscript` drives both frontend live transcript updates and barge-in checks. Barge-in can fire on non-final interim frames once `len(strings.Fields(interimTranscript)) >= 3`, but turn-taking starts only on final tokens ending with `<end>`. Do not use Soniox `finished` for turn-taking; it is stream/connection lifecycle metadata, not an utterance boundary. Below-threshold speech during bot talking is discarded (not deferred) — matches Pipecat's `reset_aggregation()` behavior.

**BotStartedSpeakingFrame / BotStoppedSpeakingFrame:** Emitted by PlaybackSinkProcessor upstream. BotStartedSpeaking on first audio frame played, BotStoppedSpeaking after TTSDoneFrame. Flow upstream through TTS → LLM → ContextAggregator → UserIdleProcessor. Used for barge-in state tracking and idle detection.

**User idle detection:** UserIdleProcessor sits between STT and ContextAggregator. Starts 15s timer on BotStoppedSpeakingFrame, cancels on TranscriptFrame. On timeout, injects `TTSSpeakFrame{Text: "Hello? Are you still there?"}` downstream. Max 2 idle prompts, resets on user speech.

**Talk-time limit:** TalkTimeMonitoringProcessor starts a max talk-time timer when the pipeline starts. On timeout, it sends `InterruptFrame` downstream to cancel active LLM/TTS/playback, then sends `TTSSpeakFrame{Text: "Your talk time is exhausted now. Ending the call."}` followed immediately by `EndFrame{Reason: "talk time exhausted"}`. The monitor does not wait for bot-speaking events; TTS and PlaybackSink preserve frame ordering by treating `EndFrame` as a graceful drain marker, so the closing TTS is generated and played before cleanup.

**TTSSpeakFrame for canned utterances:** Bypasses LLM — goes directly to TTS for standalone synthesis. TTS creates a new Cartesia context, synthesizes the text, and forwards TTSSpeakFrame to PlaybackSink (which resets its interrupted state). Audio flows normally through word tracking and commit.

**InterruptFrame for barge-in:** ContextAggregator detects barge-in (min-word threshold met while `botSpeaking`), sends `InterruptFrame` downstream. LLM receives it on system channel (priority), cancels HTTP request, forwards downstream. TTS cancels Cartesia context, clears the active context before draining queued TTS events, resets state, forwards downstream. PlaybackSink stops playback.

**Cartesia context-id validation (stale audio prevention):** TTS uses `atomic.Value` for `activeContextId`. The reader goroutine (`readTTSConnectionData`) validates every Cartesia message's `context_id` against the active value before pushing typed events to the processor goroutine. The processor re-checks the event context against `currentContextId` before mutating PCM/word state. On interrupt, `activeContextId` is set to `""` before queued TTS events are drained — all messages from the cancelled context are dropped or ignored as stale. Matches Pipecat's `audio_context_available()` three-layer defense while keeping non-atomic buffering state owned by one goroutine.

**LLMResponseStartFrame carries timestamp:** `StartedAt time.Time` is set by LLM when the turn begins. PlaybackSink uses it for accurate E2E latency measurement (measures from LLM turn start to first audio frame played).

**Word timestamp interleaving in TTS:** TTS tracks `audioTimePushed` (seconds) and buffers `pendingWords` from Cartesia timestamp messages. After each opus frame is pushed, words whose `start <= audioTimePushed` are emitted as `WordTimestampFrame`. This means words arrive at PlaybackSink in the correct playback order.

**Commit timing:** Assistant text is committed to conversation history when `TTSDoneFrame` flows upstream from PlaybackSink through TTS → LLM → ContextAggregator (normal completion) or on barge-in (partial text from internally accumulated words). NOT deferred to next turn.

**User message concatenation:** If the user pauses mid-thought (Soniox fires `<end>`) and the LLM gets barged in before responding, the next user utterance is concatenated with the previous one in the messages array rather than creating separate entries.

**Metrics framework (Pipecat-style):** `ProcessorMetrics` helper with `Start`/`Stop` timer pairs. Processors emit `MetricsFrame` which is intercepted by the pipeline's Send function (never reaches other processors). Handler logs + sends `UIEvent{Type: Metrics}` to frontend. Current metrics: LLM TTFB, LLM processing, TTS text aggregation, TTS TTFB, E2E turn latency.

**Generic UIEvents:** `UIEvent` has typed `Type` (enum) + generic `Data map[string]interface{}`. Any processor can send any payload without struct changes.

**Session cleanup:** UI WebSocket disconnects and frontend disconnect button presses queue an `EndFrame`; cleanup happens only when that `EndFrame` reaches PlaybackSink. `Session.End()` is idempotent and ignores late requests once cleanup has started or the session context has been cancelled, so websocket close events caused by cleanup do not enqueue a second `EndFrame`. The frontend disconnect button sends `{type:"end_call"}` on the UI WebSocket and waits for `call_ended` before locally disconnecting LiveKit/WebSocket.

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
- **Committed turns:** User turns appear on `user_transcript` event. Assistant turns appear on `committed_assistant` event with metrics badges.
- **Metrics:** Buffered as `pendingMetrics` array, attached to assistant turn when committed. Multiple badges per turn (LLM TTFB, TTS TTFB, E2E latency, etc.).
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
- Upstream frames (WordTimestampFrame, TTSDoneFrame, BotStarted/StoppedSpeakingFrame) pass through TTS and LLM as forwarding hops to reach ContextAggregator and UserIdleProcessor
- Soniox sends transcript snapshots: individual responses duplicate prior non-final tokens and add/refine the current hypothesis. STT should forward token text plus response metadata (`ResponseID`, `Finished`) without owning transcript semantics. ContextAggregator replaces non-final text per response, updates the UI from that latest snapshot, and starts turns only from final tokens ending in `<end>`; `Finished` is logged for debugging but is not the turn boundary
- Cartesia may send late audio/done messages after context cancellation — `activeContextId` atomic check in TTS reader goroutine drops them
