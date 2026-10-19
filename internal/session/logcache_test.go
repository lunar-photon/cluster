package session

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestLogCacheRememberLookupAndCap(t *testing.T) {
	c := &LogCache{Path: filepath.Join(t.TempDir(), "joblogs.json"), Max: 3}
	base := time.Now()
	for i := 0; i < 5; i++ {
		id := strconv.Itoa(100 + i)
		if err := c.Remember(map[string]LogPath{id: {Path: "/out/" + id, Seen: base.Add(time.Duration(i) * time.Minute)}}); err != nil {
			t.Fatal(err)
		}
	}
	if e, ok := c.Lookup("104"); !ok || e.Path != "/out/104" {
		t.Errorf("newest entry: %+v %v", e, ok)
	}
	if _, ok := c.Lookup("100"); ok {
		t.Error("oldest entries should be dropped past Max")
	}
	if err := c.Remember(map[string]LogPath{"200": {Path: ""}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Lookup("200"); ok {
		t.Error("empty paths are not stored")
	}
}
