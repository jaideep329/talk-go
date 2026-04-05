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

### Current State: Pipeline Architecture (Working)

The pipeline is **complete and working end-to-end** with barge-in, word timestamp tracking, multi-session support, and websocket reconnection. Each `/connect` request creates an independent session with its own room, pipeline, and processors.

#### Pipeline files:

| File | Purpose |
|------|---------|
| `main.go` | Entry point. Loads `.env`, configures logging (both terminal + `app.log`), silences LiveKit/pion logs, serves HTTP (`:3000`). `handleConnect` creates a new session per client |
| `livekit_room.go` | `joinRoom(roomName, audioSource)` — bot joins a specific LiveKit room. `onTrackSubscribed` calls `audioSource.SetTrack(track)`. `generateToken()` creates LiveKit JWT tokens |
| `livekit-client.html` | Browser client using `livekit-client` JS SDK. Connects to room via `/connect`, enables mic, plays remote audio tracks. Connect button disabled during request |
| `frame.go` | Frame types: `Frame` interface. Concrete: `AudioFrame`, `TextFrame`, `TranscriptFrame{Text, IsFinal}`, `InterruptFrame`, `LLMResponseStartFrame`, `LLMResponseEndFrame`, `WordTimestampFrame{Words, Start}`, `EndFrame` |
| `frame_processor.go` | `FrameProcessor` interface: `Process(in <-chan Frame, out chan<- Frame)` |
| `pipeline.go` | `Pipeline` struct, `createSession()` — generates unique room name, creates all processors and wires them up. Each session is fully independent |
| `turn_context.go` | `TurnContext` — shared across LLM, TTS, and PlaybackSink. Manages per-turn `context.WithCancel` for instant interrupt broadcast, plus spoken word tracking with timestamps |
| `audio_source_processor.go` | Source processor (ignores `in`). `SetTrack(track)` starts a new reader goroutine. Decodes Opus→PCM (16kHz mono). Old readers exit naturally on track EOF |
| `stt_processor.go` | Receives `AudioFrame`s, sends PCM to Soniox websocket, emits `TranscriptFrame`s. Auto-reconnects on websocket disconnect |
| `llm_processor.go` | Accumulates final `TranscriptFrame` tokens, triggers `runLLM` on `<end>`. Streams OpenAI (gpt-4.1). Detects barge-in, manages conversation history via `TurnContext` spoken words |
| `tts_processor.go` | Aggregates sentences, sends to Cartesia with single `context_id` per turn. PCM→Opus encoding. Interleaves `WordTimestampFrame`s with `AudioFrame`s. Auto-reconnects on websocket disconnect |
| `playback_sink_processor.go` | Publishes LiveKit audio track. 20ms ticker pacing. Tracks spoken words via `TurnContext.AppendWords()`. Records `playbackStartedAt` on first audio frame |

#### Pipeline wiring:
```go
AudioSourceProcessor → STTProcessor → LLMProcessor → TTSProcessor → PlaybackSinkProcessor
```

### Pipeline Design Decisions

**Processor communication pattern:** Processors do NOT hold references to each other. All data flows through `Process(in, out)` — no shared context parameter. Each processor manages its own lifecycle. For async work, use an **internal channel** to bridge background goroutines back to the `Process` select loop (async bridge pattern).

**Pipeline lifecycle:** `EndFrame` propagates through the pipeline to shut down all processors. No shared `context.Context` for lifecycle — processors manage their own internal contexts.

**Cartesia TTS context strategy:** Single `context_id` per LLM turn (not per sentence). All sentences sent with `"continue": true` and `"add_timestamps": true` for word-level tracking. Final flush with `"continue": false`. On interrupt, sends `{"context_id": ..., "cancel": true}` to stop Cartesia server-side generation.

**Track re-subscription:** `AudioSourceProcessor.SetTrack(track)` starts a new reader goroutine. Old goroutine exits naturally on EOF. No pipeline teardown needed.

