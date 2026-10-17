package remote

import (
	"os/exec"
	"testing"
)

func TestQuoteRoundTrip(t *testing.T) {
	cases := []string{
		"", "plain", "two words", "it's", `"double"`, "$HOME", "`id`", "a\nb", "semi;colon", "--flag=x y", "'''",
	}
	for _, s := range cases {
		out, err := exec.Command("sh", "-c", "printf %s "+Quote(s)).Output()
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if string(out) != s {
			t.Errorf("Quote(%q) round-tripped to %q", s, out)
		}
	}
}

func TestJoinKeepsArgv(t *testing.T) {
	line := Join("printf", "[%s]", "a b", "c'd", "")
	out, err := exec.Command("sh", "-c", line).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "[a b][c'd][]"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
