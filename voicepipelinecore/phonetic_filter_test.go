package voicepipelinecore

import "testing"

func TestNewPhoneticFilterNilOnEmptyDict(t *testing.T) {
	if f := newPhoneticFilter(nil); f != nil {
		t.Fatalf("nil dict should yield nil filter, got %v", f)
	}
	if f := newPhoneticFilter(map[string]string{}); f != nil {
		t.Fatalf("empty dict should yield nil filter, got %v", f)
	}
}

func TestPhoneticFilterApply(t *testing.T) {
	f := newPhoneticFilter(map[string]string{
		"curelink": "cure link",
		"DISHA":    "deesha", // mixed case key — lookup is case-insensitive
	})

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"replaces token", "welcome to curelink", "welcome to cure link"},
		{"case-insensitive key and token", "Hi Disha", "Hi deesha"},
		{"leaves unknown tokens", "hello world", "hello world"},
		{"drops bare tool call", "functions.update_stage()", ""},
		{"strips tool call keeps text", "okay functions.update_agenda({\"a\":1}) bye", "okay  bye"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.apply(tc.in); got != tc.want {
				t.Fatalf("apply(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPhoneticFilterApplyNilReceiverPassthrough(t *testing.T) {
	var f *phoneticFilter
	if got := f.apply("unchanged"); got != "unchanged" {
		t.Fatalf("nil filter should pass text through, got %q", got)
	}
}
