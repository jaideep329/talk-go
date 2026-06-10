package voicepipelinecore

import (
	"strings"
	"testing"
	"time"
)

// TestContextAggregator_FinalTranscriptEmitsLLMMessages verifies a
// final transcript ending with <end> produces an LLMMessagesFrame
// downstream.
func TestContextAggregator_FinalTranscriptEmitsLLMMessages(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx, nil, "")

	down, _ := runProcessorTest(t, fix, runConfig{
		processor: a,
		framesToSend: []Frame{
			TranscriptFrame{Text: "hello", IsFinal: true},
			TranscriptFrame{Text: "<end>", IsFinal: true},
		},
		sendEndFrame: true,
	})

	llmMsg, ok := findFrame[LLMMessagesFrame](down)
	if !ok {
		t.Fatalf("expected LLMMessagesFrame, got %s", describeFrameTypes(down))
	}
	if len(llmMsg.Messages) == 0 {
		t.Fatal("LLMMessagesFrame should contain messages")
	}
	// First message is the system prompt; second is the user message.
	if llmMsg.Messages[0].Role != "system" {
		t.Errorf("first message should be system, got %q", llmMsg.Messages[0].Role)
	}
	last := llmMsg.Messages[len(llmMsg.Messages)-1]
	if last.Role != "user" {
		t.Errorf("last message should be user, got %q", last.Role)
	}
	if last.Content != "hello" {
		t.Errorf("user content: got %q, want 'hello'", last.Content)
	}
}

func TestContextAggregator_SuppressesInterimUserTranscriptionEvents(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx, nil, "")

	runProcessorTest(t, fix, runConfig{
		processor: a,
		framesToSend: []Frame{
			TranscriptFrame{Text: "hel", IsFinal: false, ResponseID: 1},
			TranscriptFrame{Text: "lo", IsFinal: false, ResponseID: 1},
		},
		sendEndFrame: true,
	})

	for _, entry := range fix.TaskCtx.UIEvents.Snapshot() {
		if entry.Type == "user-transcription" {
			t.Fatalf("interim transcript emitted RTVI user-transcription event: %+v", entry)
		}
	}
}

func TestContextAggregator_FinalUserTranscriptionStillEmitsRTVI(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx, nil, "")

	runProcessorTest(t, fix, runConfig{
		processor: a,
		framesToSend: []Frame{
			TranscriptFrame{Text: "hello", IsFinal: true, ResponseID: 1},
			TranscriptFrame{Text: "<end>", IsFinal: true, ResponseID: 1},
		},
		sendEndFrame: true,
	})

	var found bool
	for _, entry := range fix.TaskCtx.UIEvents.Snapshot() {
		if entry.Type != "user-transcription" {
			continue
		}
		data, ok := entry.Data.(map[string]any)
		if !ok {
			t.Fatalf("user-transcription data = %#v", entry.Data)
		}
		if data["text"] != "hello" || data["final"] != true {
			t.Fatalf("user-transcription data = %+v, want final hello", data)
		}
		found = true
	}
	if !found {
		t.Fatal("final transcript did not emit RTVI user-transcription event")
	}
}

func TestContextAggregator_InitialMessagesSeedLLMContext(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx, []Message{
		{Role: "system", Content: "seed context"},
		{Role: "assistant", Content: "hello seed"},
	}, "")

	down, _ := runProcessorTest(t, fix, runConfig{
		processor: a,
		framesToSend: []Frame{
			TranscriptFrame{Text: "new user", IsFinal: true},
			TranscriptFrame{Text: "<end>", IsFinal: true},
		},
		sendEndFrame: true,
	})

	llmMsg, ok := findFrame[LLMMessagesFrame](down)
	if !ok {
		t.Fatalf("expected LLMMessagesFrame, got %s", describeFrameTypes(down))
	}
	if len(llmMsg.Messages) != 3 {
		t.Fatalf("message count = %d, want 3", len(llmMsg.Messages))
	}
	if llmMsg.Messages[0].Content != "seed context" {
		t.Fatalf("first message content = %q, want seed context", llmMsg.Messages[0].Content)
	}
	last := llmMsg.Messages[len(llmMsg.Messages)-1]
	if last.Role != "user" || last.Content != "new user" {
		t.Fatalf("last message = %+v, want user new user", last)
	}
}

