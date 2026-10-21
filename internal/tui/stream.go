package tui

import (
	"bufio"
	"bytes"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/lunar-photon/cluster/internal/remote"
)

// outStream follows one job's output file with `tail -F` over the shared ssh
// connection. Lines arrive in batches at most every flushEvery, so a job
// that prints thousands of lines a second can't flood the screen. The remote
// tail exits when stdin closes (stop, or the dashboard exiting), so nothing
// is left running on the login node.
type outStream struct {
	jobID string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	ch    chan []string
	done  chan struct{} // closed by stop, so a replaced stream's sends give up
	once  sync.Once
}

type streamMsg struct {
	s     *outStream
	lines []string
}

type streamEndMsg struct{ s *outStream }

const flushEvery = 250 * time.Millisecond

// startStream begins following path, printing the last n lines first.
// tail polls the file every 2 s, which is all a network filesystem allows
// anyway and costs one stat a poll.
func startStream(r *remote.Client, jobID, path string, n int) (*outStream, error) {
	script := "tail -n " + strconv.Itoa(n) + " -s 2 -F -- " + remote.Quote(path) +
		" 2>/dev/null & t=$!; cat >/dev/null; kill $t 2>/dev/null"
	cmd := r.Command([]string{"-T", "-o", "BatchMode=yes"}, script)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &outStream{jobID: jobID, cmd: cmd, stdin: stdin, ch: make(chan []string, 4), done: make(chan struct{})}
	go s.read(stdout)
	return s, nil
}

// read splits output into lines (tqdm redraws with \r, so that counts as a
// line break too) and hands them over in batches.
func (s *outStream) read(stdout io.Reader) {
	defer close(s.ch)
	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		sc.Split(splitLines)
		for sc.Scan() {
			if t := sc.Text(); len(bytes.TrimSpace([]byte(t))) > 0 {
				select {
				case lines <- t:
				case <-s.done:
					return
				}
			}
		}
	}()
	var batch []string
	send := func() bool {
		if len(batch) == 0 {
			return true
		}
		select {
		case s.ch <- batch:
			batch = nil
			return true
		case <-s.done:
			return false
		}
	}
	tick := time.NewTicker(flushEvery)
	defer tick.Stop()
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				send()
				return
			}
			batch = append(batch, l)
		case <-tick.C:
			if !send() {
				return
			}
		case <-s.done:
			return
		}
	}
}

// splitLines is bufio.ScanLines that also breaks on a lone \r.
func splitLines(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// listen waits for the next batch of lines.
func (s *outStream) listen() tea.Cmd {
	return func() tea.Msg {
		lines, ok := <-s.ch
		if !ok {
			return streamEndMsg{s}
		}
		return streamMsg{s, lines}
	}
}

// stop ends the stream: closing stdin makes the remote side kill tail.
func (s *outStream) stop() {
	s.once.Do(func() {
		close(s.done)
		s.stdin.Close()
		done := make(chan struct{})
		go func() { _ = s.cmd.Wait(); close(done) }()
		go func() {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = s.cmd.Process.Kill()
			}
		}()
	})
}
