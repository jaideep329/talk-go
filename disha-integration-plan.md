# talk-go — Disha integration + core library split

**Audience**: A coding agent (or human) who has filesystem access to:
- `/Users/jaideepsingh/Projects/talk-go/` (this repo)
- `/Users/jaideepsingh/Projects/disha-backend/` (the Python reference)

You have no prior conversation context. This document is self-contained. Read it top to bottom before writing any code.

**Do NOT commit anything.** The user (Jaideep) wants to review each phase locally before commits. Work in the working tree, run tests after each phase, and stop after the phase boundaries listed in §13 unless explicitly told to continue.

---

## 1. Why this work exists

`talk-go` is a real-time voice-call pipeline in Go (LiveKit transport + Soniox STT + OpenAI LLM + Cartesia TTS) that mirrors Pipecat's architecture in Python. It currently runs end-to-end as a standalone demo: a browser hits `/connect`, the bot joins the same LiveKit room, the user talks to the bot.

The next step is to integrate `talk-go` into the existing **Disha** product backend (Python, lives at `/Users/jaideepsingh/Projects/disha-backend/`), specifically replacing the **`sales_call`** Pipecat bot. Disha already has all the surrounding infrastructure for sales calls: scheduling, Redis-backed conversation state, Postgres persistence, dashboard analytics, post-call STM generation, talktime accounting. We do **not** rebuild any of that in Go.

What we add to `talk-go`:
1. The ability to load a conversation's prior state from Disha's Redis at call start.
2. The ability to persist conversation chunks to Disha's Redis during the call.
3. The ability to fire the same HTTP lifecycle callbacks Disha's Python bot fires (`update_conversation` at three moments, `run_post_call_operations` at the end, `enqueue_job` to trigger the Redis→Postgres chunk sync).

**Crucially**, the user wants all of this expressed as a clean library/integration split:

> The pipeline must remain free of business logic. All Disha-specific code (Redis, HTTP, conversation IDs, prompts, end-reason policies) lives in a separate package and hooks into the core via callbacks and observers. Mirror Pipecat's pattern where `sales_call.py` wires event handlers onto core processors without modifying the core.

If you find yourself writing `import "talk-go/disha"` inside the core package, stop and add a callback instead.

---

## 2. Current repo state (pre-work snapshot)

### Module
- `go.mod` module name: `awesomeProject` (legacy; **rename in phase 0** — see §4).
- Go 1.24.4.
- `github.com/redis/go-redis/v9` is already in go.sum as a transitive dep; promote to a direct dep.

### Layout (today, before this work)
All `.go` files at the repo root in `package main`. The relevant files:

```
audio_source_processor.go    Decodes LiveKit/Opus → PCM, pushes AudioFrame downstream
context_aggregator.go        Owns conversation messages, transcripts, barge-in detection
frame.go                     Frame interface + FrameMeta + all concrete frame types + constructors
frame_test.go                Tests for FrameMeta
helpers_test.go              Test fixture (testFixture, runProcessorTest, QueueProcessor)
livekit_room.go              joinRoom() + generateToken()
llm_processor.go             OpenAI streaming LLM
main.go                      HTTP server: /connect, /ws, /
metrics.go                   ProcessorMetrics + MetricsFrame
pipeline.go                  Pipeline, TaskContext, PipelineTask, sessions map, createSession
pipeline_edges.go            PipelineSourceProcessor, PipelineSinkProcessor
playback_sink_processor.go   Bot audio publishing to LiveKit + silence tail
processor.go                 BaseProcessor + Processor interface
stt_processor.go             Soniox STT websocket
talktime_monitoring_processor.go  Max-talk-time timer
tts_processor.go             Cartesia TTS websocket
ui_events.go                 Frontend WebSocket broadcaster
user_idle_processor.go       Idle prompt injection
*_test.go                    One test file per processor + frame_test.go + processor_test.go
```

### Key types in the current code (so you can find them quickly)

- `Frame` interface (`frame.go:62-69`): `ID() int64; Name() string; FrameType() FrameType; IsSystem() bool; IsInterruptible() bool`. All concrete frames embed `FrameBase{Meta FrameMeta}` and have `NewXxxFrame(...)` constructors. Tests still allow struct literals (zero meta).
- `Processor` interface (`processor.go:33`): `Name; Prev; Next; Link; SetPrev; QueueFrame; ProcessFrame; PushFrame; Broadcast; Start; Stop`.
- `BaseProcessor` (`processor.go:92`): inputSysCh + inputDataCh + procCh + per-processor ctx + interrupt cancel-and-recreate. Every processor embeds `*BaseProcessor`.
- `TaskContext` (`pipeline.go:52`): `Ctx, Logger, Room, UIEvents, Metrics func(MetricsFrame), EndTask func(reason string), wg *sync.WaitGroup`.
- `PipelineTask` (`pipeline.go:64`): owns `TaskCtx, Cancel, Source *PipelineSourceProcessor, Pipeline *Pipeline, RoomName, wg sync.WaitGroup, endRequested, cleanupStarted, cleanupOnce`. `End(reason)` queues an EndFrame at the source. `completeEnd(frame)` runs the cleanup once when the sink sees EndFrame.
- Global `sessions` map at `pipeline.go:77` plus `getSession`/`removeSession`/`createSession`.

### Current `/connect` flow (`main.go:35-51`)
```go
func handleConnect(w http.ResponseWriter, r *http.Request) {
    roomName, _ := createSession()           // mints "room-<rand>", builds pipeline, starts it
    token, _ := generateToken(roomName, "web-user")
    json.NewEncoder(w).Encode(map[string]string{
        "server_url": os.Getenv("LIVEKIT_URL"),
        "token":      token,
        "room_name":  roomName,
    })
}
```

### Pipeline composition (current; from `pipeline.go:141-152`)
```
PipelineSource → AudioSource → STT → UserIdle → ContextAggregator → TalkTimeMonitor → LLM → TTS → PlaybackSink → PipelineSink
```

