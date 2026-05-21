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

The pipeline is **complete and working end-to-end** with the Pipecat-style `BaseProcessor` design (single shared `WaitGroup`, two-channel system-frame priority, cancel-and-recreate on interrupt, explicit per-frame direction), barge-in (min-word detection), word timestamp tracking, multi-session support, websocket reconnection, a live UI, EndFrame-driven session cleanup, user idle detection, talk-time enforcement, metrics framework, and a context aggregator that separates conversation orchestration from LLM execution. Each `/connect` request creates an independent `PipelineTask`.

A full test suite covering every processor lives in `_test.go` files; `go test -race ./...` passes cleanly.

#### Pipeline files:

| File | Purpose |
|------|---------|
| `main.go` | Entry point. Loads `.env`, configures logging (both terminal + `app.log`), silences LiveKit/pion logs, serves HTTP (`:3000`). `/connect` creates a PipelineTask, `/ws` streams UI events and queues `EndFrame` on disconnect or `end_call` |
| `processor.go` | The Pipecat-style core: `Direction`, `Envelope{Frame, Direction}`, `Processor` interface, `BaseProcessor` struct, `BroadcastableFrame` interface. `BaseProcessor` owns the per-processor goroutine layout (inputLoop + processLoop), the two input channels (inputSysCh + inputDataCh) for structural system-frame priority, the procCh and per-interrupt procCtx, cancel-and-recreate on `InterruptFrame`, EndFrame auto-cancel, `PushFrame` with `MetricsFrame` intercept, `Broadcast`, `Go` (shared-WG tracking), `Link`/`Prev`/`Next` |
| `pipeline.go` | `Pipeline` (thin: just `Link` + `Start`/`Stop`), `TaskContext` (Ctx, Logger, Room, UIEvents, Metrics, shared `wg`), `PipelineTask` (lifecycle owner: holds source, pipeline, cleanup hook, idempotency flags). `createSession()` wires everything. `completeEnd` disconnects the LiveKit room BEFORE the bounded 10s `wg.Wait` because `AudioSource.ReadRTP` doesn't respect context — disconnecting is what unblocks the reader |
| `pipeline_edges.go` | `PipelineSourceProcessor` (external `Queue(EndFrame)` injection point; `drainExternalFrames` goroutine cancels b.ctx after pushing EndFrame), `PipelineSinkProcessor` (invokes `onEnd` callback on EndFrame) |
| `frame.go` | `Frame` interface with `IsSystem()` (system priority routing) and `IsInterruptible()` (false for EndFrame so it survives interrupt purge — Pipecat's `UninterruptibleFrame` mixin in Go shape). `BroadcastableFrame` adds `Clone() Frame`. Concrete frames: `AudioFrame`, `TextFrame`, `TranscriptFrame`, `EndFrame`, `LLMResponseStartFrame` (carries `StartedAt`), `LLMResponseEndFrame`, `WordTimestampFrame`, `TTSDoneFrame`, `TTSSpeakFrame`, `BotStartedSpeakingFrame`/`BotStoppedSpeakingFrame` (both implement Clone), `LLMMessagesFrame`. System frames: `InterruptFrame`; `MetricsFrame` is system but intercepted by `BaseProcessor.PushFrame` |
| `metrics.go` | `MetricLabel` constants, `MetricsData` struct, `MetricsFrame` (intercepted by `BaseProcessor.PushFrame` → `taskCtx.Metrics`), `ProcessorMetrics` helper with `Start`/`StartAt`/`Stop`/`Reset` (thread-safe timer map) |
| `ui_events.go` | `UIEventType` enum, `UIEvent` struct (`Type` + `Data map[string]interface{}`), `UIEventSender` — per-task WebSocket broadcaster to frontend |
| `livekit_room.go` | `joinRoom(roomName, audioSource)` — bot joins LiveKit room. `generateToken()` creates JWT tokens |
| `livekit-client.html` | Browser client: Pico CSS + Alpine.js. Dark theme. Live transcript, committed turns, multiple metrics badges per turn, connect/disconnect/mute controls |
| `audio_source_processor.go` | Source processor. `SetTrack(track)` spawns a Go-tracked reader goroutine that decodes Opus→PCM (16kHz mono) and calls `PushFrame(AudioFrame, Downstream)` directly. No internal channel |
| `stt_processor.go` | Sends PCM to Soniox websocket, emits raw `TranscriptFrame`s with `ResponseID` and response-level `Finished` metadata. **Connect is lazy** — `Start` spawns a `runReader` goroutine that dials Soniox then reads. Writer goroutine waits on the `connected` signal channel before sending. Auto-reconnects. Logs each STT response and token. `sttDialURL` is a package var so tests can redirect it |
| `user_idle_processor.go` | Sits between STT and ContextAggregator. Tracks user activity (`TranscriptFrame`) and bot completion (`BotStoppedSpeakingFrame`). Injects `TTSSpeakFrame` after `idleTimeout` (7s). Max `maxIdlePrompts` (7) prompts. `idlePromptCount` is `atomic.Int32` (timer callback and `ProcessFrame` both touch it) |
| `context_aggregator.go` | Owns conversation context (messages, one shared interim transcript snapshot, final transcript, barge-in, word tracking, commit). Accumulates final STT tokens across final response chunks until `<end>`, then sends `LLMMessagesFrame` to LLM. Min-word barge-in and frontend live transcript both use the same latest non-final snapshot. Discards below-threshold speech during bot talking. Forwards `BotStarted/StoppedSpeakingFrame` in their arrival direction (no longer hard-coded to upstream) |
| `talktime_monitoring_processor.go` | Sits between ContextAggregator and LLM. `runTimer` is a Go-tracked goroutine started in `Start`; on timeout pushes `InterruptFrame`, `TTSSpeakFrame`, `EndFrame` downstream. `ending` is `atomic.Bool` (timer goroutine writes, `ProcessFrame` reads) |
| `llm_processor.go` | Lean: receives `LLMMessagesFrame` → spawns a Go-tracked `runLLM` goroutine that calls OpenAI (gpt-4.1, streaming SSE) using the per-frame ctx. Stored `cancelLLM` cancels in-flight HTTP on EndFrame. InterruptFrame relies on the base's transitive procCtx cancellation. `llmEndpoint` is a package var so tests can stub OpenAI via `httptest.NewServer` |
| `tts_processor.go` | Cartesia client + sentence aggregator + EndFrame drain controller. **Three goroutines per session:** `runReader` (lazy connect + websocket read, emits typed events on `ttsEvents`), `orchestrator` (owns ALL TTS state — aggregation, synthesis, shutdown — and drives Cartesia), and base input/process loops. `ProcessFrame` is a thin relay over a `commands` channel. EndFrame blocks `ProcessFrame` until orchestrator forwards it; this lets PlaybackSink/etc. shut down in pipeline order. State machine for synthesis is a `ttsSynth` enum (Idle/Streaming/Closing) replacing the older boolean pair. Context-id validation via `atomic.Value`. `ttsDialURL` is a package var; orchestrator waits on `connected` before processing commands |
| `playback_sink_processor.go` | Publishes LiveKit audio track. **Three goroutines:** base input/process loops + a `runPlayback` goroutine with the 20ms ticker. `ProcessFrame` relays through `queueCh`; `runPlayback` owns `playbackQueue`, opus decode/encode, the background mixer, and pacing. `Broadcast`s `BotStartedSpeakingFrame` on first audio and `BotStoppedSpeakingFrame` after `TTSDoneFrame` (both directions, per Pipecat's MediaSender pattern). On `InterruptFrame` walks `playbackQueue` preserving `!IsInterruptible()` frames (EndFrame survives the purge). On `EndFrame` writes a short silence tail before forwarding (matches Pipecat's `audio_out_end_silence_secs`). Metrics (E2E latency) |

#### Testing infrastructure (Pipecat parity)

Ported from `pipecat/tests/utils.py`. Every processor has a `_test.go` file; helpers live in `helpers_test.go`:

| File | Purpose |
|------|---------|
| `helpers_test.go` | `testFixture` (TaskContext + Logger + RootCtx/RootCancel + shared WG + metrics capture), `QueueProcessor` (captures upstream OR downstream frames), `runProcessorTest(t, fix, cfg)` (wires source → processor under test → sink, sends frames, optionally queues EndFrame, force-Stops everyone after a settle window, waits on WG with bounded timeout), `SleepFrame` (synthetic; consumed inline by the send loop), `assertFrameTypes`, `findFrame[T]`, `countFrames[T]` |
| `test_setup_test.go` | `init()` that redirects `sttDialURL`/`ttsDialURL` to an unreachable loopback so background connect goroutines fail fast in tests |
| `processor_test.go` | BaseProcessor: basic forward, upstream forward, EndFrame auto-cancel of b.ctx (verified via `trackedProcessor`), MetricsFrame intercept, system-frame priority (verified via `blockingProcessor`), InterruptFrame purges procCh keeping `!IsInterruptible()` frames (uses `waitForSeen` to deterministically order the interrupt before releasing the blocked frame), Link sets neighbors, Broadcast sends both directions |
| `pipeline_edges_test.go` | PipelineSource Queue() forwards downstream; PipelineSink onEnd callback fires on EndFrame and ignores other frames |
| `audio_source_processor_test.go` | EndFrame forwarding, default-forward for unknown frames |
| `stt_processor_test.go` | EndFrame forwarding, AudioFrame consumed (not forwarded), pass-through for unknown frames |
| `user_idle_processor_test.go` | Timer cancellation on TranscriptFrame, BotStarted/Stopped consumption, EndFrame cancels timer |
| `context_aggregator_test.go` | Final transcript → `LLMMessagesFrame`, barge-in emits InterruptFrame at min-words threshold, no barge-in below threshold, TTSDoneFrame commits assistant message |
| `talktime_monitoring_processor_test.go` | Timer emits Interrupt+TTSSpeak+EndFrame on timeout; frames pass through before timeout; downstream frames dropped during shutdown |
| `llm_processor_test.go` | Streams TextFrames downstream (httptest stub); InterruptFrame cancels in-flight HTTP; EndFrame cancels in-flight; upstream frames pass through; server error handled gracefully |
| `tts_processor_test.go` | InterruptFrame forwarded downstream immediately by ProcessFrame; upstream frames (Word/TTSDone/BotSpeaking) pass through |

Run: `go test -race ./...` (≈5s).

The harness pattern matches Pipecat exactly: wrap the processor between two QueueProcessors, send frames at the source, capture upstream-pushes at the source and downstream-pushes at the sink, then assert frame counts or types. The one Go-specific addition is `settleDelay` (because upstream pushes race with EndFrame's downstream propagation; without a small delay, EndFrame can cancel the source before an upstream frame arrives back at it).

#### Pipeline wiring:
```go
PipelineSourceProcessor → AudioSourceProcessor → STTProcessor → UserIdleProcessor → ContextAggregator → TalkTimeMonitoringProcessor → LLMProcessor → TTSProcessor → PlaybackSinkProcessor → PipelineSinkProcessor
```

### Processor Initialization Pattern

All processors take only `taskCtx *TaskContext` and embed `*BaseProcessor`. No shared per-turn state — all cross-processor communication happens via frames.

```go
NewAudioSourceProcessor(taskCtx)
NewSTTProcessor(taskCtx)
NewUserIdleProcessor(taskCtx)
NewContextAggregator(taskCtx)
NewTalkTimeMonitoringProcessor(taskCtx)
NewLLMProcessor(taskCtx)
NewTTSProcessor(taskCtx)
NewPlaybackSinkProcessor(taskCtx)
```

Inside each constructor:

```go
func NewXxx(taskCtx *TaskContext) *XxxProcessor {
    p := &XxxProcessor{...}
    p.BaseProcessor = NewBaseProcessor("Xxx", p, taskCtx)
    return p
}
```

`TaskContext` is the single task-level dependency containing `Ctx` (root context for the task), `Logger`, `Room`, `UIEvents`, `Metrics` (handler), and `wg` (shared WaitGroup for goroutine tracking).

### Key Design Decisions

**Pipecat-style BaseProcessor (the core):** Every processor embeds `*BaseProcessor`. The base owns:
- Two input channels — `inputSysCh` (priority) and `inputDataCh` — and a `procCh` for data frames pending processing
- Per-processor cancellation tree: `taskCtx.Ctx → b.ctx → procCtx`. `b.ctx` is set in `NewBaseProcessor` (not `Start`) so it's immutable after construction — no race between `QueueFrame` readers and `Start` writers.
- Two goroutines per processor: `inputLoop` (drains input channels with system priority) and `processLoop` (drains procCh, invokes `ProcessFrame` one frame at a time). `processLoop` prioritises `ctx.Done` over `procCh` so an in-flight `ProcessFrame` returning due to cancellation can't accidentally pull another frame from procCh before exiting.
- `PushFrame(frame, dir)` routes to `prev`/`next` via interface dispatch. `MetricsFrame` is intercepted here and delivered to `taskCtx.Metrics` (never propagates).
- `Broadcast(BroadcastableFrame)` clones the frame and pushes both directions.
- `Go(fn)` adds the goroutine to `taskCtx.wg` (the shared task-wide WaitGroup) so `PipelineTask.completeEnd` can wait for everyone.
- On `InterruptFrame` (system frame, dispatched inline by `inputLoop`), the base cancels `procCtx` (stopping in-flight ProcessFrame work that respects ctx), waits up to `procLoopExitTimeout` (3s) for the processLoop goroutine to exit, drains `procCh` keeping frames where `IsInterruptible() == false`, then starts a fresh processLoop with a new `procCtx` and requeues the kept frames. This mirrors Pipecat's `_start_interruption → cancel-and-recreate-process-task` flow.
- On `EndFrame` (received via processLoop), after the user's `ProcessFrame(EndFrame)` returns, the base sets `cancelling = true` and cancels `b.ctx`, which cascades to inputLoop and any `Go`-tracked goroutines.

**Single shared WaitGroup per PipelineTask:** `PipelineTask` owns `wg sync.WaitGroup`. `TaskContext.wg` is `&task.wg`. Every goroutine spawned via `BaseProcessor.Go` is tracked there. `PipelineTask.completeEnd` does `pipeline.Stop()` then `wg.Wait()` with a bounded 10s timeout. No per-processor WaitGroup, no central TaskManager.

**Direction is data, not type-inferred:** Channels carry `Envelope{Frame, Direction}`. `ProcessFrame(ctx, frame, dir)` receives direction as a parameter. Pass-through processors use a `default: PushFrame(frame, dir)` branch, eliminating the ~12 hard-coded upstream-forwarding cases that existed before the BaseProcessor migration.

**Pipecat-style pipeline edges:** External lifecycle frames enter through `PipelineSourceProcessor.Queue(...)`, never by writing directly to another processor's channel. `PipelineSinkProcessor.onEnd` triggers `PipelineTask.completeEnd` when `EndFrame` reaches it. The source's external-frame goroutine cancels `b.ctx` after pushing an EndFrame so the base's input/process loops also unwind (they have no other shutdown signal because the EndFrame leaves via a side goroutine, not procCh).

**Pipeline.Run → Pipeline.Start/Stop:** No central `Send` closure, no positional routing. `Start(ctx)` links neighbours via `Link()` (which sets `prev`/`next` pointers bidirectionally) and starts each processor. `Stop()` calls each processor's `Stop()`, which sets `cancelling = true` and cancels `b.ctx`; the actual goroutine wait happens at the `PipelineTask` level.

**EndFrame-driven call shutdown:** Call end follows Pipecat's graceful `EndFrame` pattern. External handlers (UI WebSocket disconnects, frontend `"end_call"` messages, future transport lifecycle events) should call `PipelineTask.End(reason)` only; they must not directly cancel the task, disconnect LiveKit, or remove the task from the sessions map. `EndFrame` is **not** a system frame: it travels through the normal data path so processors see shutdown in pipeline order. `PipelineTask.End()` queues the frame on `PipelineSourceProcessor`; TTS defers `EndFrame` while it is still generating the current utterance; `PlaybackSinkProcessor` drains queued playback, writes a short silence tail, then forwards `EndFrame` downstream (matches Pipecat's `BaseOutputTransport.MediaSender` pattern); `PipelineSinkProcessor` invokes `PipelineTask.completeEnd`, which sends `call_ended`, calls `pipeline.Stop()`, disconnects the LiveKit room, waits on the shared WG (10s bounded), cancels session ctx, and removes the task. Resource closures inside processors are allowed only as part of handling `EndFrame`.

**`IsInterruptible()` on every frame:** The Go-shaped equivalent of Pipecat's `UninterruptibleFrame` mixin. Only `EndFrame` returns `false` (and `InterruptFrame` / `MetricsFrame` return `false` conventionally; they never enter procCh anyway). The base's `interruptProcessLoop` reads `IsInterruptible()` when draining `procCh`. `PlaybackSink.handleQueueFrame` for `InterruptFrame` uses the same check when purging `playbackQueue` — so a deferred EndFrame at PlaybackSink survives a late interrupt.

**ContextAggregator owns conversation state:** Separated from LLM processor. Owns messages array, live transcript aggregation, final transcript accumulation, barge-in detection, word tracking, and commit logic. LLM processor is lean — just receives `LLMMessagesFrame`, calls the API, streams responses, handles cancellation.

**Min-word barge-in (Pipecat-style):** STT stays dumb and forwards raw Soniox token frames with response-level metadata. Soniox interim responses are transcript snapshots, not deltas: each non-final response repeats the current hypothesis, so ContextAggregator replaces the previous non-final transcript with the latest response's non-final tokens. Final tokens are deltas and may arrive across multiple final response chunks before `<end>`, so ContextAggregator accumulates final tokens until the end token instead of resetting by response ID. One shared `interimTranscript` drives both frontend live transcript updates and barge-in checks. Barge-in can fire on non-final interim frames once `len(strings.Fields(interimTranscript)) >= 3`, but turn-taking starts only on final tokens ending with `<end>`. Do not use Soniox `finished` for turn-taking; it is stream/connection lifecycle metadata, not an utterance boundary. Below-threshold speech during bot talking is discarded (not deferred) — matches Pipecat's `reset_aggregation()` behaviour.

**BotStartedSpeakingFrame / BotStoppedSpeakingFrame are broadcast:** Emitted by `PlaybackSinkProcessor.runPlayback` via `b.Broadcast(...)` (both directions). Upstream copies reach UserIdle via ContextAggregator (cancels idle timer, updates `botSpeaking` state); downstream copies terminate at PipelineSink today but are available for future post-Playback processors. Mirrors Pipecat's `BaseOutputTransport.MediaSender._bot_started_speaking` / `_bot_stopped_speaking`. Broadcast sibling IDs are intentionally not implemented — they exist in Pipecat only to let observers deduplicate, and we have no observers.

**User idle detection:** UserIdleProcessor sits between STT and ContextAggregator. Starts 7s timer on `BotStoppedSpeakingFrame`, cancels on `TranscriptFrame` or `BotStartedSpeakingFrame`. On timeout, injects `TTSSpeakFrame{Text: "Hello?"}` downstream from the timer goroutine via `PushFrame`. `idlePromptCount` is `atomic.Int32` (race fix from the BaseProcessor migration). Max 7 idle prompts.

**Talk-time limit:** `TalkTimeMonitoringProcessor` spawns a `runTimer` goroutine in `Start` (tracked via `Go`). On timeout pushes `InterruptFrame` → `TTSSpeakFrame{Text: "Your talk time is exhausted now. Ending the call."}` → `EndFrame{Reason: "talk time exhausted"}` downstream. `ending` is `atomic.Bool` so `ProcessFrame` (on processLoop) can safely check while `runTimer` (separate goroutine) writes. The monitor does not wait for bot-speaking events; TTS and PlaybackSink preserve frame ordering by treating `EndFrame` as a graceful drain marker.

**TTSSpeakFrame for canned utterances:** Bypasses LLM — goes directly to TTS for standalone synthesis. TTS creates a new Cartesia context, synthesizes the text, and forwards `TTSSpeakFrame` to PlaybackSink (which resets its `interrupted` state). Audio flows normally through word tracking and commit.

**InterruptFrame for barge-in:** ContextAggregator detects barge-in (min-word threshold met while `botSpeaking`), sends `InterruptFrame` downstream. Each downstream processor receives it on its `inputSysCh` (system priority), the base cancels its procCtx (killing any in-flight `ProcessFrame`), purges procCh, then calls the user's `ProcessFrame(InterruptFrame)` to do processor-specific cleanup (TTS cancels Cartesia context + clears state, LLM resets metrics, PlaybackSink clears playbackQueue preserving EndFrame).

**Cartesia context-id validation (stale audio prevention):** TTS uses `atomic.Value` for `activeContextId`. The reader goroutine (`readTTSConnectionData`) validates every Cartesia message's `context_id` against the active value before pushing typed events to the orchestrator. The orchestrator re-checks the event context against `currentContextId` before mutating PCM/word state. On interrupt, `activeContextId` is set to `""` before queued TTS events are drained — all messages from the cancelled context are dropped or ignored as stale.

**TTS synthesis state machine (`ttsSynth` enum):** Replaces the older `awaitingTTSDone`/`ttsFlushSent` boolean pair. Three states: `ttsSynthIdle`, `ttsSynthStreaming`, `ttsSynthClosing`. Transitions are named, handleEnd checks `synth != ttsSynthIdle` to decide whether to defer EndFrame.

**LLMResponseStartFrame carries timestamp:** `StartedAt time.Time` is set by LLM when the turn begins. PlaybackSink uses it for accurate E2E latency measurement (measures from LLM turn start to first audio frame played).

**Word timestamp interleaving in TTS:** TTS tracks `audioTimePushed` (seconds) and buffers `pendingWords` from Cartesia timestamp messages. After each opus frame is pushed, words whose `start <= audioTimePushed` are emitted as `WordTimestampFrame`. This means words arrive at PlaybackSink in the correct playback order.

**Commit timing:** Assistant text is committed to conversation history when `TTSDoneFrame` flows upstream from PlaybackSink through TTS → LLM → ContextAggregator (normal completion) or on barge-in (partial text from internally accumulated words). NOT deferred to next turn.

**User message concatenation:** If the user pauses mid-thought (Soniox fires `<end>`) and the LLM gets barged in before responding, the next user utterance is concatenated with the previous one in the messages array rather than creating separate entries.

**Metrics framework (Pipecat-style):** `ProcessorMetrics` helper with `Start`/`Stop` timer pairs. Processors emit `MetricsFrame` via `PushFrame` which intercepts and routes to `taskCtx.Metrics` (handler logs + sends `UIEvent{Type: Metrics}` to frontend). Current metrics: LLM TTFB, LLM processing, TTS text aggregation, TTS TTFB, E2E turn latency.

**Generic UIEvents:** `UIEvent` has typed `Type` (enum) + generic `Data map[string]interface{}`. Any processor can send any payload without struct changes.

**Lazy websocket connect (STT and TTS):** Connect calls were moved from constructors into Go-tracked goroutines spawned by `Start`. `runReader` dials in a retry loop, then signals `connected` (a chan struct{}) and proceeds with the read loop. STT's writer and TTS's orchestrator both wait on `connected` before sending. The retry sleep respects `b.ctx` / `taskCtx.Ctx` / `done` so `Stop` unblocks them promptly. Side benefit: `/connect` HTTP response is no longer gated on the Soniox+Cartesia dial latency.

**Session cleanup:** UI WebSocket disconnects and frontend disconnect button presses call `PipelineTask.End(reason)` which queues an `EndFrame`; cleanup happens only when that `EndFrame` reaches PlaybackSink (which has its own drain) and ultimately PipelineSink (which fires `onEnd → completeEnd`). `End()` is idempotent and ignores late requests once cleanup has started or the task context has been cancelled. The frontend disconnect button sends `{type:"end_call"}` on the UI WebSocket and waits for `call_ended` before locally disconnecting LiveKit/WebSocket. The sessions registry map name is `sessions`, even though the type is `*PipelineTask` (the user-facing concept is still "a session").

**Cartesia TTS context strategy:** Single `context_id` per LLM turn. All sentences sent with `"continue": true` and `"add_timestamps": true`. Final flush with `"continue": false`. On interrupt, sends `{"cancel": true}`.

### Key Technical Details

- **Audio formats**: Soniox expects s16le/16kHz/mono. Cartesia outputs s16le/24kHz/mono. LiveKit/WebRTC uses Opus at 48kHz clock rate (always signaled as stereo in SDP).
- **Opus decode**: `opus.NewDecoder(16000, 1)` — decodes directly to 16kHz for Soniox.
- **Opus encode**: `opus.NewEncoder(24000, 1, opus.AppVoIP)` — encodes 24kHz Cartesia PCM. Frame size: 480 samples (20ms at 24kHz) = 960 bytes.
- **LiveKit track publish**: `ClockRate: 48000, Channels: 2` in RTP capability (WebRTC requirement), even though actual audio is 24kHz mono.
- **Pacing**: Opus frames must be sent at real-time pace (one 20ms frame per tick) or the browser jitter buffer overflows and drops audio.
- **Websocket idle timeouts**: Soniox and Cartesia close connections after ~15-20s of no data. Pipeline is initialized lazily on client connect.
- **Track re-subscription**: LiveKit fires `onTrackSubscribed` multiple times during renegotiation. `AudioSourceProcessor.SetTrack()` handles this cleanly by spawning a new Go-tracked reader goroutine per call.
- **Concurrent websocket writes**: gorilla/websocket panics on concurrent writes. Each ws has exactly one writer goroutine (STT's `writeAudioWebsocket`; TTS writes only from the orchestrator goroutine).
- **`completeEnd` ordering**: `pipeline.Stop()` → `Room.Disconnect()` → `wg.Wait(timeout=10s)` → `task.Cancel()` → `removeSession`. Room.Disconnect comes BEFORE the WG wait because `AudioSource.ReadRTP` doesn't take a context; closing the LiveKit room is what unblocks the reader.

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
- `github.com/hajimehoshi/go-mp3` — MP3 decoding for background office sound
- `github.com/go-logr/stdr` — Used to silence LiveKit/pion debug logs

### Build & Run

```bash
brew install opus opusfile pkg-config  # one-time
go build ./...
./awesomeProject  # or: go run .
# Open http://localhost:3000, click Connect
```

### Testing

```bash
go test ./...               # all tests
go test -race ./...         # with race detector (~5s)
go test -race -count=5 ./... # stress (5x)
go test -v -run TestUserIdle ./...  # one processor's tests
```

`test_setup_test.go` redirects `sttDialURL` / `ttsDialURL` to unreachable loopbacks; LLM tests stub OpenAI via `httptest.NewServer` by overriding `llmEndpoint`. No external service is hit during tests. Integration testing (real Soniox/Cartesia/LiveKit) is out of scope for the unit suite — exercise via the actual app at `http://localhost:3000`.

### Debugging / Profiling

- `net/http/pprof` is imported in `main.go` — heap profiles available at `http://localhost:3000/debug/pprof/heap`
- Typical memory: ~38-41 MB RSS for a single session (8 MB heap, rest is Go runtime + CGO/libopus + WebRTC + TLS)
- All pipeline-level allocations are negligible — memory is dominated by LiveKit/pion/protobuf/opus internals

### Known Issues / Gotchas

- Audio can sound choppy if Opus frames aren't paced at 20ms intervals — use `time.Ticker`, not `time.Sleep`
- The go.mod module name is still `awesomeProject` (from the original learning project)
- Upstream frames (WordTimestampFrame, TTSDoneFrame, BotStarted/StoppedSpeakingFrame) pass through TTS and LLM as forwarding hops to reach ContextAggregator and UserIdleProcessor; the `default: PushFrame(frame, dir)` in each processor handles this
- Soniox sends transcript snapshots: individual responses duplicate prior non-final tokens and add/refine the current hypothesis. STT should forward token text plus response metadata (`ResponseID`, `Finished`) without owning transcript semantics. ContextAggregator replaces non-final text per response, updates the UI from that latest snapshot, and starts turns only from final tokens ending in `<end>`; `Finished` is logged for debugging but is not the turn boundary
- Cartesia may send late audio/done messages after context cancellation — `activeContextId` atomic check in TTS reader goroutine drops them
- The 3s `procLoopExitTimeout` in `BaseProcessor.interruptProcessLoop` is a safety net: if a user's `ProcessFrame` ignores the per-frame ctx and runs longer than 3s, the base proceeds with the purge anyway and the in-flight ProcessFrame may complete concurrently with the new processLoop. All our processors' `ProcessFrame` impls respect ctx or finish in well under a second
- TTS orchestrator waits on `connected` before processing commands. If Cartesia is permanently unreachable, commands queue in `t.commands` (capacity 100). `PipelineTask.completeEnd`'s 10s WG-wait timeout is the ultimate escape hatch
- Tests use `settleDelay` between framesToSend and EndFrame when the processor under test pushes upstream frames (e.g. broadcast tests, barge-in tests). Without it, EndFrame's downstream propagation can cancel the source's b.ctx before the upstream frame arrives back at it, dropping the upstream frame