func TestContextAggregator_AppendRunLLMEmitsFirstTurnFromInitialContext(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx, []Message{
		{Role: "system", Content: "sales prompt"},
		{Role: "user", Content: "hello?"},
	}, "")

	// Greet-first: append no messages, just run the LLM on the initial
	// context (what DailyRoom pushes on user join).
	down, _ := runProcessorTest(t, fix, runConfig{
		processor:    a,
		framesToSend: []Frame{LLMMessagesAppendFrame{RunLLM: true}},
		settleDelay:  100 * time.Millisecond,
		sendEndFrame: true,
	})

	llmMsg, ok := findFrame[LLMMessagesFrame](down)
	if !ok {
		t.Fatalf("expected LLMMessagesFrame, got %s", describeFrameTypes(down))
	}
	if len(llmMsg.Messages) != 2 {
		t.Fatalf("message count = %d, want 2 (the initial context)", len(llmMsg.Messages))
	}
	if llmMsg.Messages[0].Content != "sales prompt" {
		t.Fatalf("first message = %q, want sales prompt", llmMsg.Messages[0].Content)
	}
	// The append frame itself must be consumed, not forwarded.
	if _, forwarded := findFrame[LLMMessagesAppendFrame](down); forwarded {
		t.Fatal("LLMMessagesAppendFrame should be consumed by ContextAggregator, not forwarded")
	}
}

func TestContextAggregator_AppendAddsMessages(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx, []Message{
		{Role: "system", Content: "sales prompt"},
	}, "")

	down, _ := runProcessorTest(t, fix, runConfig{
		processor: a,
		framesToSend: []Frame{
			LLMMessagesAppendFrame{Messages: []Message{{Role: "system", Content: "injected nudge"}}, RunLLM: true},
		},
		settleDelay:  100 * time.Millisecond,
		sendEndFrame: true,
	})

	llmMsg, ok := findFrame[LLMMessagesFrame](down)
	if !ok {
		t.Fatalf("expected LLMMessagesFrame, got %s", describeFrameTypes(down))
	}
	if len(llmMsg.Messages) != 2 {
		t.Fatalf("message count = %d, want 2 (prompt + appended)", len(llmMsg.Messages))
	}
	if llmMsg.Messages[1].Content != "injected nudge" {
		t.Fatalf("appended message = %q, want injected nudge", llmMsg.Messages[1].Content)
	}
}

