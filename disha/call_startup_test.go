package disha

import "testing"

// TestMessageFromChunkTuple pins the filter to sales_call.py: replay a
// chunk iff it is not a debug log and has no (truthy) additional_data;
// role and text are taken verbatim with no further filtering.
func TestMessageFromChunkTuple(t *testing.T) {
	cases := []struct {
		name     string
		tuple    []any
		wantOK   bool
		wantRole string
		wantText string
	}{
		{"user turn", []any{"id", "user", "hello", false, nil}, true, "user", "hello"},
		{"assistant turn", []any{"id", "assistant", "hi", false, nil}, true, "assistant", "hi"},
		{"debug dropped", []any{"id", "assistant", "x", true, nil}, false, "", ""},
		{"additional_data dropped", []any{"id", "assistant", "x", false, map[string]any{"k": "v"}}, false, "", ""},
		{"empty additional_data kept", []any{"id", "assistant", "x", false, map[string]any{}}, true, "assistant", "x"},
		{"non-standard role kept", []any{"id", "tool", "tool turn", false, nil}, true, "tool", "tool turn"},
		{"empty text kept", []any{"id", "user", "", false, nil}, true, "user", ""},
		{"four-element tuple kept", []any{"id", "user", "hi", false}, true, "user", "hi"},
		{"too short dropped", []any{"id", "user", "hi"}, false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := messageFromChunkTuple(tc.tuple)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && (msg.Role != tc.wantRole || msg.Content != tc.wantText) {
				t.Fatalf("msg = %+v, want {Role:%q Content:%q}", msg, tc.wantRole, tc.wantText)
			}
		})
	}
}

func TestIsTruthyJSON(t *testing.T) {
	truthy := []any{true, float64(1), "x", map[string]any{"k": 1}, []any{1}, struct{}{}}
	falsy := []any{nil, false, float64(0), "", map[string]any{}, []any{}}
	for _, v := range truthy {
		if !isTruthyJSON(v) {
			t.Errorf("isTruthyJSON(%#v) = false, want true", v)
		}
	}
	for _, v := range falsy {
		if isTruthyJSON(v) {
			t.Errorf("isTruthyJSON(%#v) = true, want false", v)
		}
	}
}
