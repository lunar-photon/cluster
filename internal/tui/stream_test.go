package tui

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lunar-photon/cluster/internal/remote"
	"github.com/lunar-photon/cluster/internal/slurm"
)

func TestStreamDeliversLinesAndStopsRemoteTail(t *testing.T) {
	dir := t.TempDir()
	// ssh runs the remote command locally; tail prints, records its PID, and
	// then waits like `tail -F` on a quiet file.
	os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/usr/bin/env bash\nexec bash -c \"${@: -1}\"\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "tail"), []byte("#!/usr/bin/env bash\necho $$ > \""+dir+"/tail.pid\"\n"+
		"printf 'step 1\\nstep 2\\n 50%%|##   | 1/2 [\\r100%%|####| 2/2 [\\n'\nexec sleep 60\n"), 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	r := remote.New("fakehost")
	r.SSH, r.ControlPath = filepath.Join(dir, "ssh"), ""

	s, err := startStream(r, "42", "/x/out.log", 30)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	deadline := time.After(10 * time.Second)
	for len(got) < 4 {
		select {
		case lines, ok := <-s.ch:
			if !ok {
				t.Fatalf("stream ended early with %q", got)
			}
			got = append(got, lines...)
		case <-deadline:
			t.Fatalf("timed out with %q", got)
		}
	}
	want := []string{"step 1", "step 2", " 50%|##   | 1/2 [", "100%|####| 2/2 ["}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q want %q", got, want)
	}
	if p := slurm.ParseProgress(got); p == nil || p.Done != 2 {
		t.Errorf("progress from streamed tqdm lines: %+v", p)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "tail.pid"))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	s.stop()
	for i := 0; i < 50; i++ {
		if syscall.Kill(pid, 0) != nil {
			return // the remote tail is gone
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("tail kept running after stop")
}

func TestTimeCell(t *testing.T) {
	start := time.Date(2026, 10, 21, 10, 0, 0, 0, time.Local)
	run := slurm.Job{State: "RUNNING", Time: "?", TimeLimit: "1:00:00"}
	l := slurm.Live{JobDetail: slurm.JobDetail{Start: start, TimeLimit: time.Hour}}

	if got, near := timeCell(run, l, true, start.Add(24*time.Minute+13*time.Second)); got != "00:24:13 / 01:00:00 · 00:35:47 left" || near {
		t.Errorf("%q %v", got, near)
	}
	if _, near := timeCell(run, l, true, start.Add(55*time.Minute)); !near {
		t.Error("last 10 minutes should be marked")
	}
	l.TimeLimit = 0
	if got, _ := timeCell(run, l, true, start.Add(90*time.Second)); got != "00:01:30 / no limit" {
		t.Errorf("%q", got)
	}
	pend := slurm.Job{State: "PENDING", TimeLimit: "4:00:00", Submit: start}
	if got, _ := timeCell(pend, slurm.Live{}, false, start.Add(12*time.Minute)); got != "waiting 00:12:00 · limit 4:00:00" {
		t.Errorf("%q", got)
	}
	// No live data yet: what squeue said.
	if got, _ := timeCell(run, slurm.Live{}, false, start); got != "? / 1:00:00" {
		t.Errorf("%q", got)
	}
}