func TestContextAggregator_FunctionCallFramesUpdateContextAndRunLLM(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx, []Message{
		{Role: "system", Content: "sales prompt"},
	}, "")

	source := newQueueProcessor(fix.TaskCtx, "test-source", Upstream)
	sink := newQueueProcessor(fix.TaskCtx, "test-sink", Downstream)
	source.Link(a)
	a.Link(sink)
	source.Start(fix.RootCtx)
	a.Start(fix.RootCtx)
	sink.Start(fix.RootCtx)

	sink.QueueFrame(NewFunctionCallInProgressFrame("get_guidance", "call_1", map[string]any{"situation": "pain"}, `{"situation":"pain"}`, false), Upstream)
	time.Sleep(20 * time.Millisecond)
	sink.QueueFrame(NewFunctionCallResultFrame("get_guidance", "call_1", map[string]any{"situation": "pain"}, `{"situation":"pain"}`, "guidance text", true), Upstream)
	time.Sleep(30 * time.Millisecond)

	source.QueueFrame(EndFrame{}, Downstream)
	if err := waitForWG(fix.WG, 3*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}

	if len(a.messages) != 3 {
		t.Fatalf("context messages = %+v, want prompt + assistant tool call + tool result", a.messages)
	}
	assistant := a.messages[1]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant tool message = %+v, want one tool call", assistant)
	}
	if assistant.ToolCalls[0].ID != "call_1" || assistant.ToolCalls[0].Function.Name != "get_guidance" {
		t.Fatalf("tool call = %+v, want call_1 get_guidance", assistant.ToolCalls[0])
	}
	if assistant.ToolCalls[0].Function.Arguments != `{"situation":"pain"}` {
		t.Fatalf("tool arguments = %q, want raw JSON", assistant.ToolCalls[0].Function.Arguments)
	}
	tool := a.messages[2]
	if tool.Role != "tool" || tool.ToolCallID != "call_1" || tool.Content != "guidance text" {
		t.Fatalf("tool result message = %+v, want call_1 guidance text", tool)
	}

	if c := countFrames[FunctionCallInProgressFrame](source.Captured()); c != 1 {
		t.Fatalf("expected FunctionCallInProgressFrame upstream once, got %d", c)
	}
	if c := countFrames[FunctionCallResultFrame](source.Captured()); c != 1 {
		t.Fatalf("expected FunctionCallResultFrame upstream once, got %d", c)
	}
	llmMsg, ok := findFrame[LLMMessagesFrame](sink.Captured())
	if !ok {
		t.Fatalf("expected LLMMessagesFrame after tool result, got %s", describeFrameTypes(sink.Captured()))
	}
	if len(llmMsg.Messages) != 3 || llmMsg.Messages[2].Role != "tool" || llmMsg.Messages[2].Content != "guidance text" {
		t.Fatalf("LLM context after tool result = %+v", llmMsg.Messages)
	}
}

func TestContextAggregator_FunctionCallsUsePipecatAssistantToolPairs(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx, []Message{
		{Role: "system", Content: "sales prompt"},
	}, "")

	source := newQueueProcessor(fix.TaskCtx, "test-source", Upstream)
	sink := newQueueProcessor(fix.TaskCtx, "test-sink", Downstream)
	source.Link(a)
	a.Link(sink)
	source.Start(fix.RootCtx)
	a.Start(fix.RootCtx)
	sink.Start(fix.RootCtx)

	sink.QueueFrame(NewFunctionCallInProgressFrame("get_guidance", "call_1", nil, `{"situation":"pain"}`, false), Upstream)
	sink.QueueFrame(NewFunctionCallInProgressFrame("lookup_plan", "call_2", nil, `{"plan":"starter"}`, false), Upstream)
	time.Sleep(20 * time.Millisecond)
	sink.QueueFrame(NewFunctionCallResultFrame("get_guidance", "call_1", nil, `{"situation":"pain"}`, "guidance text", false), Upstream)
	sink.QueueFrame(NewFunctionCallResultFrame("lookup_plan", "call_2", nil, `{"plan":"starter"}`, "plan text", true), Upstream)
	time.Sleep(30 * time.Millisecond)

	source.QueueFrame(EndFrame{}, Downstream)
	if err := waitForWG(fix.WG, 3*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}

	if len(a.messages) != 5 {
		t.Fatalf("context messages = %+v, want prompt + two assistant/tool pairs", a.messages)
	}
	assistant := a.messages[1]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 {
		t.Fatalf("first assistant tool message = %+v, want one tool call", assistant)
	}
	if assistant.ToolCalls[0].Function.Name != "get_guidance" {
		t.Fatalf("first tool call = %+v, want get_guidance", assistant.ToolCalls)
	}
	if a.messages[2].Role != "tool" || a.messages[2].ToolCallID != "call_1" || a.messages[2].Content != "guidance text" {
		t.Fatalf("first tool result = %+v", a.messages[2])
	}
	assistant = a.messages[3]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 {
		t.Fatalf("second assistant tool message = %+v, want one tool call", assistant)
	}
	if assistant.ToolCalls[0].Function.Name != "lookup_plan" {
		t.Fatalf("second tool call = %+v, want lookup_plan", assistant.ToolCalls)
	}
	if a.messages[4].Role != "tool" || a.messages[4].ToolCallID != "call_2" || a.messages[4].Content != "plan text" {
		t.Fatalf("second tool result = %+v", a.messages[4])
	}
	llmMsg, ok := findFrame[LLMMessagesFrame](sink.Captured())
	if !ok {
		t.Fatalf("expected LLMMessagesFrame after final tool result, got %s", describeFrameTypes(sink.Captured()))
	}
	if len(llmMsg.Messages) != 5 || len(llmMsg.Messages[1].ToolCalls) != 1 || len(llmMsg.Messages[3].ToolCalls) != 1 {
		t.Fatalf("LLM context after tool results = %+v", llmMsg.Messages)
	}
}

