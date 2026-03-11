package main

import (
	"context"
	"log"
	"unicode"
	"unicode/utf8"
)

func endsWithPunctuation(s string) bool {
	if s == "" {
		return false
	}
	lastRune, _ := utf8.DecodeLastRuneInString(s)
	return unicode.IsPunct(lastRune)
}

type TTSProcessor struct {
	currentAggregation string
}

func (t *TTSProcessor) Process(ctx context.Context, in <-chan Frame, out chan<- Frame) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-in:
			if !ok {
				return
			}
			switch f := frame.(type) {
			case TextFrame:
				t.currentAggregation += f.Text
				if endsWithPunctuation(t.currentAggregation) {

				}
				log.Printf("Received text frame: %s\n", f.Text)
				out <- f
			default:
				log.Printf("Received non-text frame of type %T\n", frame)
			}
		}
	}
}