### Key existing design decisions (carry over unchanged)
- **EndFrame is a data frame, not a system frame.** It travels through the pipeline in order. Processors with async work (TTS) defer it until safe.
- **InterruptFrame is a system frame.** BaseProcessor cancels procCtx, drains procCh, recreates.
- **MetricsFrame is intercepted by `BaseProcessor.PushFrame`** and delivered to `taskCtx.Metrics` — never reaches downstream processors. (We'll add a second consumer in §6.)
- **`taskCtx.EndTask(reason)`** is the canonical way for any processor to request graceful task shutdown. Don't push EndFrame downstream directly.
- **`PipelineTask.completeEnd`** runs exactly once via `sync.Once`, calls `Pipeline.Stop()`, disconnects LiveKit room, waits 10s on the shared WG, then cancels and removes the session.
- **STT/TTS have `Stop()` overrides** that close their websockets in addition to cancelling ctx — required so blocking `ReadMessage` exits during cleanup.

### Existing AGENTS.md
The repo has an AGENTS.md at the root with a full architecture description. **Update it as part of phase 0 with the new core/integration rule** — exact text in §14.

---

## 3. Disha backend contracts (read this once, then keep open as reference)

All file:line citations are in `/Users/jaideepsingh/Projects/disha-backend/`.

### 3.1 HTTP endpoints we will call as a CLIENT

#### `PATCH /bot/update_conversation`
**Source**: `bots/views.py:62-67` (request model), `bots/views.py:575-578` (handler).

Request body:
```json
{
  "conversation_id": "uuid string, REQUIRED",
  "bot_joined_at":   "2026-05-22T13:45:00+05:30 or null",
  "user_joined_at":  "iso8601 or null",
  "user_first_speech_at": "iso8601 or null",
  "bot_first_speech_at":  "iso8601 or null"
}
```
- All timestamp fields are optional. Send only the one(s) you have at each lifecycle moment.
- 10 second timeout in the Python client; replicate that.
- 200 OK on success; non-200 should be logged but must not crash the pipeline.

#### `POST /bot/run_post_call_operations`
**Source**: `bots/views.py:70-81` (request model), `bots/views.py:581-584` (handler), `bots/sales_call/sales_call.py:452-461` (call site).

Request body:
```json
{
  "conversation_id": "uuid REQUIRED",
  "end_reason": "talktime_exhausted | client_disconnected | user_idle | null",
  "total_user_duration": 123,
  "first_user_audio_frames_received_at": "iso8601 or null",
  "ended_at": "iso8601 REQUIRED",
  "log_data_s3_key": "" ,
  "onboarding_call_done": false,
  "diet_plan_intensity_level": null,
  "fitness_plan_intensity_level": null,
  "latest_onboarding_call_stage": null,
  "conversation_variables": null
}
```
- `log_data_s3_key` is REQUIRED by the schema but accepts empty string. We send `""` for now (no RTVI port).
- `onboarding_call_done` always `false` for sales call.
- All `*_intensity_level`, `latest_onboarding_call_stage`, `conversation_variables` always `null` for sales call.
- This endpoint is server-side **idempotent** (Disha uses `post_call_operations_started_at`). Safe to retry once on transient failure.

#### `POST /bot/enqueue_job`
**Source**: Need to verify exact path — search for the endpoint that enqueues SQS jobs from outside. Likely at `bots/views.py` or `common/`. Use `Grep` for `enqueue_job` or `queue_job` decorated as an HTTP route.

If no such endpoint exists yet on Disha's side, **stop and tell the user**. The user said in the spec:
> "For the chunk sync trigger, we can use the `enqueue_job` endpoint to enqueue a job for chunk syncing."

So either:
- (a) the endpoint exists and you can find it,
- (b) the user needs to add it on Disha side — flag this back to them with the exact contract we expect:

```json
POST /bot/enqueue_job
{
  "func": "sync_conversation_chunks_to_db",
  "kwargs": {
    "user_id": "uuid",
    "conversation_id": "uuid",
    "bot_type": "sales_call"
  },
  "queue_name": "p1_fast_l1",
  "tracking_type": "LAZY"
}
```

Mirror of `bots/onboarding_call/conversation_persistence_processor.py:147-159` — that's the Python side that today enqueues via direct SQS, which we want to expose as an HTTP endpoint we can call from Go.

#### Disha API auth
All three endpoints are authenticated. Read `DISHA_API_AUTH_TOKEN` from env and send `Authorization: Bearer ${DISHA_API_AUTH_TOKEN}`. Confirm header format with the user if the Python client uses a different scheme — check `bots/operations/voice_bot_operations.py` or wherever `VoiceBotAPIService` lives.

### 3.2 Redis keys we read/write

#### `conversation_data:{conversation_id}` (READ at session start, written by Disha orchestrator)
**Source**: `bots/bot_session_manager.py:406-419`.

Value (JSON-encoded):
```json
{
  "conversation": {
    "id": "uuid",
    "user_id": "uuid",
    "bot_type": "sales_call",
    "resumed_from_chunk_id": "uuid or null",
    "start_chunk_id": "uuid or null",
    "end_chunk_id":   "uuid or null",
    "...": "other CallConversation columns"
  },
  "chunks": [
    ["chunk_id_uuid", "user", "text content", false, null],
    ["chunk_id_uuid", "assistant", "text content", false, null]
  ],
  "user_profile": {
    "user_id": "uuid",
    "phone":   "+91...",
    "...":     "other UserProfile columns"
  },
  "unprocessed_chat_context": "optional, present for non-onboarding bots",
  "resumed_chunk": "optional ConversationChunk dict if resumed_from_chunk_id set"
}
```

Notes:
- `chunks` is an array of 5-tuples: `[id, role, text, is_debug_log, additional_data]`.
- For sales call we only need: `conversation.id`, `conversation.user_id`, `conversation.bot_type`, `conversation.resumed_from_chunk_id`, `chunks[]` (filtered by `is_debug_log==false` and role in {`user`, `assistant`}), `user_profile.user_id`, optionally `resumed_chunk`.
- TTL: 7200 seconds. If the key is missing, **return 502** from `/connect` — do not fall back to Postgres (the user explicitly said no Postgres in Go).

#### `conversation_chunks:{user_id}:{conversation_id}` (WRITE per turn; READ never)
**Source**: `services/conversation_chunks_cache.py:23, 60-71`.

Redis LIST. We `RPUSH` one JSON-encoded chunk per committed turn. Schema (21 keys, exact match to `bots/onboarding_call/conversation_persistence_processor.py:113-132`):

```json
{
  "id": "uuid v4 minted by us",
  "text": "the message text",
  "role": "user | assistant",
  "bot_type": "sales_call",
  "conversation_id": "uuid",
  "user_id": "uuid",
  "current_agenda": null,
  "stt_ttfb_ms": null,
  "llm_ttfb_ms": 123.4,
  "tts_ttfb_ms": 234.5,
  "v2v_latency_ms": 800.1,
  "text_aggregation_ms": 15.2,
  "created": "2026-05-22T13:45:01.123456+05:30",
  "is_debug_log": false,
  "additional_data": null,
  "main_agent_system_prompt_langfuse_key": null,
  "system_message_params_s3_key": null,
  "conversation_state_s3_key": null
}
```

Field rules for our Go writer:
- `id`: fresh `uuid.NewString()` per chunk.
- `text`, `role`, `created`: from the commit event.
- `bot_type`: always `"sales_call"`.
- `conversation_id`, `user_id`: from the session.
- `current_agenda`: always `null` (sales call doesn't have agenda state).
- `stt_ttfb_ms`: always `null` (we don't measure STT TTFB today).
- `llm_ttfb_ms`, `tts_ttfb_ms`, `text_aggregation_ms`, `v2v_latency_ms`: populated only on the assistant chunk from the per-turn metrics buffer. `null` on user chunks.
- `is_debug_log`: always `false` (sales call doesn't persist debug logs).
- `additional_data`: always `null`.
- `main_agent_system_prompt_langfuse_key`: always `null` (no Langfuse port).
- `system_message_params_s3_key`, `conversation_state_s3_key`: always `null` (no S3 port).

Retry policy in the Python client: 3 attempts with exponential backoff on `redis.exceptions.TimeoutError`. Replicate this in Go with go-redis's built-in retry plus a wrapper.

### 3.3 End-of-call ordering (mirror exactly)

From `bots/sales_call/sales_call.py:413-487`:

1. (Skipped in our port — RTVI logs to S3.)
2. `POST /bot/run_post_call_operations` (HTTP, 10s timeout).
3. (Skipped — we don't use Daily transport, so no Daily metrics queue.)
4. Pipeline shutdown (`task.cancel()` in Python; `Pipeline.Stop()` + `wg.Wait` in Go — already implemented).
5. After pipeline returns, enqueue the chunk-sync job via `POST /bot/enqueue_job`.

If you flip the order, you can race Redis-LIST deletion against in-flight chunk writes.

### 3.4 EndReason mapping

Disha's canonical end reasons (find in `bots/` — search for `CallConversationEndReason`):
- `talktime_exhausted` — talk time hit max
- `client_disconnected` — user disconnected the WebSocket / left the room
- `user_idle` — idle timeout fired N times (we don't end on idle today, but reserve)

Our internal Go strings (set today):
- `"talk time exhausted"` (from `talktime_monitoring_processor.go:13`)
- `"ui websocket disconnected"` (from `main.go:72`)
- `"ui requested end call"` (from `main.go:84`)

Translate in the Disha package before sending the post-call payload. Map:
```
"talk time exhausted"           → "talktime_exhausted"
"ui websocket disconnected"     → "client_disconnected"
"ui requested end call"         → "client_disconnected"
(any unknown)                   → null  (omit from payload)
```

---

## 4. Phase 0 — Module rename and package split (mechanical, biggest mechanical change)

### 4.1 Module rename

In `go.mod`:
```
- module awesomeProject
+ module github.com/jaideep329/talk-go
```

Run `go mod tidy` after.

Also delete the leftover binary `awesomeProject` at the repo root (it's a build artifact) and add it to `.gitignore` if not already excluded.

### 4.2 Package split — target layout

```
talk-go/
├── go.mod                           # module github.com/jaideep329/talk-go
├── go.sum
├── main.go                          # HTTP server, sessions map, wiring
├── livekit-client.html              # unchanged
├── background-office-sound.mp3      # unchanged
├── AGENTS.md                        # updated (see §14)
├── disha-integration-plan.md        # this file
├── voicepipelinecore/
│   ├── frame.go
│   ├── processor.go
│   ├── pipeline.go                  # just the Pipeline struct (chain + Start/Stop)
│   ├── task.go                      # NEW: TaskContext, PipelineTask, TaskOptions, NewTask
│   ├── observer.go                  # NEW: TaskObserver interface + dispatch worker
│   ├── pipeline_edges.go
│   ├── livekit_room.go              # joinRoom + GenerateToken (capitalized)
│   ├── ui_events.go
│   ├── metrics.go
│   ├── audio_source_processor.go
│   ├── stt_processor.go
│   ├── user_idle_processor.go
│   ├── context_aggregator.go        # accepts initialMessages + dispatches observers
│   ├── talktime_monitoring_processor.go
│   ├── llm_processor.go
│   ├── tts_processor.go
│   ├── playback_sink_processor.go
│   └── *_test.go                    # all tests move here
└── disha/                           # added in phase 2+
    └── (created later)
```

### 4.3 Mechanical migration steps

1. `mkdir voicepipelinecore`
2. `git mv` every `.go` file (production + test) at the root into `voicepipelinecore/`, EXCEPT:
   - Keep `main.go` at the root (you'll rewrite it).
   - Do not move `livekit-client.html`, `background-office-sound.mp3`, `app.log`, the binary, `.env`, etc.
3. In every moved file, change `package main` → `package voicepipelinecore`.
4. The current `main.go` at the root needs full rewrite — you'll do this in §4.5.
5. Re-export anything `main.go` (or future `disha/`) will need. Quick checklist of probable renames (verify against the actual code):
   - `sessions`, `sessionsMu`, `getSession`, `removeSession` — these move OUT of voicepipelinecore entirely (into `main.go`). See §4.4.
   - `createSession` — gets replaced by `voicepipelinecore.NewTask(ctx, opts)` (see §6).
   - `loadEnv` — stays in `main.go` (it's a binary-level concern).
   - `joinRoom` → `JoinRoom`, `generateToken` → `GenerateToken`. These are now public API.
   - `upgrader` (gorilla websocket) — currently used by the WebSocket handler. Move the upgrader definition to `main.go` since `/ws` lives there.
   - Anything in `ui_events.go` that's only consumed inside the package can stay unexported. `UIEventSender` is already exported.
6. `taskCtx` (lowercase field on `BaseProcessor`) stays unexported — only used inside the core package.
7. `cancel` field on `BaseProcessor` (lowercase) used by `PipelineSourceProcessor.drainExternalFrames` (`pipeline_edges.go:57`) — stays unexported (same package, fine).

### 4.4 Sessions map moves to `main.go`

Today (`pipeline.go:76-91`):
```go
var (
    sessions   = map[string]*PipelineTask{}
    sessionsMu sync.Mutex
)

func getSession(roomName string) *PipelineTask { ... }
func removeSession(roomName string) { ... }
```

After the split, `voicepipelinecore` doesn't know about a sessions registry. That's a `main.go` concern (it's how the binary looks up tasks for the `/ws` handler and for cleanup-on-removal).

In `main.go`:
```go
var (
    sessions   = map[string]*voicepipelinecore.PipelineTask{}
    sessionsMu sync.Mutex
)
```

`PipelineTask.completeEnd` currently calls `removeSession(t.RoomName)` (`pipeline.go:212`). After the split it should call a callback passed via `TaskOptions.OnCleanup` instead, so the core doesn't know about the registry. Or, simpler: expose a `Task.OnCleanup func()` field that `main.go` populates with a closure that does `delete(sessions, task.RoomName)`. I recommend the latter — it's one field.

### 4.5 New `main.go` skeleton (post-phase-0; will be filled in further in phase 5)

```go
package main

import (
    "context"
    "encoding/json"
    "io"
    "log"
    "net/http"
    "os"
    "sync"

    "github.com/go-logr/stdr"
    "github.com/gorilla/websocket"
    protoLogger "github.com/livekit/protocol/logger"
    lksdk "github.com/livekit/server-sdk-go/v2"

    "github.com/jaideep329/talk-go/voicepipelinecore"
)

var (
    sessions   = map[string]*voicepipelinecore.PipelineTask{}
    sessionsMu sync.Mutex

    upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
)

func main() {
    loadEnv(".env")
    appLog, _ := os.Create("app.log")
    log.SetOutput(io.MultiWriter(os.Stderr, appLog))
    log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
    stdr.SetVerbosity(0)
    lksdk.SetLogger(protoLogger.LogRLogger(stdr.New(log.New(io.Discard, "", 0))))

    http.HandleFunc("/connect", handleConnect)
    http.HandleFunc("/ws", handleWebSocket)
    http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        http.ServeFile(w, r, "livekit-client.html")
    })
    log.Println("HTTP server listening on :3000")
    if err := http.ListenAndServe(":3000", nil); err != nil {
        log.Fatal(err)
    }
}

// handleConnect — phase 0 version: keeps today's behavior. Phase 5 will
// upgrade it to read conversation_id and dispatch to disha.BuildSession.
func handleConnect(w http.ResponseWriter, r *http.Request) {
    logger := log.New(log.Writer(), "[room] ", log.Flags())

    opts := voicepipelinecore.TaskOptions{
        Logger:          logger,
        InitialMessages: []voicepipelinecore.Message{
            {Role: "system", Content: defaultDevSystemPrompt},
        },
    }
    task, err := voicepipelinecore.NewTask(context.Background(), opts)
    if err != nil {
        http.Error(w, err.Error(), 500)
        return
    }

    task.OnCleanup = func() {
        sessionsMu.Lock(); delete(sessions, task.RoomName); sessionsMu.Unlock()
    }

    sessionsMu.Lock(); sessions[task.RoomName] = task; sessionsMu.Unlock()

    if err := task.Start(); err != nil { http.Error(w, err.Error(), 500); return }

    token, err := voicepipelinecore.GenerateToken(task.RoomName, "web-user")
    if err != nil { http.Error(w, err.Error(), 500); return }

    json.NewEncoder(w).Encode(map[string]string{
        "server_url": os.Getenv("LIVEKIT_URL"),
        "token":      token,
        "room_name":  task.RoomName,
    })
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
    roomName := r.URL.Query().Get("room")
    sessionsMu.Lock(); task := sessions[roomName]; sessionsMu.Unlock()
    if task == nil { http.Error(w, "unknown room", 404); return }

    conn, err := upgrader.Upgrade(w, r, nil)
    if err != nil { return }
    task.TaskCtx.UIEvents.AddClient(conn)
    for {
        _, msg, err := conn.ReadMessage()
        if err != nil {
            task.TaskCtx.UIEvents.RemoveClient(conn)
            task.End(voicepipelinecore.EndReasonClientDisconnect)
            return
        }
        var ev struct{ Type string `json:"type"` }
        json.Unmarshal(msg, &ev)
        if ev.Type == "end_call" {
            task.End(voicepipelinecore.EndReasonClientDisconnect)
        }
    }
}

const defaultDevSystemPrompt = `You are an expert health coach...` // copy from context_aggregator.go:135-138
```

Note: `task.End` now takes an `EndReason` (enum), not a free-form string. See §6.

### 4.6 Phase 0 verification

```bash
go mod tidy
go build ./...
go test -race -count=2 ./...
./talk-go            # binary name follows module name after rename; or `go run .`
# In another terminal:
curl -XPOST http://localhost:3000/connect
# Then open http://localhost:3000 in a browser, click Connect, talk to the bot.
```

All 41 tests should still pass. The browser flow should behave identically to before.

---

## 5. Phase 1 — Public API surface (the contract)

No new behavior. Just rearrange what's already there so the public surface of `voicepipelinecore` is intentional.

### 5.1 `voicepipelinecore.TaskOptions`

In a new file `voicepipelinecore/task.go`:

```go
package voicepipelinecore

import (
    "context"
    "log"
    "time"
)

// TaskOptions configures one voice session. Pass to NewTask.
type TaskOptions struct {
    // Logger is required.
    Logger *log.Logger

    // RoomName is the LiveKit room name. If empty, NewTask mints one
    // like "room-1234567".
    RoomName string

    // InitialMessages seed the conversation history. The first entry
    // should be a system prompt. If empty, the ContextAggregator
    // starts with no messages and prompt injection becomes the
    // caller's responsibility on first user message.
    InitialMessages []Message

    // MaxTalkTime overrides the talk-time monitor's default if non-zero.
    MaxTalkTime time.Duration

    // ---- Lifecycle callbacks. All nil-safe. Fire at most once per
    // session unless noted. Callbacks may do I/O; they run in their
    // own goroutines (tracked by the task's WaitGroup), so the
    // pipeline is never blocked by user code.

    OnBotJoined       func(at time.Time)                    // after LiveKit Connect succeeds
    OnUserJoined      func(at time.Time)                    // first non-bot LiveKit participant connects
    OnUserFirstSpeech func(at time.Time)                    // first finalized user transcript text
    OnBotFirstSpeech  func(at time.Time)                    // first bot audio frame written to wire
    OnFirstUserAudio  func(at time.Time)                    // first non-silence audio frame received
    OnCallEnded       func(reason EndReason, stats CallStats) // exactly once, after pipeline drain

    // OnCleanup runs after OnCallEnded, after all goroutines exit.
    // Use this to remove the task from binary-level registries.
    OnCleanup func()

    // Observers receive per-turn data. Each observer is dispatched on
    // its own goroutine with a FIFO queue (capacity 256). The
    // pipeline never blocks on observer work, but per-observer call
    // ordering is preserved.
    Observers []TaskObserver
}

// Message is one entry in the LLM conversation history.
type Message struct {
    Role    string // "system" | "user" | "assistant"
    Content string
}

// EndReason describes why a session ended.
type EndReason string

const (
    EndReasonUnspecified       EndReason = "unspecified"
    EndReasonTalkTimeExhausted EndReason = "talk_time_exhausted"
    EndReasonClientDisconnect  EndReason = "client_disconnected"
    EndReasonUserIdle          EndReason = "user_idle"
    EndReasonError             EndReason = "error"
)

// CallStats are filled in by the core and passed to OnCallEnded.
type CallStats struct {
    TotalUserDurationSec  int
    FirstUserAudioFrameAt *time.Time
    EndedAt               time.Time
}
```

### 5.2 `voicepipelinecore.TaskObserver`

In `voicepipelinecore/observer.go`:

```go
package voicepipelinecore

import (
    "log"
    "sync"
    "time"
)

// TaskObserver receives per-turn data. Implementations should be
// safe to call from a single dedicated goroutine (the core gives
// each observer its own worker).
type TaskObserver interface {
    OnUserCommitted(text string, at time.Time)
    OnAssistantCommitted(text string, at time.Time, m TurnMetrics)
}

// TurnMetrics are collected during one assistant turn. STT TTFB is
// intentionally absent — we don't measure it today.
type TurnMetrics struct {
    LLMTtfbMs         *float64
    TTSTtfbMs         *float64
    TextAggregationMs *float64
    V2VLatencyMs      *float64
}

// observerWorker wraps one TaskObserver with a FIFO queue + goroutine.
// Constructed by startObserverWorker. The pipeline submits calls via
// dispatch; the worker drains them in order. On ctx cancellation, the
// worker drains pending calls best-effort and exits.
type observerWorker struct {
    obs    TaskObserver
    queue  chan func(TaskObserver)
    logger *log.Logger
}

const observerQueueCap = 256

func startObserverWorker(taskCtx *TaskContext, obs TaskObserver) *observerWorker {
    w := &observerWorker{
        obs:    obs,
        queue:  make(chan func(TaskObserver), observerQueueCap),
        logger: taskCtx.Logger,
    }
    taskCtx.wg.Add(1)
    go func() {
        defer taskCtx.wg.Done()
        for {
            select {
            case <-taskCtx.Ctx.Done():
                // Best-effort drain so any commits queued just before
                // shutdown still reach the observer.
                for {
                    select {
                    case fn := <-w.queue:
                        w.run(fn)
                    default:
                        return
                    }
                }
            case fn := <-w.queue:
                w.run(fn)
            }
        }
    }()
    return w
}

func (w *observerWorker) run(fn func(TaskObserver)) {
    defer func() {
        if r := recover(); r != nil {
            w.logger.Printf("observer panic: %v", r)
        }
    }()
    fn(w.obs)
}

func (w *observerWorker) dispatch(fn func(TaskObserver)) {
    select {
    case w.queue <- fn:
    default:
        w.logger.Printf("observer queue full (cap=%d), dropping callback", observerQueueCap)
    }
}

// observerSet bundles all observers for a task. Hung off TaskContext.
type observerSet struct {
    mu      sync.Mutex
    workers []*observerWorker
}

func (s *observerSet) emitUserCommitted(text string, at time.Time) {
    s.mu.Lock(); defer s.mu.Unlock()
    for _, w := range s.workers {
        w.dispatch(func(o TaskObserver) { o.OnUserCommitted(text, at) })
    }
}

func (s *observerSet) emitAssistantCommitted(text string, at time.Time, m TurnMetrics) {
    s.mu.Lock(); defer s.mu.Unlock()
    for _, w := range s.workers {
        w.dispatch(func(o TaskObserver) { o.OnAssistantCommitted(text, at, m) })
    }
}
```

### 5.3 `voicepipelinecore.PipelineTask` (revised constructor)

Replace the current `createSession()` flow with `NewTask(ctx, opts)`.

```go
// NewTask builds a PipelineTask but does not start it. The returned
// task has TaskCtx populated and Pipeline assembled. Call Start to
// begin the call.
func NewTask(parentCtx context.Context, opts TaskOptions) (*PipelineTask, error) {
    if opts.Logger == nil {
        return nil, errors.New("voicepipelinecore: TaskOptions.Logger is required")
    }
    roomName := opts.RoomName
    if roomName == "" {
        roomName = fmt.Sprintf("room-%d", rand.IntN(9000000)+1000000)
    }

    ctx, cancel := context.WithCancel(parentCtx)
    uiEvents := NewUIEventSender(opts.Logger)

    task := &PipelineTask{
        Cancel:     cancel,
        RoomName:   roomName,
        endReason:  EndReasonUnspecified,
        onCallEnded: opts.OnCallEnded,
        onCleanup:  opts.OnCleanup,
    }

    metricsHandler := func(mf MetricsFrame) {
        // Existing UI dispatch + log...
        for _, d := range mf.Data {
            opts.Logger.Printf("Metric [%s] %s: %.1fms\n", d.Processor, d.Label, d.ValueMs)
            uiEvents.Send(UIEvent{Type: Metrics, Data: map[string]interface{}{
                "processor": d.Processor, "label": string(d.Label), "value_ms": d.ValueMs,
            }})
        }
        // NEW: also feed the per-turn aggregator. See §6.
        task.metrics.absorb(mf)
    }

    taskCtx := &TaskContext{
        Ctx:      ctx,
        Logger:   opts.Logger,
        UIEvents: uiEvents,
        Metrics:  metricsHandler,
        EndTask:  func(reason string) { task.End(mapReason(reason)) },
        wg:       &task.wg,
        // Lifecycle fire-once helpers (see §6).
        lifecycle: newLifecycle(opts),
        observers: newObserverSetFromOptions(taskCtx /*self*/, opts.Observers), // bootstrap quirk; you'll handle
    }
    task.TaskCtx = taskCtx

    // ... unchanged pipeline assembly ...
    pipelineSource := NewPipelineSourceProcessor(taskCtx)
    audioSource    := NewAudioSourceProcessor(taskCtx)
    taskCtx.Room    = JoinRoom(roomName, audioSource)

    // NEW: ContextAggregator takes initialMessages.
    contextAggregator := NewContextAggregator(taskCtx, opts.InitialMessages)

    sttProcessor := NewSTTProcessor(taskCtx)
    userIdle     := NewUserIdleProcessor(taskCtx)
    talkTime     := NewTalkTimeMonitoringProcessor(taskCtx) // honor opts.MaxTalkTime
    if opts.MaxTalkTime > 0 {
        talkTime = NewTalkTimeMonitoringProcessorWithMaxTalkTime(taskCtx, opts.MaxTalkTime)
    }
    llmProcessor := NewLLMProcessor(taskCtx)
    ttsProcessor := NewTTSProcessor(taskCtx)
    playbackSink := NewPlaybackSinkProcessor(taskCtx)
    pipelineSink := NewPipelineSinkProcessor(taskCtx, task.completeEnd)
    task.Source = pipelineSource
    task.Pipeline = NewPipeline([]Processor{
        pipelineSource, audioSource, sttProcessor, userIdle,
        contextAggregator, talkTime, llmProcessor, ttsProcessor,
        playbackSink, pipelineSink,
    })

    return task, nil
}

func (t *PipelineTask) Start() error {
    t.Pipeline.Start(t.TaskCtx.Ctx)
    return nil
}

// End requests graceful shutdown. The previous internal string-based
// End(reason string) is kept as endInternal for the EndTask closure;
// the public API takes the enum.
func (t *PipelineTask) End(reason EndReason) {
    t.endReason = reason
    t.endInternal(string(reason))
}
```

`mapReason` translates legacy free-form strings (from the existing `taskCtx.EndTask(reason string)` callers — TalkTimeMonitor uses `"talk time exhausted"`) into the enum. Or you can change every `EndTask` caller to use the enum directly; that's cleaner — pick that.

### 5.4 What stays the same

- `Pipeline`, `Processor`, `BaseProcessor`, `Direction`, `Envelope`, `Frame`, every concrete frame type, every processor's internal logic.
- `EndFrame` plumbing through the pipeline.
- `InterruptFrame` cancel-and-recreate.
- `MetricsFrame` interception in `BaseProcessor.PushFrame`.
- All the websocket reconnection logic in STT/TTS.

### 5.5 Phase 1 verification

- Existing tests pass unchanged.
- New `task_test.go`:
  - Build a task with `OnCallEnded` and observers, send a synthetic EndFrame, verify the callback fires with reason and stats.
  - Build a task with an observer, send a `UserCommittedFrame`-equivalent (call the aggregator commit path directly via a test), verify the observer's `OnUserCommitted` fires.

---

## 6. Phase 1.5 — Lifecycle hooks + per-turn metrics aggregation (still in voicepipelinecore)

These are part of the public callback contract but distinct mechanical chunks.

### 6.1 Fire-once lifecycle helper

Add `lifecycle` to `TaskContext`:

```go
// In task.go
type lifecycle struct {
    botJoined        sync.Once
    userJoined       sync.Once
    userFirstSpeech  sync.Once
    botFirstSpeech   sync.Once
    firstUserAudio   sync.Once
    onBotJoined        func(time.Time)
    onUserJoined       func(time.Time)
    onUserFirstSpeech  func(time.Time)
    onBotFirstSpeech   func(time.Time)
    onFirstUserAudio   func(time.Time)
    wg                 *sync.WaitGroup
}

func (l *lifecycle) fireBotJoined(at time.Time) {
    l.botJoined.Do(func() {
        if l.onBotJoined == nil { return }
        l.wg.Add(1)
        go func() { defer l.wg.Done(); l.onBotJoined(at) }()
    })
}
// ... same for the other four ...
```

The processors fire these:
- `LiveKit room joined` → in `livekit_room.go` after `Connect` returns successfully, call `taskCtx.lifecycle.fireBotJoined(time.Now())`. But `JoinRoom` is called from `NewTask`, BEFORE TaskCtx is fully set up. **Refactor**: pass `taskCtx` into `JoinRoom`, fire after Connect.
- `Participant connected (non-bot)` → in `livekit_room.go`'s `RoomCallback`, add an `OnParticipantConnected` handler that checks identity != bot identity, calls `taskCtx.lifecycle.fireUserJoined(time.Now())`.
- `First non-silence audio frame received` → `audio_source_processor.go`'s `readAudioTrack`: detect non-silence (any frame where `bytes.Contains(data, nonZero)` or any sample magnitude > some threshold; simplest: any frame whose PCM has a sample > 1000) and call `taskCtx.lifecycle.fireFirstUserAudio(time.Now())`. Also stash that timestamp on the task for `CallStats.FirstUserAudioFrameAt`.
- `First finalized user transcript text` → `context_aggregator.go`'s `submitUserMessage`: call `taskCtx.lifecycle.fireUserFirstSpeech(time.Now())` before pushing the LLMMessagesFrame.
- `First bot audio frame written to wire` → `playback_sink_processor.go`'s `tick()`: where it currently calls `p.Broadcast(NewBotStartedSpeakingFrame())` (only fires on first audio of a turn), also fire `taskCtx.lifecycle.fireBotFirstSpeech(time.Now())` — `sync.Once` ensures it only fires once across the whole call.

### 6.2 Per-turn metrics aggregator

Today `MetricsFrame` is intercepted by `BaseProcessor.PushFrame` and only consumed by the UI handler. We need to also collect them per-turn so the assistant commit can carry them to observers.

Add to `PipelineTask`:

```go
type perTurnMetrics struct {
    mu    sync.Mutex
    current TurnMetrics
}

func (m *perTurnMetrics) absorb(mf MetricsFrame) {
    m.mu.Lock(); defer m.mu.Unlock()
    for _, d := range mf.Data {
        v := d.ValueMs
        switch {
        case d.Processor == "llm" && d.Label == MetricTTFB:
            m.current.LLMTtfbMs = &v
        case d.Processor == "tts" && d.Label == MetricTTFB:
            m.current.TTSTtfbMs = &v
        case d.Processor == "tts" && d.Label == MetricTextAggregation:
            m.current.TextAggregationMs = &v
        case d.Processor == "playback" && d.Label == MetricE2ELatency:
            m.current.V2VLatencyMs = &v
        }
    }
}

func (m *perTurnMetrics) snapshotAndReset() TurnMetrics {
    m.mu.Lock(); defer m.mu.Unlock()
    snap := m.current
    m.current = TurnMetrics{}
    return snap
}
```

In `ContextAggregator.commitSpokenText(interrupted bool)`:
```go
spoken := a.spokenSoFar()
if spoken != "" {
    // ... existing logging + a.messages append + UIEvents.Send ...
    metricsSnapshot := a.taskCtx.task.metrics.snapshotAndReset()  // see note
    a.taskCtx.observers.emitAssistantCommitted(spoken, time.Now(), metricsSnapshot)
}
```

In `ContextAggregator.addUserMessage(text string)`:
```go
// ... existing concat or append logic ...
a.taskCtx.observers.emitUserCommitted(text, time.Now())
```

**Wiring nuance**: TaskContext does not currently have a back-pointer to the task (and probably shouldn't). Simpler approach: have `task.metrics` and `task.observers` accessible via `TaskContext` indirection. Add `taskCtx.metrics *perTurnMetrics` and `taskCtx.observers *observerSet` fields populated by `NewTask`. The context aggregator reads them directly.

### 6.3 EndReason translation table

Add to `voicepipelinecore/task.go`:

```go
func mapReason(s string) EndReason {
    switch s {
    case "talk time exhausted":
        return EndReasonTalkTimeExhausted
    case "ui websocket disconnected", "ui requested end call":
        return EndReasonClientDisconnect
    case "":
        return EndReasonUnspecified
    default:
        return EndReasonUnspecified
    }
}
```

Either keep this table or, cleaner, change `taskCtx.EndTask` signature from `func(string)` to `func(EndReason)` and update the one caller (`TalkTimeMonitor`) to pass `EndReasonTalkTimeExhausted`. Pick the cleaner option.

### 6.4 `OnCallEnded` dispatch

In `PipelineTask.completeEnd`, after `wg.Wait` returns (or times out):

```go
if t.onCallEnded != nil {
    stats := CallStats{
        TotalUserDurationSec:  t.userPresence.totalDurationSec(),  // see §6.5
        FirstUserAudioFrameAt: t.userPresence.firstUserAudioFrameAt,
        EndedAt:               time.Now(),
    }
    // Run synchronously here — the caller has already waited for cleanup.
    t.onCallEnded(t.endReason, stats)
}
if t.onCleanup != nil {
    t.onCleanup()
}
```

### 6.5 `UserPresence` (per-call duration tracking)

New `voicepipelinecore/user_presence.go`:

```go
package voicepipelinecore

import (
    "sync"
    "time"
)

type UserPresence struct {
    mu                       sync.Mutex
    firstUserJoinedAt        *time.Time
    lastUserLeftAt           *time.Time
    firstUserAudioFrameAt    *time.Time
}

func (p *UserPresence) MarkUserJoined(at time.Time) {
    p.mu.Lock(); defer p.mu.Unlock()
    if p.firstUserJoinedAt == nil {
        p.firstUserJoinedAt = &at
    }
}

func (p *UserPresence) MarkUserLeft(at time.Time) {
    p.mu.Lock(); defer p.mu.Unlock()
    p.lastUserLeftAt = &at
}

func (p *UserPresence) MarkFirstUserAudio(at time.Time) {
    p.mu.Lock(); defer p.mu.Unlock()
    if p.firstUserAudioFrameAt == nil {
        p.firstUserAudioFrameAt = &at
    }
}

func (p *UserPresence) TotalDurationSec() int {
    p.mu.Lock(); defer p.mu.Unlock()
    if p.firstUserJoinedAt == nil {
        return 0
    }
    end := time.Now()
    if p.lastUserLeftAt != nil {
        end = *p.lastUserLeftAt
    }
    return int(end.Sub(*p.firstUserJoinedAt).Seconds())
}
```

`UserPresence` lives on `PipelineTask` (and indirectly via `TaskContext.Presence`). Update accordingly. LiveKit `RoomCallback.OnParticipantConnected`/`OnParticipantDisconnected` call `MarkUserJoined`/`MarkUserLeft`. AudioSource's first-audio detection calls `MarkFirstUserAudio` AND fires the lifecycle callback.

### 6.6 Phase 1.5 verification

Unit tests:
- Build a task with all five lifecycle callbacks + one observer, drive it through synthetic frames (TranscriptFrame final → triggers OnUserFirstSpeech + OnUserCommitted; AudioFrame → triggers OnBotFirstSpeech via playback), verify each fires exactly once.
- Send fake `MetricsFrame`s through, verify the next assistant commit's `TurnMetrics` contains the values.
- Verify observer queue handles burst (200 commits sent rapidly) without dropping.
- Verify observer panic is caught and logged, doesn't crash the task.

---

## 7. Phase 2 — Disha package: types + Redis client

Create `disha/` directory.

### 7.1 `disha/types.go`

```go
package disha

import "time"

// ConversationData mirrors what Disha writes to
// "conversation_data:{conversation_id}" in Redis.
// Source: bots/bot_session_manager.py:406-419
type ConversationData struct {
    Conversation             ConversationRow        `json:"conversation"`
    Chunks                   [][]any                `json:"chunks"` // 5-tuples
    UserProfile              map[string]any         `json:"user_profile"`
    UnprocessedChatContext   *string                `json:"unprocessed_chat_context,omitempty"`
    ResumedChunk             map[string]any         `json:"resumed_chunk,omitempty"`
}

type ConversationRow struct {
    ID                  string  `json:"id"`
    UserID              string  `json:"user_id"`
    BotType             string  `json:"bot_type"`
    ResumedFromChunkID  *string `json:"resumed_from_chunk_id"`
    StartChunkID        *string `json:"start_chunk_id"`
    EndChunkID          *string `json:"end_chunk_id"`
    // Many other fields on CallConversation; add only what we read.
}

// ConversationChunk mirrors the 21-key dict at
// bots/onboarding_call/conversation_persistence_processor.py:113-132
type ConversationChunk struct {
    ID                               string   `json:"id"`
    Text                             string   `json:"text"`
    Role                             string   `json:"role"`
    BotType                          string   `json:"bot_type"`
    ConversationID                   string   `json:"conversation_id"`
    UserID                           string   `json:"user_id"`
    CurrentAgenda                    *string  `json:"current_agenda"`
    STTTtfbMs                        *float64 `json:"stt_ttfb_ms"`
    LLMTtfbMs                        *float64 `json:"llm_ttfb_ms"`
    TTSTtfbMs                        *float64 `json:"tts_ttfb_ms"`
    V2VLatencyMs                     *float64 `json:"v2v_latency_ms"`
    TextAggregationMs                *float64 `json:"text_aggregation_ms"`
    Created                          string   `json:"created"`
    IsDebugLog                       bool     `json:"is_debug_log"`
    AdditionalData                   any      `json:"additional_data"`
    MainAgentSystemPromptLangfuseKey *string  `json:"main_agent_system_prompt_langfuse_key"`
    SystemMessageParamsS3Key         *string  `json:"system_message_params_s3_key"`
    ConversationStateS3Key           *string  `json:"conversation_state_s3_key"`
}

// UpdateConversationRequest mirrors bots/views.py:62-67
type UpdateConversationRequest struct {
    ConversationID    string     `json:"conversation_id"`
    BotJoinedAt       *time.Time `json:"bot_joined_at,omitempty"`
    UserJoinedAt      *time.Time `json:"user_joined_at,omitempty"`
    UserFirstSpeechAt *time.Time `json:"user_first_speech_at,omitempty"`
    BotFirstSpeechAt  *time.Time `json:"bot_first_speech_at,omitempty"`
}

// PostCallOperationsRequest mirrors bots/views.py:70-81
type PostCallOperationsRequest struct {
    ConversationID                 string         `json:"conversation_id"`
    EndReason                      *string        `json:"end_reason,omitempty"`
    TotalUserDuration              int            `json:"total_user_duration"`
    FirstUserAudioFramesReceivedAt *time.Time     `json:"first_user_audio_frames_received_at,omitempty"`
    EndedAt                        time.Time      `json:"ended_at"`
    LogDataS3Key                   string         `json:"log_data_s3_key"`     // we send ""
    OnboardingCallDone             bool           `json:"onboarding_call_done"` // false for sales
    DietPlanIntensityLevel         *string        `json:"diet_plan_intensity_level,omitempty"`
    FitnessPlanIntensityLevel      *string        `json:"fitness_plan_intensity_level,omitempty"`
    LatestOnboardingCallStage      *string        `json:"latest_onboarding_call_stage,omitempty"`
    ConversationVariables          map[string]any `json:"conversation_variables,omitempty"`
}

// EnqueueJobRequest is what we send to Disha's job-enqueue endpoint
// to trigger the Redis→Postgres chunk sync.
// Mirror of bots/onboarding_call/conversation_persistence_processor.py:147-159
type EnqueueJobRequest struct {
    Func         string         `json:"func"`
    Kwargs       map[string]any `json:"kwargs"`
    QueueName    string         `json:"queue_name"`
    TrackingType string         `json:"tracking_type"`
}
```

### 7.2 `disha/redis_client.go`

```go
package disha

import (
    "context"
    "encoding/json"
    "fmt"
    "log"
    "time"

    "github.com/redis/go-redis/v9"
)

// RedisClient is the narrow surface the disha package needs from Redis.
type RedisClient interface {
    GetConversationData(ctx context.Context, conversationID string) (*ConversationData, error)
    AppendChunk(ctx context.Context, userID, conversationID string, chunk ConversationChunk) error
    Close() error
}

type redisClient struct {
    rdb    *redis.Client
    logger *log.Logger
}

func NewRedisClient(addr, password string, db int, logger *log.Logger) RedisClient {
    return &redisClient{
        rdb: redis.NewClient(&redis.Options{
            Addr:            addr,
            Password:        password,
            DB:              db,
            MaxRetries:      3,
            MinRetryBackoff: 100 * time.Millisecond,
            MaxRetryBackoff: 1 * time.Second,
            DialTimeout:     5 * time.Second,
            ReadTimeout:     3 * time.Second,
            WriteTimeout:    3 * time.Second,
        }),
        logger: logger,
    }
}

func (c *redisClient) Close() error { return c.rdb.Close() }

func (c *redisClient) GetConversationData(ctx context.Context, conversationID string) (*ConversationData, error) {
    key := fmt.Sprintf("conversation_data:%s", conversationID)
    raw, err := c.rdb.Get(ctx, key).Bytes()
    if err == redis.Nil {
        return nil, fmt.Errorf("disha: conversation_data not found in Redis: %s", conversationID)
    }
    if err != nil {
        return nil, fmt.Errorf("disha: redis GET failed: %w", err)
    }
    var data ConversationData
    if err := json.Unmarshal(raw, &data); err != nil {
        return nil, fmt.Errorf("disha: conversation_data malformed: %w", err)
    }
    return &data, nil
}

func (c *redisClient) AppendChunk(ctx context.Context, userID, conversationID string, chunk ConversationChunk) error {
    key := fmt.Sprintf("conversation_chunks:%s:%s", userID, conversationID)
    payload, err := json.Marshal(chunk)
    if err != nil {
        return fmt.Errorf("disha: chunk marshal failed: %w", err)
    }
    if err := c.rdb.RPush(ctx, key, payload).Err(); err != nil {
        return fmt.Errorf("disha: redis RPUSH failed: %w", err)
    }
    return nil
}
```

Env vars for `main.go`:
- `REDIS_ADDR` (default `localhost:6379`)
- `REDIS_PASSWORD` (default `""`)
- `REDIS_DB` (default `0`)

### 7.3 Tests

Use `github.com/alicebob/miniredis/v2` (add as dev dep) to spin up an in-memory Redis. Test:
- `GetConversationData` returns parsed struct when key exists with valid JSON.
- `GetConversationData` returns "not found" error when key missing.
- `GetConversationData` returns "malformed" error when value is invalid JSON.
- `AppendChunk` writes a JSON entry to the LIST. Verify with `miniredis.LRange`.

---

## 8. Phase 3 — Disha HTTP API client

### 8.1 `disha/api_client.go`

```go
package disha

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "log"
    "net/http"
    "time"
)

type APIClient struct {
    baseURL    string
    authToken  string
    httpClient *http.Client
    logger     *log.Logger
}

func NewAPIClient(baseURL, authToken string, timeout time.Duration, logger *log.Logger) *APIClient {
    return &APIClient{
        baseURL:    baseURL,
        authToken:  authToken,
        httpClient: &http.Client{Timeout: timeout},
        logger:     logger,
    }
}

func (c *APIClient) UpdateConversation(ctx context.Context, req UpdateConversationRequest) error {
    return c.send(ctx, "PATCH", "/bot/update_conversation", req)
}

func (c *APIClient) RunPostCallOperations(ctx context.Context, req PostCallOperationsRequest) error {
    return c.send(ctx, "POST", "/bot/run_post_call_operations", req)
}

func (c *APIClient) EnqueueJob(ctx context.Context, req EnqueueJobRequest) error {
    return c.send(ctx, "POST", "/bot/enqueue_job", req)
}

func (c *APIClient) send(ctx context.Context, method, path string, body any) error {
    payload, err := json.Marshal(body)
    if err != nil { return fmt.Errorf("marshal: %w", err) }

    httpReq, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(payload))
    if err != nil { return fmt.Errorf("new request: %w", err) }
    httpReq.Header.Set("Content-Type", "application/json")
    httpReq.Header.Set("Authorization", "Bearer "+c.authToken)

    resp, err := c.httpClient.Do(httpReq)
    if err != nil { return fmt.Errorf("disha API %s %s: %w", method, path, err) }
    defer resp.Body.Close()

    if resp.StatusCode >= 400 {
        b, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("disha API %s %s returned %d: %s", method, path, resp.StatusCode, string(b))
    }
    return nil
}
```

Env vars for `main.go`:
- `DISHA_API_URL` (e.g. `http://localhost:8000`)
- `DISHA_API_AUTH_TOKEN`

### 8.2 Tests

Use `httptest.NewServer` to assert on the captured request bodies for each endpoint. Verify:
- Method, path, Content-Type, Authorization header.
- Body is the expected JSON shape.
- Non-2xx response returns error.
- Context cancellation respected.

---

## 9. Phase 4 — Disha observer + session builder

### 9.1 `disha/observer.go`

```go
package disha

import (
    "context"
    "log"
    "time"

    "github.com/google/uuid"
    "github.com/jaideep329/talk-go/voicepipelinecore"
)

// ChunkPersistenceObserver implements voicepipelinecore.TaskObserver
// and RPUSHes every committed turn to Redis.
type ChunkPersistenceObserver struct {
    redis          RedisClient
    userID         string
    conversationID string
    botType        string
    logger         *log.Logger
}

func NewChunkPersistenceObserver(redis RedisClient, userID, conversationID, botType string, logger *log.Logger) *ChunkPersistenceObserver {
    return &ChunkPersistenceObserver{
        redis: redis, userID: userID, conversationID: conversationID,
        botType: botType, logger: logger,
    }
}

func (o *ChunkPersistenceObserver) OnUserCommitted(text string, at time.Time) {
    o.append(text, "user", at, voicepipelinecore.TurnMetrics{})
}

func (o *ChunkPersistenceObserver) OnAssistantCommitted(text string, at time.Time, m voicepipelinecore.TurnMetrics) {
    o.append(text, "assistant", at, m)
}

func (o *ChunkPersistenceObserver) append(text, role string, at time.Time, m voicepipelinecore.TurnMetrics) {
    chunk := ConversationChunk{
        ID:                uuid.NewString(),
        Text:              text,
        Role:              role,
        BotType:           o.botType,
        ConversationID:    o.conversationID,
        UserID:            o.userID,
        Created:           at.Format(time.RFC3339Nano),
        IsDebugLog:        false,
        LLMTtfbMs:         m.LLMTtfbMs,
        TTSTtfbMs:         m.TTSTtfbMs,
        TextAggregationMs: m.TextAggregationMs,
        V2VLatencyMs:      m.V2VLatencyMs,
        // All other fields are nil by default — matches Disha schema.
    }
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    if err := o.redis.AppendChunk(ctx, o.userID, o.conversationID, chunk); err != nil {
        o.logger.Printf("chunk persist failed conv=%s role=%s: %v", o.conversationID, role, err)
    }
}
```

Note: the core dispatches `OnUserCommitted`/`OnAssistantCommitted` from a dedicated per-observer goroutine that's already non-blocking with respect to the pipeline. The Redis call here is synchronous **within that observer goroutine**, which is correct: we want FIFO ordering of chunks in Redis, and the observer worker provides that ordering. If a single RPUSH stalls for 5 seconds, only this observer's queue backs up — not the pipeline.

### 9.2 `disha/session.go`

```go
package disha

import (
    "context"
    "fmt"
    "log"
    "time"

    "github.com/jaideep329/talk-go/voicepipelinecore"
)

const SalesCallSystemPrompt = `You are an expert health coach named Disha...` // copy from context_aggregator.go:135-138

// Deps is everything the disha package needs that can't be derived
// per-session. Created once in main.go and reused.
type Deps struct {
    Logger      *log.Logger
    Redis       RedisClient
    API         *APIClient
}

// BuildSession reads conversation_data from Redis, assembles the
// initial messages + observers + lifecycle callbacks, and returns a
// PipelineTask ready to Start. The caller is responsible for adding it
// to its sessions registry and calling Start.
func BuildSession(ctx context.Context, conversationID string, deps Deps) (*voicepipelinecore.PipelineTask, error) {
    data, err := deps.Redis.GetConversationData(ctx, conversationID)
    if err != nil {
        return nil, fmt.Errorf("disha: load conversation_data: %w", err)
    }
    if data.Conversation.BotType != "sales_call" {
        return nil, fmt.Errorf("disha: unsupported bot_type %q (only sales_call supported)", data.Conversation.BotType)
    }

    initialMessages := buildInitialMessages(data)
    observer := NewChunkPersistenceObserver(deps.Redis, data.Conversation.UserID, conversationID, "sales_call", deps.Logger)

    var firstUserAudioAt *time.Time
    callLogger := log.New(log.Writer(), fmt.Sprintf("[conv=%s] ", conversationID), log.Flags()|log.Lmicroseconds)

    fireUpdate := func(req UpdateConversationRequest) {
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        if err := deps.API.UpdateConversation(ctx, req); err != nil {
            callLogger.Printf("update_conversation failed: %v", err)
        }
    }

    opts := voicepipelinecore.TaskOptions{
        Logger:          callLogger,
        RoomName:        fmt.Sprintf("conv-%s", conversationID),
        InitialMessages: initialMessages,
        Observers:       []voicepipelinecore.TaskObserver{observer},
        OnBotJoined: func(at time.Time) {
            fireUpdate(UpdateConversationRequest{ConversationID: conversationID, BotJoinedAt: &at})
        },
        OnUserJoined: func(at time.Time) {
            fireUpdate(UpdateConversationRequest{ConversationID: conversationID, UserJoinedAt: &at})
        },
        OnUserFirstSpeech: func(at time.Time) {
            fireUpdate(UpdateConversationRequest{ConversationID: conversationID, UserFirstSpeechAt: &at})
        },
        OnBotFirstSpeech: func(at time.Time) {
            fireUpdate(UpdateConversationRequest{ConversationID: conversationID, BotFirstSpeechAt: &at})
        },
        OnFirstUserAudio: func(at time.Time) {
            firstUserAudioAt = &at  // captured into post-call payload below
        },
        OnCallEnded: func(reason voicepipelinecore.EndReason, stats voicepipelinecore.CallStats) {
            // Step 2: run_post_call_operations.
            endReasonStr := mapEndReason(reason)
            req := PostCallOperationsRequest{
                ConversationID:                 conversationID,
                EndReason:                      endReasonStr,
                TotalUserDuration:              stats.TotalUserDurationSec,
                FirstUserAudioFramesReceivedAt: stats.FirstUserAudioFrameAt,
                EndedAt:                        stats.EndedAt,
                LogDataS3Key:                   "", // TODO(rtvi): port observer + S3 upload
                OnboardingCallDone:             false,
            }
            ctx1, cancel1 := context.WithTimeout(context.Background(), 10*time.Second)
            defer cancel1()
            if err := deps.API.RunPostCallOperations(ctx1, req); err != nil {
                callLogger.Printf("run_post_call_operations failed: %v", err)
            }

            // Step 5: enqueue chunk sync.
            jobReq := EnqueueJobRequest{
                Func: "sync_conversation_chunks_to_db",
                Kwargs: map[string]any{
                    "user_id":         data.Conversation.UserID,
                    "conversation_id": conversationID,
                    "bot_type":        "sales_call",
                },
                QueueName:    "p1_fast_l1",
                TrackingType: "LAZY",
            }
            ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
            defer cancel2()
            if err := deps.API.EnqueueJob(ctx2, jobReq); err != nil {
                callLogger.Printf("enqueue_job failed: %v", err)
            }
        },
    }
    return voicepipelinecore.NewTask(ctx, opts)
}

// buildInitialMessages constructs the LLM context from the system
// prompt plus the resumed chunk history (filtered).
func buildInitialMessages(data *ConversationData) []voicepipelinecore.Message {
    msgs := []voicepipelinecore.Message{
        {Role: "system", Content: SalesCallSystemPrompt},
    }
    for _, tuple := range data.Chunks {
        if len(tuple) < 4 { continue }
        // tuple = [id, role, text, is_debug_log, additional_data]
        role, _   := tuple[1].(string)
        text, _   := tuple[2].(string)
        isDebug, _ := tuple[3].(bool)
        if isDebug { continue }
        if role != "user" && role != "assistant" { continue }
        msgs = append(msgs, voicepipelinecore.Message{Role: role, Content: text})
    }
    return msgs
}

func mapEndReason(r voicepipelinecore.EndReason) *string {
    var s string
    switch r {
    case voicepipelinecore.EndReasonTalkTimeExhausted:
        s = "talktime_exhausted"
    case voicepipelinecore.EndReasonClientDisconnect:
        s = "client_disconnected"
    case voicepipelinecore.EndReasonUserIdle:
        s = "user_idle"
    default:
        return nil
    }
    return &s
}
```

### 9.3 Tests

Integration test in `disha/session_test.go`:
- Stand up `miniredis` and `httptest` servers.
- Seed `conversation_data:test-id` in miniredis with a valid blob.
- Mock the `talk-go/voicepipelinecore` boundary: you don't need to start a real pipeline. Instead, write a test that calls `BuildSession`, then synthesizes calling the callbacks (`OnBotJoined`, `OnUserCommitted`, `OnCallEnded`) and asserts:
  - `httptest` server received `PATCH /bot/update_conversation` with the right body.
  - Redis LIST `conversation_chunks:<uid>:test-id` has the right RPUSHed entries (use miniredis `LRange`).
  - `httptest` server received `POST /bot/run_post_call_operations` and `POST /bot/enqueue_job` with the right bodies, in that order.

You can drive callbacks directly via the `TaskOptions` struct returned indirectly — refactor `BuildSession` to return `(opts TaskOptions, err error)` plus a thin wrapper that calls `NewTask`, so the test can grab opts without spinning up a real LiveKit session. (Better: split `BuildSession` into `BuildOptions` + `BuildSession`. `BuildOptions` is testable; `BuildSession` just calls `NewTask(BuildOptions())`.)

---

## 10. Phase 5 — Wire `/connect` to `disha.BuildSession`

### 10.1 New `/connect` handler

```go
type connectRequest struct {
    ConversationID string `json:"conversation_id"`
}

func handleConnect(w http.ResponseWriter, r *http.Request) {
    var req connectRequest
    json.NewDecoder(r.Body).Decode(&req) // tolerate empty body for dev flow

    var task *voicepipelinecore.PipelineTask
    var err error

    if req.ConversationID != "" {
        task, err = disha.BuildSession(r.Context(), req.ConversationID, dishaDeps)
    } else {
        // Dev/test flow: use the hardcoded prompt, no Disha integration.
        task, err = voicepipelinecore.NewTask(context.Background(), voicepipelinecore.TaskOptions{
            Logger:          newRoomLogger(""),
            InitialMessages: []voicepipelinecore.Message{{Role: "system", Content: disha.SalesCallSystemPrompt}},
        })
    }
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadGateway)
        return
    }

    task.OnCleanup = func() {
        sessionsMu.Lock(); delete(sessions, task.RoomName); sessionsMu.Unlock()
    }
    sessionsMu.Lock(); sessions[task.RoomName] = task; sessionsMu.Unlock()
    task.Start()

    token, err := voicepipelinecore.GenerateToken(task.RoomName, "web-user")
    if err != nil { http.Error(w, err.Error(), 500); return }
    json.NewEncoder(w).Encode(map[string]string{
        "server_url": os.Getenv("LIVEKIT_URL"),
        "token":      token,
        "room_name":  task.RoomName,
    })
}
```

### 10.2 Wiring `dishaDeps` in `main.go`

```go
var dishaDeps disha.Deps

func main() {
    loadEnv(".env")
    // ... existing logger setup ...

    dishaDeps = disha.Deps{
        Logger: log.Default(),
        Redis:  disha.NewRedisClient(os.Getenv("REDIS_ADDR"), os.Getenv("REDIS_PASSWORD"), 0, log.Default()),
        API:    disha.NewAPIClient(os.Getenv("DISHA_API_URL"), os.Getenv("DISHA_API_AUTH_TOKEN"), 10*time.Second, log.Default()),
    }
    defer dishaDeps.Redis.Close()

    // ... HTTP handlers ...
}
```

### 10.3 Phase 5 verification

- `go build && go run .`
- Manual smoke: with no Redis running, `curl -XPOST http://localhost:3000/connect -d '{}'` should still work (dev path).
- With Redis running and a seeded `conversation_data:test-id` key, `curl -XPOST http://localhost:3000/connect -d '{"conversation_id":"test-id"}'` should return a token and start the bot.
- Check Disha-side logs to see `PATCH /bot/update_conversation` arriving with `bot_joined_at` and `user_first_speech_at` (if a real LiveKit call is made).

---

## 11. Phase 6 — End-to-end live test

Once phases 0–5 are done:

1. Spin up Disha backend locally pointing to its dev Redis + Postgres.
2. Seed a `conversation_data:<id>` key via Disha (call its existing orchestrator flow, OR write a small `cmd/seed/main.go` that mirrors the Python seeding logic).
3. Run `talk-go` with `DISHA_API_URL=http://localhost:8000`, `REDIS_ADDR=localhost:6379`.
4. From the browser, hit `/connect` with the conversation_id.
5. Have a 30-second conversation.
6. End the call (close the WebSocket).
7. Verify in Disha's database:
   - `CallConversation.bot_joined_at`, `user_joined_at`, `user_first_speech_at`, `bot_first_speech_at` populated.
   - `CallConversation.post_call_operations_started_at` populated.
   - `CallConversation.total_user_duration` populated.
   - `conversation_chunks:<uid>:<id>` in Redis populated, then drained to Postgres by Disha's SQS job within a few seconds.
   - Final `ConversationChunk` rows in Postgres match the call's turns.

---

## 12. Phase 7 — Cleanup

- Remove any stale debug logging added during development.
- Run `go vet ./...` and `golangci-lint run` (if configured).
- Run `go test -race -count=3 ./...` until all green.
- Update AGENTS.md (see §14) if not already updated in phase 0.
- **Tell the user the work is ready for commit-by-phase review.** Do not commit anything yourself.

---

## 13. Phase boundaries — stop and report after each

After each of these phases, stop work, run `go test -race -count=2 ./...`, summarize what changed, and wait for the user before continuing:

- **End of phase 0**: module renamed, package split done, all existing tests pass, browser dev flow still works. *Stop.*
- **End of phase 1 + 1.5**: TaskOptions + observers + lifecycle hooks + per-turn metrics aggregation merged. Tests pass. No behavior change visible. *Stop.*
- **End of phase 2**: Disha types + Redis client merged, with tests. *Stop.*
- **End of phase 3**: Disha HTTP API client merged, with tests. *Stop.*
- **End of phase 4**: Disha observer + BuildSession merged, with tests. *Stop.*
- **End of phase 5**: `/connect` accepts conversation_id, end-to-end ready for live test. *Stop.*
- **End of phase 6**: live test passed (or any failures + fixes documented). *Stop.*

Do not chain phases. Phase boundaries are review points.

---

## 14. AGENTS.md update (add this section near the top)

Add this block to AGENTS.md right after the "How to Collaborate" section:

```markdown
## Architectural rule: core library vs. business integration

The `voicepipelinecore/` package is the call/pipeline framework. It must
remain free of business logic: no Redis, no HTTP, no conversation IDs,
no prompts, no end-reason policies, no persistence. Treat it as an
internal library.

All business integrations (Disha persistence, post-call API calls,
prompt injection, etc.) live in their own package (`disha/`, future:
`onboarding/`, `followup/`) and hook into the core via:

  1. `voicepipelinecore.TaskOptions` callbacks (`OnBotJoined`,
     `OnUserFirstSpeech`, `OnCallEnded`, ...) — for lifecycle moments.
  2. `voicepipelinecore.TaskObserver` interface (`OnUserCommitted`,
     `OnAssistantCommitted`) — for per-turn data hand-off. Observers
     are dispatched on dedicated goroutines (one queue per observer,
     FIFO-ordered, capacity 256) so the pipeline never blocks on
     observer work.
  3. `TaskOptions.InitialMessages` — for prompt + prior-context
     injection.

If you find yourself adding `if conversationID != ""` or
`import ".../disha"` inside `voicepipelinecore/`, stop and add a
callback instead.

Mirrors Pipecat's pattern: `sales_call.py` wires `@event_handler`
decorators onto core processors; the core never imports the call type.
```

Also update the file map and pipeline diagram in AGENTS.md to reflect the new package boundary.

---

## 15. Things the user explicitly said NO to (do not implement)

- No Postgres reads or writes from Go anywhere.
- No fallback to Postgres if Redis is cold — return 502.
- No RTVI logs port. Send `log_data_s3_key=""` with `// TODO(rtvi): port observer + S3 upload`.
- No Langfuse-managed prompt fetch. Hardcoded sales prompt in `disha/session.go` only.
- No bot types other than `sales_call`.
- No pod lifecycle / GKE / scheduling. We assume the caller (Disha orchestrator) handles that and just hands us a conversation_id.
- No Daily metrics. We use LiveKit.
- No "resume from arbitrary chunk" UI. Only `resumed_chunk` from `conversation_data` if Disha set it.

---

## 16. Open items that need user clarification mid-flight

Stop and ask the user when you hit any of these:

1. **`/bot/enqueue_job` endpoint doesn't exist on Disha.** Confirm with the user. They may need to add it server-side before phase 4 can be tested end-to-end.
2. **Disha API auth header format**. The Python client likely uses something other than `Bearer`. Check `bots/operations/voice_bot_operations.py` (or wherever `VoiceBotAPIService` lives) for the exact header name and value format. If unclear, ask.
3. **Disha API base URL** — confirm with user what they want us to default to in `.env.example` (local dev: `http://localhost:8000`; staging: `https://...`; prod: `https://...`).
4. **`p1_fast_l1` queue name** for enqueue_job — confirm this is the right queue. Check `common/sqs_queue_mapping_manager.py`.
5. **Sales call system prompt** — copy verbatim from `talk-go`'s current `context_aggregator.go:135-138` (3 sentences, "expert health coach Disha", etc.). Confirm with user before assuming this is correct for production sales calls. Disha's real prompt is in Langfuse and we explicitly skipped that.
6. **Resumed chunk handling** — if `ConversationData.ResumedChunk` is set, do we need to do anything special? User said "we only resume from the resume chunk present in conversation data, nothing else." Interpret: the resumed chunk's text is already in `Chunks[]`, so `buildInitialMessages` naturally includes it. No extra handling needed. Confirm.

---

## 17. Quick code-navigation cheatsheet for the executing agent

When you need to find something:

| Need | Where |
|------|-------|
| Current /connect handler | `main.go:35-51` |
| Current sessions map | `pipeline.go:76-91` |
| Current `createSession` | `pipeline.go:93-155` |
| `PipelineTask.End` | `pipeline.go:157-175` |
| `PipelineTask.completeEnd` (cleanup) | `pipeline.go:177-215` |
| `TaskContext` struct | `pipeline.go:52-67` |
| `BaseProcessor` + Processor interface | `processor.go` |
| `Frame` interface + `FrameBase` + constructors | `frame.go` |
| Hardcoded sales prompt to move | `context_aggregator.go:135-138` |
| Min-word barge-in constant | `context_aggregator.go:8` |
| `addUserMessage` (where to emit OnUserCommitted) | `context_aggregator.go:120-130` |
| `commitSpokenText` (where to emit OnAssistantCommitted) | `context_aggregator.go:102-111` |
| `submitUserMessage` (where to fire OnUserFirstSpeech) | `context_aggregator.go:132-146` |
| `PlaybackSink` Broadcast for BotStartedSpeaking (where to fire OnBotFirstSpeech) | `playback_sink_processor.go:222` |
| AudioSource read loop (where to fire OnFirstUserAudio) | `audio_source_processor.go` |
| LiveKit Connect callsite (where to fire OnBotJoined) | `livekit_room.go` |
| Disha sales_call cleanup (reference) | `/Users/jaideepsingh/Projects/disha-backend/bots/sales_call/sales_call.py:413-487` |
| Disha bot_session_manager (conversation_data writer) | `/Users/jaideepsingh/Projects/disha-backend/bots/bot_session_manager.py:380-419` |
| Disha conversation_persistence (chunk schema) | `/Users/jaideepsingh/Projects/disha-backend/bots/onboarding_call/conversation_persistence_processor.py:113-132` |
| Disha Redis chunks cache | `/Users/jaideepsingh/Projects/disha-backend/services/conversation_chunks_cache.py` |
| Disha API request models | `/Users/jaideepsingh/Projects/disha-backend/bots/views.py:51-92` |
| Disha API handlers | `/Users/jaideepsingh/Projects/disha-backend/bots/views.py:575-585` |
| Disha URL routes | `/Users/jaideepsingh/Projects/disha-backend/bots/urls.py` |

---

## 18. Test corpus

After the work is done, ensure these test files exist and pass:

- `voicepipelinecore/*_test.go` — all existing tests should continue passing.
- `voicepipelinecore/task_test.go` — new, exercises lifecycle callbacks, observer dispatch, EndReason translation.
- `voicepipelinecore/user_presence_test.go` — new, exercises duration tracking.
- `disha/redis_client_test.go` — new, miniredis-based.
- `disha/api_client_test.go` — new, httptest-based.
- `disha/session_test.go` — new, integration test of BuildSession + observer + callbacks against miniredis + httptest.

Target: `go test -race -count=3 ./...` clean.

---

## 19. Done criteria

You are done when:
1. `voicepipelinecore` package exists, contains all the core pipeline code, has zero imports of `disha/`, `redis/`, or any HTTP client.
2. `disha` package exists, contains Redis client, HTTP client, observer, session builder.
3. `main.go` wires both packages, owns the sessions map, handles the dev fallback path.
4. AGENTS.md updated with the core/integration rule.
5. All tests pass under `-race -count=3`.
6. A live call with a real Disha conversation_id (phase 6) results in Postgres rows being created server-side via Disha's existing SQS pipeline.
7. **Nothing is committed.** Working tree changes ready for user review.

Good luck.