func TestContextAggregator_EmptyFunctionResultPushesError(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx, []Message{
		{Role: "system", Content: "sales prompt"},
	}, "")

	source := newQueueProcessor(fix.TaskCtx, "test-source", Upstream)
	sink := newQueueProcessor(fix.TaskCtx, "test-sink", Downstream)
	source.Link(a)
	a.Link(sink)
	source.Start(fix.RootCtx)
	a.Start(fix.RootCtx)
	sink.Start(fix.RootCtx)

	sink.QueueFrame(NewFunctionCallInProgressFrame("get_guidance", "call_1", nil, `{"situation":"pain"}`, false), Upstream)
	time.Sleep(20 * time.Millisecond)
	sink.QueueFrame(NewFunctionCallResultFrame("get_guidance", "call_1", nil, `{"situation":"pain"}`, "", true), Upstream)
	time.Sleep(30 * time.Millisecond)

	source.QueueFrame(EndFrame{}, Downstream)
	if err := waitForWG(fix.WG, 3*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}

	if c := countFrames[ErrorFrame](source.Captured()); c != 1 {
		t.Fatalf("expected one ErrorFrame for empty tool result, got %d in %s", c, describeFrameTypes(source.Captured()))
	}
	if len(a.messages) != 3 {
		t.Fatalf("context messages = %+v", a.messages)
	}
	tool := a.messages[2]
	if tool.Role != "tool" || tool.ToolCallID != "call_1" || !strings.Contains(tool.Content, "empty tool result") {
		t.Fatalf("tool result message = %+v, want explicit empty-result error", tool)
	}
}

func TestContextAggregator_EmitsToolResultCallEvent(t *testing.T) {
	fix := newTestFixture(t)
	var assistantToolCalls []Message
	var toolResults []Message
	fix.TaskCtx.callEvents = newCallEventDispatcher(fix.Logger, CallEvents{
		OnToolResultCommitted: func(assistantToolCall Message, toolResult Message, at time.Time) {
			assistantToolCalls = append(assistantToolCalls, assistantToolCall)
			toolResults = append(toolResults, toolResult)
		},
	})
	a := NewContextAggregator(fix.TaskCtx, []Message{{Role: "system", Content: "prompt"}}, "")

	source := newQueueProcessor(fix.TaskCtx, "test-source", Upstream)
	sink := newQueueProcessor(fix.TaskCtx, "test-sink", Downstream)
	source.Link(a)
	a.Link(sink)
	source.Start(fix.RootCtx)
	a.Start(fix.RootCtx)
	sink.Start(fix.RootCtx)

	sink.QueueFrame(NewFunctionCallInProgressFrame("get_guidance", "call_1", nil, `{"situation":"pain"}`, false), Upstream)
	time.Sleep(20 * time.Millisecond)
	sink.QueueFrame(NewFunctionCallResultFrame("get_guidance", "call_1", nil, `{"situation":"pain"}`, "guidance text", false), Upstream)
	time.Sleep(30 * time.Millisecond)

	source.QueueFrame(EndFrame{}, Downstream)
	if err := waitForWG(fix.WG, 3*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}
	fix.TaskCtx.callEvents.stopAndDrain()

	if len(assistantToolCalls) != 1 || len(toolResults) != 1 {
		t.Fatalf("tool events = assistant:%+v tool:%+v", assistantToolCalls, toolResults)
	}
	if assistantToolCalls[0].Role != "assistant" ||
		len(assistantToolCalls[0].ToolCalls) != 1 ||
		assistantToolCalls[0].ToolCalls[0].ID != "call_1" ||
		assistantToolCalls[0].ToolCalls[0].Function.Name != "get_guidance" ||
		assistantToolCalls[0].ToolCalls[0].Function.Arguments != `{"situation":"pain"}` {
		t.Fatalf("assistant tool event = %+v", assistantToolCalls[0])
	}
	if toolResults[0].Role != "tool" || toolResults[0].ToolCallID != "call_1" || toolResults[0].Content != "guidance text" {
		t.Fatalf("tool result event = %+v", toolResults[0])
	}
}

