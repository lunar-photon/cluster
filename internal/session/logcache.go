package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// LogPath records where a job wrote its output. Slurm forgets this a few
// minutes after the job ends and its accounting doesn't store it, so cluster
// notes it whenever it sees a job alive.
type LogPath struct {
	Path string    `json:"path"`
	Name string    `json:"name,omitempty"`
	Seen time.Time `json:"seen"`
}

// LogCache is a small JSON file of job ID → LogPath, capped in size.
type LogCache struct {
	Path string
	Max  int
}

func DefaultLogCache() *LogCache {
	return &LogCache{Path: filepath.Join(Dir(), "joblogs.json"), Max: 2000}
}

// Remember merges entries into the cache, keeping the most recently seen.
func (c *LogCache) Remember(entries map[string]LogPath) error {
	if len(entries) == 0 {
		return nil
	}
	return c.update(func(m map[string]LogPath) {
		for id, e := range entries {
			if e.Path != "" {
				m[id] = e
			}
		}
		if c.Max > 0 && len(m) > c.Max {
			ids := make([]string, 0, len(m))
			for id := range m {
				ids = append(ids, id)
			}
			sort.Slice(ids, func(i, j int) bool { return m[ids[i]].Seen.After(m[ids[j]].Seen) })
			for _, id := range ids[c.Max:] {
				delete(m, id)
			}
		}
	})
}

// Lookup returns the recorded output path of a job.
func (c *LogCache) Lookup(id string) (LogPath, bool) {
	var e LogPath
	var ok bool
	_ = c.update(func(m map[string]LogPath) { e, ok = m[id] })
	return e, ok
}

func (c *LogCache) update(fn func(map[string]LogPath)) error {
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(c.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	m := map[string]LogPath{}
	data, err := os.ReadFile(c.Path)
	switch {
	case err == nil && len(data) > 0:
		if err := json.Unmarshal(data, &m); err != nil {
			m = map[string]LogPath{} // a corrupt cache is only a cache
		}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	before, _ := json.Marshal(m)
	fn(m)
	after, _ := json.Marshal(m)
	if string(before) == string(after) {
		return nil
	}
	tmp := c.Path + ".tmp"
	if err := os.WriteFile(tmp, after, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.Path)
}
