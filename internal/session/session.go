// Package session records the Slurm jobs, mounts and tunnels this tool
// creates, so they can be reattached, listed and cleaned up even after the
// terminal that started them is gone.
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

type Kind string

const (
	Terminal      Kind = "terminal"       // node terminal
	TerminalMount Kind = "terminal+mount" // node terminal + cluster mount
	Jupyter       Kind = "jupyter"
)

type Session struct {
	JobID   string    `json:"job_id"`
	Kind    Kind      `json:"kind"`
	Host    string    `json:"host"`
	Profile string    `json:"profile,omitempty"`
	Created time.Time `json:"created"`
	// Mount is the local mount point this session mounted, if any.
	Mount string `json:"mount,omitempty"`
	// Node and RemotePort locate a Jupyter server on its compute node.
	Node       string `json:"node,omitempty"`
	RemotePort int    `json:"remote_port,omitempty"`
	Token      string `json:"token,omitempty"`
	// LocalPort and URL describe the local end of a Jupyter tunnel.
	LocalPort int    `json:"local_port,omitempty"`
	URL       string `json:"url,omitempty"`
	// TunnelPID is the local ssh -L process, if any.
	TunnelPID int `json:"tunnel_pid,omitempty"`
	// RemoteFiles are deleted on the cluster when the session is stopped.
	RemoteFiles []string `json:"remote_files,omitempty"`
}

// Store is a JSON file guarded by an advisory lock.
type Store struct {
	Path string
}

func stateHome() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return x
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state")
}

// Dir is the directory holding state and logs.
func Dir() string { return filepath.Join(stateHome(), "cluster") }

func DefaultStore() *Store { return &Store{Path: filepath.Join(Dir(), "sessions.json")} }

// Update loads the sessions, lets fn modify them, and writes them back, all
// under an exclusive lock so concurrent `cluster` processes don't clobber
// each other.
func (s *Store) Update(fn func([]Session) []Session) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	list, err := s.read()
	if err != nil {
		return err
	}
	return s.write(fn(list))
}

// List returns all recorded sessions, newest first.
func (s *Store) List() ([]Session, error) {
	var out []Session
	err := s.Update(func(l []Session) []Session { out = l; return l })
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, err
}

func (s *Store) Add(sess Session) error {
	return s.Update(func(l []Session) []Session {
		return append(Remove(l, sess.JobID), sess)
	})
}

func (s *Store) Delete(jobID string) error {
	return s.Update(func(l []Session) []Session { return Remove(l, jobID) })
}

func (s *Store) Get(jobID string) (Session, bool, error) {
	list, err := s.List()
	for _, x := range list {
		if x.JobID == jobID {
			return x, true, err
		}
	}
	return Session{}, false, err
}

// Remove returns l without the session for jobID.
func Remove(l []Session, jobID string) []Session {
	out := l[:0:0]
	for _, x := range l {
		if x.JobID != jobID {
			out = append(out, x)
		}
	}
	return out
}

// MountInUse reports whether a session other than exceptJobID uses mount.
func MountInUse(l []Session, mount, exceptJobID string) bool {
	for _, x := range l {
		if x.Mount == mount && x.JobID != exceptJobID {
			return true
		}
	}
	return false
}

func (s *Store) read() ([]Session, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Session
	if len(data) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *Store) write(list []Session) error {
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}