func TestContextAggregator_ExplicitEmptyInitialMessagesSkipsDefaultPrompt(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx, []Message{}, "")

	down, _ := runProcessorTest(t, fix, runConfig{
		processor: a,
		framesToSend: []Frame{
			TranscriptFrame{Text: "hello", IsFinal: true},
			TranscriptFrame{Text: "<end>", IsFinal: true},
		},
		sendEndFrame: true,
	})

	llmMsg, ok := findFrame[LLMMessagesFrame](down)
	if !ok {
		t.Fatalf("expected LLMMessagesFrame, got %s", describeFrameTypes(down))
	}
	if len(llmMsg.Messages) != 1 {
		t.Fatalf("message count = %d, want only the user message", len(llmMsg.Messages))
	}
	if llmMsg.Messages[0].Role != "user" {
		t.Fatalf("first message role = %q, want user", llmMsg.Messages[0].Role)
	}
}

// TestContextAggregator_BargeInEmitsInterrupt verifies that >= 3 interim
// words during bot speech triggers an InterruptFrame downstream.
func TestContextAggregator_BargeInEmitsInterrupt(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx, nil, "")

	down, _ := runProcessorTest(t, fix, runConfig{
		processor: a,
		framesToSend: []Frame{
			BotStartedSpeakingFrame{}, // arrives upstream from PlaybackSink; ContextAggregator sets botSpeaking
			TranscriptFrame{Text: "one two three", IsFinal: false, ResponseID: 1}, // 3 words → barge-in
		},
		settleDelay:  30 * time.Millisecond,
		sendEndFrame: true,
	})

	if c := countFrames[InterruptFrame](down); c != 1 {
		t.Errorf("expected 1 InterruptFrame downstream, got %d: %s", c, describeFrameTypes(down))
	}
}

// TestContextAggregator_NoBargeInBelowThreshold verifies that fewer
// than 3 interim words does NOT trigger an InterruptFrame.
func TestContextAggregator_NoBargeInBelowThreshold(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx, nil, "")

	down, _ := runProcessorTest(t, fix, runConfig{
		processor: a,
		framesToSend: []Frame{
			BotStartedSpeakingFrame{},
			TranscriptFrame{Text: "hi", IsFinal: false, ResponseID: 1}, // 1 word
		},
		settleDelay:  30 * time.Millisecond,
		sendEndFrame: true,
	})

	if c := countFrames[InterruptFrame](down); c != 0 {
		t.Errorf("expected no InterruptFrame for below-threshold transcript, got %d", c)
	}
}

