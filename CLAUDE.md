# talk-go — Project Context for Claude Code

## About the User

Jaideep is an experienced Python backend engineer learning Go by building real projects. He understands concurrency concepts (threading, asyncio) but is new to Go syntax, channels, goroutines, and Go idioms. He maps Go concepts to Python equivalents to build mental models.

## How to Collaborate

- **Guide step by step, don't dump full code.** Explain the "why" alongside small code snippets. Let him write the code himself unless he explicitly asks you to make changes directly.
- **When he says "make the changes directly" or similar, do it.** Otherwise default to guiding.
- **Explain Go concepts by mapping to Python equivalents** (e.g., channels = queue.Queue, goroutines = lightweight threads, defer = try/finally, interfaces = duck typing but explicit).
- **Keep responses concise.** He can read code — don't summarize what you just did.
- **Don't over-engineer.** He will push back if something is unnecessarily complex. Prefer the simplest solution first.
- **When debugging, check `app.log` first** — all `log.*` output goes there. `fmt.Print*` goes to stdout/terminal.
- **After major design decisions or discussions, always update CLAUDE.md and memory.** Don't wait to be asked — if a design pattern, architecture choice, or strategy was decided in conversation, persist it immediately.

## Project Overview

A real-time voice assistant pipeline built in Go, using LiveKit for browser-based audio I/O:

```
Browser Mic → LiveKit Room → Go Bot (Opus decode → PCM) → Soniox STT → OpenAI LLM → Cartesia TTS → (PCM → Opus encode) → LiveKit Room → Browser Speaker
```

### Architecture

The Go server joins a LiveKit room as a "bot" participant. A browser client (`livekit-client.html`) joins the same room. The bot subscribes to the user's audio track, decodes Opus to PCM, sends it to Soniox for speech-to-text. When Soniox detects an endpoint (`<end>` token), the transcript is sent to OpenAI's streaming LLM. LLM tokens are accumulated into sentences (split on `.?!`), and each sentence is sent to Cartesia TTS. Cartesia returns raw PCM audio which is encoded to Opus and published back to the LiveKit room for the browser to play.

### Current State: Mid-Refactor to Pipeline Architecture

The project has a **working but non-pipeline version** (the files described below) AND the **beginnings of a pipeline/framework refactor** inspired by Pipecat. The pipeline refactor is IN PROGRESS.

#### Working (pre-pipeline) files:

| File | Purpose |
|------|---------|
| `main.go` | Entry point. Loads `.env`, redirects log to `app.log`, joins LiveKit room, serves HTTP (`:3000`) |
| `livekit_room.go` | Bot joins LiveKit room, subscribes to user audio track, decodes Opus→PCM via `hraban/opus`, sends PCM to Soniox. Manages track re-subscription with mutex. Globals: `room`, `botTrack`, `audioMu` |
| `livekit_token.go` | HTTP handler `/getToken` — generates LiveKit JWT tokens. Accepts GET (defaults) or POST (custom room/identity) |
| `livekit-client.html` | Browser client using `livekit-client` JS SDK. Connects to room, enables mic, plays remote audio tracks |
| `soniox_stt.go` | Soniox STT websocket. Config: s16le/16kHz/mono, English, endpoint detection with 300ms max delay. `readSTTWebsocketLoop` accumulates final tokens, calls `callLLM` on `<end>` |
| `openai_llm.go` | OpenAI streaming LLM (gpt-4.1). SSE parsing with `bufio.Scanner`. Sends tokens to TTS via buffered channel. Maintains conversation history in `messages` slice. System prompt: "Always respond in exactly 2 sentences." |
| `cartesia_tts.go` | Cartesia TTS websocket (Sonic-3). Sentence-level streaming — accumulates LLM tokens, splits on `.?!`, sends each sentence to Cartesia. Decodes base64 PCM, encodes to Opus via `hraban/opus`, writes to `botTrack` with 20ms ticker pacing. Flushes remaining PCM with silence padding |
| `audio_in.go` | OLD — ffmpeg mic capture (avfoundation). Not used in LiveKit version |
| `audio_out.go` | OLD — ffmpeg audio playback (audiotoolbox). Not used in LiveKit version |

#### Pipeline refactor files (IN PROGRESS):

| File | Purpose |
|------|---------|
| `frame.go` | Frame types: `Frame` interface with `FrameType()` method. Concrete types: `AudioFrame{Data []byte}`, `TextFrame{Text string}`, `InterruptFrame{}`, `LLMResponseStartFrame{}`, `LLMResponseEndFrame{}`. Constants: `Audio`, `Text`, `Interrupt` |
| `frame_processor.go` | `FrameProcessor` interface: `Process(ctx context.Context, in <-chan Frame, out chan<- Frame)` |
| `pipeline.go` | `Pipeline` struct with `NewPipeline([]FrameProcessor)` and `Run(ctx context.Context)`. Creates buffered channels between processors, starts each in a goroutine |
| `playback_sink_processor.go` | `PlaybackSinkProcessor` — creates and publishes LiveKit audio track. Process method: type-switches on `AudioFrame`, writes to track with 20ms ticker pacing. Handles `ctx.Done()` for cancellation |
| `tts_processor.go` | `TTSProcessor` — receives `TextFrame`s, aggregates into sentences, sends to Cartesia TTS websocket, receives PCM audio, encodes to Opus, emits `AudioFrame`s. Uses single context_id per LLM turn with `continue: true` for natural prosody. Tracks word timestamps for barge-in support |

