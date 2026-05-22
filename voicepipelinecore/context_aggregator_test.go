package voicepipelinecore

import (
	"testing"
	"time"
)

// TestContextAggregator_FinalTranscriptEmitsLLMMessages verifies a
// final transcript ending with <end> produces an LLMMessagesFrame
// downstream.
func TestContextAggregator_FinalTranscriptEmitsLLMMessages(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx)

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
	if llmMsg.Messages[0]["role"] != "system" {
		t.Errorf("first message should be system, got %q", llmMsg.Messages[0]["role"])
	}
	last := llmMsg.Messages[len(llmMsg.Messages)-1]
	if last["role"] != "user" {
		t.Errorf("last message should be user, got %q", last["role"])
	}
	if last["content"] != "hello" {
		t.Errorf("user content: got %q, want 'hello'", last["content"])
	}
}

func TestContextAggregator_InitialMessagesSeedLLMContext(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx, []Message{
		{Role: "system", Content: "seed context"},
		{Role: "assistant", Content: "hello seed"},
	})

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
	if llmMsg.Messages[0]["content"] != "seed context" {
		t.Fatalf("first message content = %q, want seed context", llmMsg.Messages[0]["content"])
	}
	last := llmMsg.Messages[len(llmMsg.Messages)-1]
	if last["role"] != "user" || last["content"] != "new user" {
		t.Fatalf("last message = %+v, want user new user", last)
	}
}

func TestContextAggregator_ExplicitEmptyInitialMessagesSkipsDefaultPrompt(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx, []Message{})

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
	if llmMsg.Messages[0]["role"] != "user" {
		t.Fatalf("first message role = %q, want user", llmMsg.Messages[0]["role"])
	}
}

// TestContextAggregator_BargeInEmitsInterrupt verifies that >= 3 interim
// words during bot speech triggers an InterruptFrame downstream.
func TestContextAggregator_BargeInEmitsInterrupt(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx)

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
	a := NewContextAggregator(fix.TaskCtx)

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
	a := NewContextAggregator(fix.TaskCtx)

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
		if m["role"] == "user" {
			t.Errorf("aggregator should not have a user message; got %q", m["content"])
		}
	}
}

// TestContextAggregator_BackchannelDiscardedWhileBotStillSpeaking
// verifies the existing in-progress discard branch still works: when
// <end> arrives WHILE botSpeaking is still true, the below-threshold
// transcript is discarded synchronously.
func TestContextAggregator_BackchannelDiscardedWhileBotStillSpeaking(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx)

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
		if m["role"] == "user" {
			t.Errorf("aggregator should not have a user message; got %q", m["content"])
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
	a := NewContextAggregator(fix.TaskCtx)

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
	if last["role"] != "user" || last["content"] != "I have a question" {
		t.Errorf("user message: got %q=%q, want user='I have a question'", last["role"], last["content"])
	}
}

// TestContextAggregator_TTSDoneCommitsAssistantMessage verifies that a
// TTSDoneFrame arriving upstream commits accumulated WordTimestampFrame
// words as an assistant message.
func TestContextAggregator_TTSDoneCommitsAssistantMessage(t *testing.T) {
	fix := newTestFixture(t)
	a := NewContextAggregator(fix.TaskCtx)

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
		if m["role"] == "assistant" {
			sawAssistant = true
			if m["content"] != "hi there" {
				t.Errorf("assistant content: got %q, want 'hi there'", m["content"])
			}
		}
	}
	if !sawAssistant {
		t.Error("expected an assistant message after TTSDone commit")
	}
}

func TestContextAggregator_EmitsObserverCommittedTurnsWithMetrics(t *testing.T) {
	fix := newTestFixture(t)
	obs := &recordingObserver{}
	fix.TaskCtx.turnObservers = newConversationTurnObserverSet(fix.TaskCtx, []ConversationTurnObserver{obs})
	defer fix.TaskCtx.turnObservers.stopAndDrain()
	a := NewContextAggregator(fix.TaskCtx)

	fix.TaskCtx.metrics.absorb(NewMetricsFrame([]MetricsData{
		{Processor: "llm", Label: MetricTTFB, ValueMs: 12},
		{Processor: "tts", Label: MetricTTFB, ValueMs: 34},
	}))
	a.addUserMessage("hello")
	a.appendWords([]string{"hi", "there"})
	a.commitSpokenText(false)
	fix.TaskCtx.turnObservers.stopAndDrain()

	if err := waitForWG(fix.WG, 2*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.users) != 1 || obs.users[0] != "hello" {
		t.Fatalf("user turn observer events = %v, want [hello]", obs.users)
	}
	if len(obs.assistants) != 1 || obs.assistants[0] != "hi there" {
		t.Fatalf("assistant turn observer events = %v, want [hi there]", obs.assistants)
	}
	if len(obs.metrics) != 1 || obs.metrics[0].LLMTTFBMs != 12 || obs.metrics[0].TTSTTFBMs != 34 {
		t.Fatalf("assistant metrics = %+v, want llm=12 tts=34", obs.metrics)
	}
}

func TestContextAggregator_UserFirstSpeechLifecycleFiresOnce(t *testing.T) {
	fix := newTestFixture(t)
	calls := make(chan time.Time, 2)
	fix.TaskCtx.callEvents = newCallEventDispatcher(fix.Logger, fix.WG, CallEvents{
		OnUserFirstSpeech: func(at time.Time) { calls <- at },
	})
	a := NewContextAggregator(fix.TaskCtx)

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