// TestContextAggregator_BackchannelDiscardedWhenBotFinishesFirst verifies
// the race-case bug fix: when the user speaks below the barge-in
// threshold WHILE the bot is talking, AND the bot finishes (TTSDoneFrame
// arrives) BEFORE Soniox emits the <end> for that speech, the lagging
// <end> must NOT submit those words as a user turn. Mirrors Pipecat's
// reset_aggregation behavior at the bot-turn boundary.
func TestContextAggregator_BackchannelDiscardedWhenBotFinishesFirst(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx, nil, "")

	source := newQueueProcessor(fix.TaskCtx, "test-source", Upstream)
	sink := newQueueProcessor(fix.TaskCtx, "test-sink", Downstream)
	source.Link(a)
	a.Link(sink)
	source.Start(fix.RootCtx)
	a.Start(fix.RootCtx)
	sink.Start(fix.RootCtx)

	// Bot starts speaking.
	sink.QueueFrame(BotStartedSpeakingFrame{}, Upstream)
	time.Sleep(20 * time.Millisecond)

	// User says "okay" (1 word, below 3-word threshold). Interim then
	// final, but NO <end> yet.
	source.QueueFrame(TranscriptFrame{Text: "okay", IsFinal: false, ResponseID: 1}, Downstream)
	source.QueueFrame(TranscriptFrame{Text: "okay", IsFinal: true, ResponseID: 1}, Downstream)
	time.Sleep(20 * time.Millisecond)

	// Bot finishes naturally — TTSDoneFrame arrives BEFORE Soniox's <end>.
	sink.QueueFrame(TTSDoneFrame{}, Upstream)
	time.Sleep(20 * time.Millisecond)

	// Now Soniox finally emits <end> for the backchannel. The aggregator
	// must NOT submit "okay" as a user turn.
	source.QueueFrame(TranscriptFrame{Text: "<end>", IsFinal: true, ResponseID: 1}, Downstream)
	time.Sleep(20 * time.Millisecond)

	source.QueueFrame(EndFrame{}, Downstream)
	if err := waitForWG(fix.WG, 3*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}

	if c := countFrames[LLMMessagesFrame](sink.Captured()); c != 0 {
		t.Errorf("expected NO LLMMessagesFrame for back-channel speech, got %d in %s", c, describeFrameTypes(sink.Captured()))
	}
	if c := countFrames[InterruptFrame](sink.Captured()); c != 0 {
		t.Errorf("did not expect InterruptFrame for sub-threshold speech, got %d", c)
	}
	// And no user-role message should have been appended.
	for _, m := range a.messages {
		if m.Role == "user" {
			t.Errorf("aggregator should not have a user message; got %q", m.Content)
		}
	}
}

// TestContextAggregator_BackchannelDiscardedWhileBotStillSpeaking
// verifies the existing in-progress discard branch still works: when
// <end> arrives WHILE botSpeaking is still true, the below-threshold
// transcript is discarded synchronously.
func TestContextAggregator_BackchannelDiscardedWhileBotStillSpeaking(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx, nil, "")

	source := newQueueProcessor(fix.TaskCtx, "test-source", Upstream)
	sink := newQueueProcessor(fix.TaskCtx, "test-sink", Downstream)
	source.Link(a)
	a.Link(sink)
	source.Start(fix.RootCtx)
	a.Start(fix.RootCtx)
	sink.Start(fix.RootCtx)

	// Bot starts and remains speaking; user mumbles "yeah" (1 word).
	sink.QueueFrame(BotStartedSpeakingFrame{}, Upstream)
	time.Sleep(20 * time.Millisecond)
	source.QueueFrame(TranscriptFrame{Text: "yeah", IsFinal: false, ResponseID: 1}, Downstream)
	source.QueueFrame(TranscriptFrame{Text: "yeah", IsFinal: true, ResponseID: 1}, Downstream)
	source.QueueFrame(TranscriptFrame{Text: "<end>", IsFinal: true, ResponseID: 1}, Downstream)
	time.Sleep(30 * time.Millisecond)

	source.QueueFrame(EndFrame{}, Downstream)
	if err := waitForWG(fix.WG, 3*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}

	if c := countFrames[LLMMessagesFrame](sink.Captured()); c != 0 {
		t.Errorf("expected NO LLMMessagesFrame; in-progress discard should fire, got %d", c)
	}
	for _, m := range a.messages {
		if m.Role == "user" {
			t.Errorf("aggregator should not have a user message; got %q", m.Content)
		}
	}
}

