package mount

import (
	"strings"
	"testing"
)

func TestContainsTarget(t *testing.T) {
	mounts := `sysfs /sys sysfs rw 0 0
host:/home/me /home/me/My\040Cluster fuse.sshfs rw,nosuid 0 0
host: /home/me/Cluster fuse.sshfs rw 0 0
`
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/home/me/My Cluster", true},
		{"/home/me/Cluster", true},
		{"/home/me/Cluster/", true},
		{"/home/me/Cluster/../Cluster", true},
		{"/home/me/Other", false},
		{"/home/me/My", false},
	} {
		if got := containsTarget(strings.NewReader(mounts), normalize(tc.path)); got != tc.want {
			t.Errorf("%q: got %v want %v", tc.path, got, tc.want)
		}
	}
}

func TestUnescape(t *testing.T) {
	if got := unescape(`a\040b\134c\011d`); got != "a b\\c\td" {
		t.Errorf("got %q", got)
	}
	if got := unescape(`trailing\04`); got != `trailing\04` {
		t.Errorf("short escape should be kept: %q", got)
	}
}
