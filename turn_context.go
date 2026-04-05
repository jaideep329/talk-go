package main

import (
	"context"
	"sync"
	"time"
)

// TurnContext is shared across processors for per-turn cancellation and
// tracking which words were actually spoken. LLM calls Reset() at the
// start of each turn and Cancel() on barge-in. TTS and PlaybackSink
// select on Done() to react instantly. PlaybackSink appends words via
// AppendWords(). LLM reads them via SpokenSoFar() or FlushAll().
type TurnContext struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc

	// Spoken word tracking
	words           []string
	startTimes      []float64 // seconds from context start
	playbackStarted time.Time // when first audio frame was played
	turnStarted     time.Time // when user finished speaking (<end> token)
}

func NewTurnContext() *TurnContext {
	ctx, cancel := context.WithCancel(context.Background())
	return &TurnContext{ctx: ctx, cancel: cancel}
}

// Reset creates a new context for a new turn.
func (t *TurnContext) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ctx, t.cancel = context.WithCancel(context.Background())
	t.turnStarted = time.Now()
}

// TurnStarted returns when the current turn started.
func (t *TurnContext) TurnStarted() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.turnStarted
}

// Cancel cancels the current turn (barge-in).
func (t *TurnContext) Cancel() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cancel()
}

// Done returns the current turn's done channel to select on.
func (t *TurnContext) Done() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ctx.Done()
}

// StartPlayback records when audio playback began for this turn.
func (t *TurnContext) StartPlayback() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.playbackStarted = time.Now()
}

// AppendWords adds words and their start times.
func (t *TurnContext) AppendWords(words []string, startTimes []float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, w := range words {
		if len(t.words) > 0 && len(w) > 0 && w[0] != '.' && w[0] != ',' && w[0] != '!' && w[0] != '?' && w[0] != ';' && w[0] != ':' {
			t.words = append(t.words, " "+w)
		} else {
			t.words = append(t.words, w)
		}
		if i < len(startTimes) {
			t.startTimes = append(t.startTimes, startTimes[i])
		}
	}
}

// SpokenSoFar returns only the words that were actually spoken based on
// elapsed playback time, then resets. Used on barge-in.
func (t *TurnContext) SpokenSoFar() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	elapsed := time.Since(t.playbackStarted).Seconds()
	var spoken string
	for i, w := range t.words {
		if i < len(t.startTimes) && t.startTimes[i] <= elapsed {
			spoken += w
		}
	}
	t.words = nil
	t.startTimes = nil
	return spoken
}

// FlushAll returns all words (full response completed) and resets.
func (t *TurnContext) FlushAll() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var text string
	for _, w := range t.words {
		text += w
	}
	t.words = nil
	t.startTimes = nil
	return text
}
