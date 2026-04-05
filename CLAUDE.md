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

The pipeline refactor is **complete and working end-to-end**. All old non-pipeline files have been deleted. The pipeline is initialized lazily when the browser client clicks Connect (`/connect` endpoint).

#### Pipeline files:

| File | Purpose |
|------|---------|
| `main.go` | Entry point. Loads `.env`, configures logging (both terminal + `app.log`), silences LiveKit/pion logs, serves HTTP (`:3000`). `handleConnect` endpoint initializes pipeline on first client connection via `sync.Once` |
| `livekit_room.go` | `joinRoom(audioSource)` — bot joins LiveKit room. `onTrackSubscribed` calls `audioSource.SetTrack(track)` to swap tracks on renegotiation. `generateToken()` creates LiveKit JWT tokens |
| `livekit-client.html` | Browser client using `livekit-client` JS SDK. Connects to room via `/connect`, enables mic, plays remote audio tracks |
| `frame.go` | Frame types: `Frame` interface with `FrameType()` method. Concrete types: `AudioFrame`, `TextFrame`, `TranscriptFrame{Text, IsFinal}`, `InterruptFrame`, `LLMResponseStartFrame`, `LLMResponseEndFrame` |
| `frame_processor.go` | `FrameProcessor` interface: `Process(ctx context.Context, in <-chan Frame, out chan<- Frame)` |
| `pipeline.go` | `Pipeline` struct with `NewPipeline([]FrameProcessor)` and `Run(ctx context.Context)`. Creates buffered channels between processors, starts each in a goroutine. `initPipeline()` creates all processors and wires them up (called once via `sync.Once`) |
| `audio_source_processor.go` | `AudioSourceProcessor` — source processor (ignores `in`). `SetTrack(track)` starts a new reader goroutine. Decodes Opus→PCM (16kHz mono), emits `AudioFrame`s via internal channel. Old reader goroutines exit naturally on track EOF |
| `stt_processor.go` | `STTProcessor` — receives `AudioFrame`s, sends PCM to Soniox websocket, emits `TranscriptFrame`s (with `IsFinal` flag). Two background goroutines: websocket reader + audio writer. Contains `SonioxToken` and `SonioxResponseMessage` types |
| `llm_processor.go` | `LLMProcessor` — accumulates final `TranscriptFrame` tokens, triggers `runLLM` on `<end>`. Streams OpenAI response (gpt-4.1), emits `LLMResponseStartFrame`, `TextFrame`s (tokens), `LLMResponseEndFrame`. Maintains conversation history. Async bridge pattern |
| `tts_processor.go` | `TTSProcessor` — receives `TextFrame`s, aggregates into sentences (split on punctuation), sends to Cartesia with single `context_id` per turn (`continue: true`). PCM→Opus encoding (24kHz, 480 samples = 960 bytes per frame). Flushes remaining PCM with silence padding on Cartesia `"done"`. Sends `continue: false` on `LLMResponseEndFrame`. Async bridge pattern |
| `playback_sink_processor.go` | `PlaybackSinkProcessor` — creates and publishes LiveKit audio track. Writes `AudioFrame`s to track with 20ms ticker pacing |

#### Pipeline wiring:
```go
AudioSourceProcessor → STTProcessor → LLMProcessor → TTSProcessor → PlaybackSinkProcessor
```

### Pipeline Design Decisions

**Processor communication pattern:** Processors do NOT hold references to each other. All communication flows through `Process(ctx, in, out)`. For processors with background goroutines (websocket readers, etc.), use an **internal channel** to bridge async work back to the `Process` select loop:

```go
// Pattern: async bridge (TTS, STT, Audio Source)
type TTSProcessor struct {
    audioFrames chan Frame  // internal: background goroutine → Process loop
}

func (t *TTSProcessor) Process(ctx context.Context, in <-chan Frame, out chan<- Frame) {
    go t.readTTSConnectionData()  // background websocket reader
    for {
        select {
        case <-ctx.Done(): return
        case frame := <-in:          // handle TextFrame, control frames
        case frame := <-t.audioFrames: out <- frame  // forward async results
        }
    }
}
```

This is the Go equivalent of Pipecat's `push_frame()` + `asyncio.Queue` pattern. The `Process` select loop acts as a serialization point, which is safer than Pipecat's approach (Python asyncio is single-threaded; Go is not).

Two processor patterns exist:
1. **Synchronous**: frame in → process → frame out. No background goroutines needed.
2. **Async bridge** (TTS, STT, Audio Source, LLM): background goroutine writes to internal `chan Frame`, `Process` forwards to `out`.

**Cartesia TTS context strategy:** Single `context_id` per LLM turn (not per sentence). All sentences sent with `"continue": true`, final flush with `"continue": false`. This preserves prosodic continuity across sentences. One playback queue of interleaved audio frames and word timestamps. No context queue needed.

**Track re-subscription:** `AudioSourceProcessor.SetTrack(track)` starts a new reader goroutine. Old goroutine exits naturally on EOF when LiveKit closes the old track. No pipeline teardown needed — only the audio source swaps.

**Lazy pipeline initialization:** Pipeline is created on first `/connect` request (not at startup) via `sync.Once`. This avoids websocket idle timeouts on Soniox/Cartesia before any client connects.

### What Needs to Happen Next

#### Barge-in / Interruption (two passes):

**Pass 1 — Basic barge-in:**
1. **LLM Processor** detects barge-in: if it receives a `TranscriptFrame` while `isStreaming == true`, cancel the in-flight HTTP request (use `context.WithCancel`), send `InterruptFrame` downstream, add `responseText` accumulated so far as assistant message in history.
2. **TTS Processor** handles `InterruptFrame`: stop sending to Cartesia, clear `pcmBuffer`, clear `currentAggregation`, drain pending `audioFrames`, forward `InterruptFrame` to `out`.
3. **PlaybackSink Processor** handles `InterruptFrame`: stop playing immediately, discard buffered frames.

**Pass 2 — Accurate context via word timestamps:**
1. TTS pushes `WordTimestampFrame{Words []string}` through pipeline alongside audio frames.
2. PlaybackSink tracks which words were actually played based on interleaved timestamps.
3. On interrupt, only the actually-spoken words are added to LLM conversation history (instead of full `responseText` so far).

#### Cleanup:
- Consider renaming module from `awesomeProject`.

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
