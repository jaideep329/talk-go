package voicepipelinecore

import (
	"regexp"
	"strings"
)

// phoneticFilter rewrites text before it is sent to the TTS provider so
// brand names and Hindi words are pronounced correctly. The only thing
// supplied from outside the pipeline is the dictionary itself (a
// token → replacement map, passed via TaskOptions.PhoneticDict); the
// filtering logic lives here in the core.
//
// Ported from Disha's bots/onboarding_call/phonetic_text_filter.py.
type phoneticFilter struct {
	dict     map[string]string
	tokenRe  *regexp.Regexp
	fnCallRe *regexp.Regexp
}

// newPhoneticFilter builds a filter from the supplied dictionary.
// Returns nil when the dictionary is empty so callers can cheaply skip
// filtering. Keys are lowercased so lookups are case-insensitive.
func newPhoneticFilter(dict map[string]string) *phoneticFilter {
	if len(dict) == 0 {
		return nil
	}
	lower := make(map[string]string, len(dict))
	for k, v := range dict {
		lower[strings.ToLower(k)] = v
	}
	return &phoneticFilter{
		dict: lower,
		// RE2 (Go's regexp) has no lookbehind/lookahead, so we approximate
		// Python's `(?<!\w)([...]+)(?!\w)` by greedily matching contiguous
		// runs of the same character class. Devanagari dandas ।॥ are
		// matched as standalone tokens.
		tokenRe:  regexp.MustCompile(`[a-zA-Z0-9\x{0900}-\x{0963}\x{0966}-\x{097F}]+|[\x{0964}\x{0965}]`),
		fnCallRe: regexp.MustCompile(`functions\.(?:update_stage|update_agenda)(?:\s*\([\s\S]*?\)|\s*\{[\s\S]*?\})?`),
	}
}

// apply strips any `functions.update_*` tool-call leftovers, then maps
// every lowercased token through the dictionary. Returns "" when the
// text becomes non-speakable (e.g. it was only a leftover tool call).
func (f *phoneticFilter) apply(text string) string {
	if f == nil {
		return text
	}
	cleaned := text
	if strings.Contains(cleaned, "functions.update") {
		cleaned = f.fnCallRe.ReplaceAllString(cleaned, "")
		if strings.TrimSpace(cleaned) == "" {
			return ""
		}
	}
	translated := f.tokenRe.ReplaceAllStringFunc(cleaned, func(token string) string {
		if replacement, ok := f.dict[strings.ToLower(token)]; ok && replacement != "" {
			return replacement
		}
		return token
	})
	translated = strings.TrimSpace(translated)
	if !containsSpeakable(translated) {
		return ""
	}
	return translated
}

func containsSpeakable(text string) bool {
	for _, r := range text {
		if isAlnumRune(r) {
			return true
		}
	}
	return false
}

func isAlnumRune(r rune) bool {
	switch {
	case r >= '0' && r <= '9':
		return true
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= 0x0900 && r <= 0x097F:
		return true
	}
	return false
}