// TestContextAggregator_BargeInPreservesUserTranscript verifies the
// regression boundary: when the user DOES cross the barge-in threshold,
// their accumulated speech is NOT reset by the TTSDone path (since
// barge-in fires before TTSDone in our flow, and interruptSent gates
// the reset).
func TestContextAggregator_BargeInPreservesUserTranscript(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx, nil, "")

	source := newQueueProcessor(fix.TaskCtx, "test-source", Upstream)
	sink := newQueueProcessor(fix.TaskCtx, "test-sink", Downstream)
	source.Link(a)
	a.Link(sink)
	source.Start(fix.RootCtx)
	a.Start(fix.RootCtx)
	sink.Start(fix.RootCtx)

	sink.QueueFrame(BotStartedSpeakingFrame{}, Upstream)
	time.Sleep(20 * time.Millisecond)

	// User says "I have a question" — 4 words → barge-in fires.
	source.QueueFrame(TranscriptFrame{Text: "I have a question", IsFinal: false, ResponseID: 1}, Downstream)
	time.Sleep(20 * time.Millisecond)
	// Now final tokens + <end> arrive after barge-in.
	source.QueueFrame(TranscriptFrame{Text: "I have a question", IsFinal: true, ResponseID: 1}, Downstream)
	source.QueueFrame(TranscriptFrame{Text: "<end>", IsFinal: true, ResponseID: 1}, Downstream)
	time.Sleep(30 * time.Millisecond)

	source.QueueFrame(EndFrame{}, Downstream)
	if err := waitForWG(fix.WG, 3*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}

	if c := countFrames[InterruptFrame](sink.Captured()); c != 1 {
		t.Errorf("expected 1 InterruptFrame, got %d", c)
	}
	llmMsg, ok := findFrame[LLMMessagesFrame](sink.Captured())
	if !ok {
		t.Fatalf("expected LLMMessagesFrame after barge-in, got %s", describeFrameTypes(sink.Captured()))
	}
	last := llmMsg.Messages[len(llmMsg.Messages)-1]
	if last.Role != "user" || last.Content != "I have a question" {
		t.Errorf("user message: got %q=%q, want user='I have a question'", last.Role, last.Content)
	}
}

// TestContextAggregator_TTSDoneCommitsAssistantMessage verifies that a
// TTSDoneFrame arriving upstream commits accumulated WordTimestampFrame
// words as an assistant message.
func TestContextAggregator_TTSDoneCommitsAssistantMessage(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx, nil, "")

	// Drive the aggregator: user message → words spoken → TTSDone.
	// To emit upstream frames, we'd need a processor downstream that
	// pushes them upstream; simplest is to call QueueFrame directly with
	// Upstream direction.
	source := newQueueProcessor(fix.TaskCtx, "test-source", Upstream)
	sink := newQueueProcessor(fix.TaskCtx, "test-sink", Downstream)
	source.Link(a)
	a.Link(sink)
	source.Start(fix.RootCtx)
	a.Start(fix.RootCtx)
	sink.Start(fix.RootCtx)

	// Send a user message to populate messages list.
	source.QueueFrame(TranscriptFrame{Text: "hello", IsFinal: true}, Downstream)
	source.QueueFrame(TranscriptFrame{Text: "<end>", IsFinal: true}, Downstream)
	time.Sleep(20 * time.Millisecond)

	// Now send WordTimestampFrames upstream (simulating PlaybackSink
	// emitting words as bot speaks).
	sink.QueueFrame(WordTimestampFrame{Words: []string{"hi"}}, Upstream)
	sink.QueueFrame(WordTimestampFrame{Words: []string{"there"}}, Upstream)
	time.Sleep(20 * time.Millisecond)

	// TTSDone arrives upstream → commits.
	sink.QueueFrame(TTSDoneFrame{}, Upstream)
	time.Sleep(20 * time.Millisecond)

	source.QueueFrame(EndFrame{}, Downstream)
	if err := waitForWG(fix.WG, 3*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}

	// After commit, the aggregator's messages should contain an assistant
	// message with the spoken words.
	var sawAssistant bool
	for _, m := range a.messages {
		if m.Role == "assistant" {
			sawAssistant = true
			if m.Content != "hi there" {
				t.Errorf("assistant content: got %q, want 'hi there'", m.Content)
			}
		}
	}
	if !sawAssistant {
		t.Error("expected an assistant message after TTSDone commit")
	}
}

