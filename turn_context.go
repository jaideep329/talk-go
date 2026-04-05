package main

import (
	"context"
	"sync"
	"time"
)

// TurnContext is shared across processors for per-turn cancellation and
// tracking which words were actually spoken. Words are appended by
// PlaybackSink as they are played (interleaved with audio by TTS).
// LLM calls Reset() at the start of each turn and Cancel() on barge-in.
type TurnContext struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc

	words        []string
	turnStarted  time.Time // when user finished speaking (<end> token)
	playbackDone chan struct{}
}

func NewTurnContext() *TurnContext {
	ctx, cancel := context.WithCancel(context.Background())
	return &TurnContext{ctx: ctx, cancel: cancel, playbackDone: make(chan struct{})}
}

// Reset creates a new context for a new turn.
func (t *TurnContext) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ctx, t.cancel = context.WithCancel(context.Background())
	t.turnStarted = time.Now()
	t.playbackDone = make(chan struct{})
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

// PlaybackDone returns a channel that closes when all audio for this turn has been played.
func (t *TurnContext) PlaybackDone() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.playbackDone
}

// MarkPlaybackDone signals that all audio for this turn has been played.
func (t *TurnContext) MarkPlaybackDone() {
	t.mu.Lock()
	defer t.mu.Unlock()
	select {
	case <-t.playbackDone:
	default:
		close(t.playbackDone)
	}
}

// AppendWords adds words that have been played to the user.
func (t *TurnContext) AppendWords(words []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, w := range words {
		if len(t.words) > 0 && len(w) > 0 && w[0] != '.' && w[0] != ',' && w[0] != '!' && w[0] != '?' && w[0] != ';' && w[0] != ':' {
			t.words = append(t.words, " "+w)
		} else {
			t.words = append(t.words, w)
		}
	}
}

// SpokenSoFar returns all words that were played and resets. Used on barge-in.
func (t *TurnContext) SpokenSoFar() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var spoken string
	for _, w := range t.words {
		spoken += w
	}
	t.words = nil
	return spoken
}

// FlushAll returns all words (full response completed) and resets.
func (t *TurnContext) FlushAll() string {
	return t.SpokenSoFar()
}
