package cli

import "testing"

func TestNameMatches(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"", "anything", true},
		{"proj_*", "proj_08_fit", true},
		{"proj_*", "other_proj_job", false},
		{"proj_?8*", "proj_08_fit", true},
		{"*fit", "proj_08_fit", true},
		{"[", "x", false}, // malformed glob matches nothing
	} {
		if got := nameMatches(tc.pattern, tc.name); got != tc.want {
			t.Errorf("nameMatches(%q, %q) = %v", tc.pattern, tc.name, got)
		}
	}
}
