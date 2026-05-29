package disha

import "testing"

func TestUnresolvedTemplateTokens(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"clean", "Hello Riya, today is 2 Jan 2026.", ""},
		{"unfilled var", "Hello {{ name }}.", "{{ name }}"},
		{"jinja control", "{% if x %}yes{% endif %}", "{% if x %}, {% endif %}"},
		{"mixed", "{{ a }} and {% for x in y %}", "{{ a }}, {% for x in y %}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unresolvedTemplateTokens(tc.text); got != tc.want {
				t.Fatalf("unresolvedTemplateTokens(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestRenderTemplateSubstitutesBareVars(t *testing.T) {
	got := renderTemplate("Hi {{ name }}, {{ unknown }}", map[string]string{"name": "Riya"})
	// Known var is substituted; unknown is left intact for the leftover check.
	if want := "Hi Riya, {{ unknown }}"; got != want {
		t.Fatalf("renderTemplate = %q, want %q", got, want)
	}
}