func TestContextAggregator_EmitsCommittedTurnCallEventsWithMetrics(t *testing.T) {
	fix := newTestFixture(t)
	var users []string
	var assistants []string
	var metrics []TurnMetrics
	var userPromptKeys []string
	var assistantPromptKeys []string
	fix.TaskCtx.callEvents = newCallEventDispatcher(fix.Logger, CallEvents{
		OnUserTurnCommitted: func(text string, at time.Time, promptKey string) {
			users = append(users, text)
			userPromptKeys = append(userPromptKeys, promptKey)
		},
		OnAssistantTurnCommitted: func(text string, at time.Time, m TurnMetrics, promptKey string) {
			assistants = append(assistants, text)
			metrics = append(metrics, m)
			assistantPromptKeys = append(assistantPromptKeys, promptKey)
		},
	})
	a := NewContextAggregator(fix.TaskCtx, nil, "sales_call/main_sys-3day_v2_v17")

	fix.TaskCtx.metrics.absorb(NewMetricsFrame([]MetricsData{
		{Processor: "llm", Label: MetricTTFB, ValueMs: 12},
		{Processor: "tts", Label: MetricTTFB, ValueMs: 34},
	}))
	a.addUserMessage("hello")
	a.appendWords([]string{"hi", "there"})
	a.commitSpokenText(false)
	fix.TaskCtx.callEvents.stopAndDrain()

	if len(users) != 1 || users[0] != "hello" {
		t.Fatalf("user turn events = %v, want [hello]", users)
	}
	if len(assistants) != 1 || assistants[0] != "hi there" {
		t.Fatalf("assistant turn events = %v, want [hi there]", assistants)
	}
	if len(metrics) != 1 || metrics[0].LLMTTFBMs != 12 || metrics[0].TTSTTFBMs != 34 {
		t.Fatalf("assistant metrics = %+v, want llm=12 tts=34", metrics)
	}
	if len(userPromptKeys) != 1 || userPromptKeys[0] != "sales_call/main_sys-3day_v2_v17" {
		t.Fatalf("user prompt keys = %v, want sales prompt key", userPromptKeys)
	}
	if len(assistantPromptKeys) != 1 || assistantPromptKeys[0] != "sales_call/main_sys-3day_v2_v17" {
		t.Fatalf("assistant prompt keys = %v, want sales prompt key", assistantPromptKeys)
	}
}

func TestContextAggregator_UserFirstSpeechLifecycleFiresOnce(t *testing.T) {
	fix := newTestFixture(t)
	calls := make(chan time.Time, 2)
	fix.TaskCtx.callEvents = newCallEventDispatcher(fix.Logger, CallEvents{
		OnUserFirstSpeech: func(at time.Time) { calls <- at },
	})
	defer fix.TaskCtx.callEvents.stopAndDrain()
	a := NewContextAggregator(fix.TaskCtx, nil, "")

	runProcessorTest(t, fix, runConfig{
		processor: a,
		framesToSend: []Frame{
			TranscriptFrame{Text: "first", IsFinal: true},
			TranscriptFrame{Text: "<end>", IsFinal: true},
			TranscriptFrame{Text: "second", IsFinal: true},
			TranscriptFrame{Text: "<end>", IsFinal: true},
		},
		sendEndFrame: true,
	})
	fix.TaskCtx.callEvents.stopAndDrain()

	select {
	case <-calls:
	default:
		t.Fatal("OnUserFirstSpeech was not called")
	}
	select {
	case <-calls:
		t.Fatal("OnUserFirstSpeech should only fire once")
	default:
	}
}