**Multi-session:** Each `/connect` request creates a new room (`room-<random>`), a new pipeline, and independent processor instances. No shared state between sessions. All logs prefixed with `[room-XXXXX]` via per-session `*log.Logger`.

**Websocket reconnection:** STT and TTS processors auto-reconnect on websocket errors (handles Soniox/Cartesia idle timeouts of ~15-20s).

#### Interruption / Barge-in (DONE)

**TurnContext** (`turn_context.go`): Shared `context.WithCancel` across LLM, TTS, and PlaybackSink. LLM calls `Cancel()` on barge-in (wakes all processors instantly via `Done()` channel). Calls `Reset()` at the start of each new turn. Processors use the **nil channel trick**: after handling interrupt, set `done = nil` to stop selecting on the closed channel; re-enable on `LLMResponseStartFrame`.

**Barge-in detection** (LLM Processor): Any `TranscriptFrame` (final or non-final) arriving while `isStreaming == true` triggers interrupt. Sub-millisecond reaction time (~0.17ms from transcript to cancel). `isStreaming` stays true after `LLMResponseEndFrame` (bot may still be speaking) — only reset on barge-in or next turn start.

**Interrupt propagation**: `TurnContext.Cancel()` broadcasts to all processors simultaneously (no frame-based propagation — frames get stuck behind buffered audio). TTS cancels Cartesia context, drains internal audio channel, clears buffers. PlaybackSink sets `interrupted = true`, skips audio frames until next `LLMResponseStartFrame`.

**Word timestamp tracking** (for accurate LLM context):
- TTS requests `"add_timestamps": true` from Cartesia
- Cartesia sends `"timestamps"` messages with words and start times (seconds from context start)
- TTS pushes `WordTimestampFrame{Words, Start}` into `audioFrames` channel (interleaved with `AudioFrame`s)
- `TurnContext` shared struct: PlaybackSink appends words + start times; records `playbackStartedAt` on first audio frame
- On **barge-in**: `SpokenSoFar()` uses `elapsed = time.Since(playbackStartedAt)` to return only words whose `start <= elapsed`
- On **normal turn end**: `FlushAll()` returns all words (committed when next turn starts, not on `LLMResponseEndFrame`, since word timestamps may not have all arrived yet)
- LLM adds the result to `messages` as the assistant turn

### What Needs to Happen Next

#### Cleanup:
- Consider renaming module from `awesomeProject`
- Remove `InterruptFrame` from `frame.go` (no longer used — replaced by `TurnContext`)

### Key Technical Details

- **Audio formats**: Soniox expects s16le/16kHz/mono. Cartesia outputs s16le/24kHz/mono. LiveKit/WebRTC uses Opus at 48kHz clock rate (always signaled as stereo in SDP).
- **Opus decode**: `opus.NewDecoder(16000, 1)` — decodes directly to 16kHz for Soniox.
- **Opus encode**: `opus.NewEncoder(24000, 1, opus.AppVoIP)` — encodes 24kHz Cartesia PCM. Frame size: 480 samples (20ms at 24kHz) = 960 bytes.
- **LiveKit track publish**: `ClockRate: 48000, Channels: 2` in RTP capability (WebRTC requirement), even though actual audio is 24kHz mono.
- **Pacing**: Opus frames must be sent at real-time pace (one 20ms frame per tick) or the browser jitter buffer overflows and drops audio.
- **Websocket idle timeouts**: Soniox and Cartesia close connections after ~15-20s of no data. Pipeline is initialized lazily on client connect.
- **Track re-subscription**: LiveKit fires `onTrackSubscribed` multiple times during renegotiation. `AudioSourceProcessor.SetTrack()` handles this cleanly without pipeline teardown.
- **Concurrent websocket writes**: gorilla/websocket panics on concurrent writes. Ensure only one goroutine writes to each websocket at a time.

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
- The go.mod module name is still `awesomeProject` (from the original learning project). The binary is named `awesomeProject`.
