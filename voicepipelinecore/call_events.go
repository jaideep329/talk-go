package voicepipelinecore

import (
	"log"
	"sync"
	"time"
)

type callEventDispatcher struct {
	logger *log.Logger
	wg     *sync.WaitGroup

	onBotJoined       func(time.Time)
	onUserJoined      func(time.Time)
	onUserFirstSpeech func(time.Time)
	onBotFirstSpeech  func(time.Time)
	onFirstUserAudio  func(time.Time)

	botJoinedOnce       sync.Once
	userJoinedOnce      sync.Once
	userFirstSpeechOnce sync.Once
	botFirstSpeechOnce  sync.Once
	firstUserAudioOnce  sync.Once
}

func newCallEventDispatcher(logger *log.Logger, wg *sync.WaitGroup, events CallEvents) *callEventDispatcher {
	return &callEventDispatcher{
		logger:            logger,
		wg:                wg,
		onBotJoined:       events.OnBotJoined,
		onUserJoined:      events.OnUserJoined,
		onUserFirstSpeech: events.OnUserFirstSpeech,
		onBotFirstSpeech:  events.OnBotFirstSpeech,
		onFirstUserAudio:  events.OnFirstUserAudio,
	}
}

func (l *callEventDispatcher) fireBotJoined(at time.Time) {
	if l == nil || l.onBotJoined == nil {
		return
	}
	l.botJoinedOnce.Do(func() {
		l.run("OnBotJoined", func() { l.onBotJoined(at) })
	})
}

func (l *callEventDispatcher) fireUserJoined(at time.Time) {
	if l == nil || l.onUserJoined == nil {
		return
	}
	l.userJoinedOnce.Do(func() {
		l.run("OnUserJoined", func() { l.onUserJoined(at) })
	})
}

func (l *callEventDispatcher) fireUserFirstSpeech(at time.Time) {
	if l == nil || l.onUserFirstSpeech == nil {
		return
	}
	l.userFirstSpeechOnce.Do(func() {
		l.run("OnUserFirstSpeech", func() { l.onUserFirstSpeech(at) })
	})
}

func (l *callEventDispatcher) fireBotFirstSpeech(at time.Time) {
	if l == nil || l.onBotFirstSpeech == nil {
		return
	}
	l.botFirstSpeechOnce.Do(func() {
		l.run("OnBotFirstSpeech", func() { l.onBotFirstSpeech(at) })
	})
}

func (l *callEventDispatcher) fireFirstUserAudio(at time.Time) {
	if l == nil || l.onFirstUserAudio == nil {
		return
	}
	l.firstUserAudioOnce.Do(func() {
		l.run("OnFirstUserAudio", func() { l.onFirstUserAudio(at) })
	})
}

func (l *callEventDispatcher) run(name string, fn func()) {
	if l.wg != nil {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.runSafely(name, fn)
		}()
		return
	}
	go l.runSafely(name, fn)
}

func (l *callEventDispatcher) runSafely(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil && l.logger != nil {
			l.logger.Printf("%s callback panicked: %v\n", name, r)
		}
	}()
	fn()
}
