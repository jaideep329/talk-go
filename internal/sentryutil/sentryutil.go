package sentryutil

import (
	"strings"

	"github.com/getsentry/sentry-go"
)

type Event struct {
	Err     error
	Message string
	Tags    map[string]string
	Details map[string]any
}

func Capture(event Event) {
	if event.Err == nil && strings.TrimSpace(event.Message) == "" {
		return
	}
	sentry.WithScope(func(scope *sentry.Scope) {
		if len(event.Tags) > 0 {
			scope.SetTags(event.Tags)
		}
		if len(event.Details) > 0 {
			scope.SetContext("details", sentry.Context(event.Details))
		}
		if event.Err != nil {
			sentry.CaptureException(event.Err)
			return
		}
		scope.SetLevel(sentry.LevelError)
		sentry.CaptureMessage(event.Message)
	})
}