#### Pipeline Design Decisions

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
1. **Synchronous** (STT, LLM): frame in → process → frame out. No background goroutines needed.
2. **Async bridge** (TTS, Audio Source): background goroutine writes to internal `chan Frame`, `Process` forwards to `out`.

**Cartesia TTS context strategy:** Single `context_id` per LLM turn (not per sentence). All sentences sent with `"continue": true`, final flush with `"continue": false`. This preserves prosodic continuity across sentences. One playback queue of interleaved audio frames and word timestamps. No context queue needed.

### What Needs to Happen Next

The pipeline refactor needs these processors implemented (in suggested order):

1. **Audio Source Processor** — reads from LiveKit `*webrtc.TrackRemote`, decodes Opus→PCM, pushes `AudioFrame`s. Ignores `in` channel (it's a source). Currently this logic lives in `readAudioTrack()` in `livekit_room.go`.

2. **STT Processor** — receives `AudioFrame`, sends PCM bytes to Soniox websocket, accumulates transcript, emits `TextFrame` on `<end>`. Currently in `soniox_stt.go`.

3. **LLM Processor** — receives `TextFrame` (full transcript), streams OpenAI response, accumulates sentences, emits `TextFrame` per sentence. Currently in `openai_llm.go`.

4. **TTS Processor** — DONE (`tts_processor.go`). Receives `TextFrame`s, aggregates into sentences (split on punctuation), sends to Cartesia with single context_id per turn (`continue: true`), receives PCM chunks, encodes to Opus, emits `AudioFrame`s via internal channel (async bridge pattern). Flushes remaining PCM with silence padding on Cartesia `"done"`. Sends `continue: false` on `LLMResponseEndFrame` to close context.

5. **Playback Sink Processor** — DONE (`playback_sink_processor.go`). Receives `AudioFrame`, writes to LiveKit bot track with pacing.

After all processors are migrated, wire them up in `main.go`:
```go
pipeline := NewPipeline([]FrameProcessor{audioSource, sttProcessor, llmProcessor, ttsProcessor, playbackSink})
pipeline.Run(ctx)
```

Then implement:
- **Barge-in / interruption**: When user speaks while bot is playing, cancel the pipeline context to stop all processors, flush channels, start a new turn. `InterruptFrame` propagates through the pipeline. Word timestamps track what was actually spoken — only spoken words are added to LLM conversation history.
- **Word timestamps**: Cartesia word timestamps are interleaved with audio frames in the playback queue. On barge-in, the "actually spoken" buffer determines what goes into LLM history.

### Key Technical Details

- **Audio formats**: Soniox expects s16le/16kHz/mono. Cartesia outputs s16le/24kHz/mono. LiveKit/WebRTC uses Opus at 48kHz clock rate (always signaled as stereo in SDP).
- **Opus decode**: `opus.NewDecoder(16000, 1)` — decodes directly to 16kHz for Soniox.
- **Opus encode**: `opus.NewEncoder(24000, 1, opus.AppVoIP)` — encodes 24kHz Cartesia PCM. Frame size: 480 samples (20ms at 24kHz) = 960 bytes.
- **LiveKit track publish**: `ClockRate: 48000, Channels: 2` in RTP capability (WebRTC requirement), even though actual audio is 24kHz mono.
- **Pacing**: Opus frames must be sent at real-time pace (one 20ms frame per tick) or the browser jitter buffer overflows and drops audio.
- **Websocket idle timeouts**: Soniox and Cartesia close connections after ~15-20s of no data. Init them when the user connects (in `onTrackSubscribed`), not at startup.
- **Track re-subscription**: LiveKit fires `onTrackSubscribed` multiple times during renegotiation. Use a mutex to close old connections and start fresh.
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

### Build & Run

```bash
brew install opus opusfile pkg-config  # one-time
go build ./...
./awesomeProject  # or: go run .
# Open http://localhost:3000, click Connect
```

### Known Issues / Gotchas

- Audio can sound choppy if Opus frames aren't paced at 20ms intervals — use `time.Ticker`, not `time.Sleep`
- The `SentenceFrame` type was removed from `frame.go` — sentences are now `TextFrame`s. The LLM processor should handle sentence boundary detection.
- `audio_in.go` and `audio_out.go` are legacy files from the pre-LiveKit ffmpeg approach. Can be deleted once the pipeline refactor is complete.
- The go.mod module name is still `awesomeProject` (from the original learning project). The binary is named `awesomeProject`.
