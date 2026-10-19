package slurm

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseJobDetailAndExpand(t *testing.T) {
	line := "JobId=900 JobName=fit_run UserId=alice(1001) GroupId=lab(1001) JobState=RUNNING Reason=None " +
		"RunTime=00:11:17 TimeLimit=04:00:00 StartTime=2026-10-19T19:51:14 EndTime=2026-10-19T23:51:14 " +
		"Partition=batch NodeList=cn3 NumCPUs=4 WorkDir=/home/alice " +
		"StdErr=/home/alice/runs/%x_%j.err StdOut=/home/alice/runs/.slurm_%j.log ExitCode=0:0"
	d := ParseJobDetail(line)
	if d.ID != "900" || d.Name != "fit_run" || d.User != "alice" || d.NumCPUs != 4 || d.TimeLimit != 4*time.Hour {
		t.Fatalf("%+v", d)
	}
	if d.StdOut != "/home/alice/runs/.slurm_900.log" || d.StdErr != "/home/alice/runs/fit_run_900.err" {
		t.Errorf("paths: %q %q", d.StdOut, d.StdErr)
	}
	if got := ExpandOutputPath("a%%b%q", d); got != "a%b%q" {
		t.Errorf("%q", got)
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"04:00:00":   4 * time.Hour,
		"1-00:00:00": 24 * time.Hour,
		"37:40":      37*time.Minute + 40*time.Second,
		"00:00.520":  520 * time.Millisecond,
		"30":         30 * time.Minute,
		"UNLIMITED":  0,
	} {
		if got := ParseDuration(in); got != want {
			t.Errorf("ParseDuration(%q) = %v, want %v", in, got, want)
		}
	}
	if FormatDuration(26*time.Hour+5*time.Second) != "1-02:00:05" {
		t.Error(FormatDuration(26*time.Hour + 5*time.Second))
	}
}

func TestParseAccountingFoldsSteps(t *testing.T) {
	rows := [][]string{
		{"101", "fit_a", "batch", "COMPLETED", "0:0", "00:37:40", "04:00:00", "4", "00:00:00", "", "2026-10-19T19:13:13", "2026-10-19T19:50:53", "/home/alice"},
		{"101.batch", "batch", "", "COMPLETED", "0:0", "00:37:40", "", "4", "02:00:00", "2048K", "", "", ""},
		{"101.0", "python", "", "COMPLETED", "0:0", "00:30:00", "", "4", "01:00:00", "3G", "", "", ""},
		{"102", "fit_b", "batch", "CANCELLED by 1001", "0:15", "00:05:00", "00:20:00", "1", "00:00:00", "", "", "", "/home/alice"},
		{"103", "fit_c", "batch", "RUNNING", "0:0", "00:01:00", "01:00:00", "1", "00:00:00", "", "", "Unknown", "/home/alice"},
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(strings.Join(r, sep) + "\n")
	}
	acc := ParseAccounting(b.String())
	if len(acc) != 3 {
		t.Fatalf("%d jobs", len(acc))
	}
	a := acc[0]
	if a.TotalCPU != 3*time.Hour || a.MaxRSSMB != 3072 || a.Failed() || !a.Final() {
		t.Errorf("job 101: %+v", a)
	}
	// 3 h CPU over 37:40 × 4 CPUs ≈ 1.19 (more than 100% is possible with hyperthreads or bad data)
	if eff := a.CPUEfficiency(); eff < 1.19 || eff > 1.2 {
		t.Errorf("efficiency %.3f", eff)
	}
	if b := acc[1]; b.State != "CANCELLED" || !b.Failed() || b.MaxRSSMB != -1 || b.CPUEfficiency() != -1 {
		t.Errorf("job 102: %+v", b)
	}
	if c := acc[2]; c.Final() {
		t.Error("running job is not final")
	}
}

func TestParseLiveAndProgress(t *testing.T) {
	start := time.Date(2026, 10, 19, 10, 0, 0, 0, time.Local)
	mtime := start.Add(40 * time.Minute)
	now := start.Add(42 * time.Minute)
	out := "##NOW " + itoa64(now.Unix()) + "\n" +
		"##JOB\n" +
		"JobId=900 JobName=fit_run JobState=RUNNING TimeLimit=04:00:00 StartTime=" + start.Format("2006-01-02T15:04:05") +
		" NumCPUs=4 StdOut=/x/.slurm_900.log\n" +
		"##MTIME " + itoa64(mtime.Unix()) + "\n" +
		"##TAIL\n" +
		"catalogue loaded on 10/19\n" +
		"[249/1000] object a (12 s)\n" +
		"[250/1000] object b (11 s)\n" +
		"some trailing log line\n" +
		"##JOB\n" +
		"JobId=901 JobName=other JobState=RUNNING TimeLimit=00:30:00 StartTime=" + start.Format("2006-01-02T15:04:05") + "\n"
	jobs := ParseLive(out)
	if len(jobs) != 2 {
		t.Fatalf("%d jobs", len(jobs))
	}
	j := jobs[0]
	if j.Progress == nil || j.Progress.Done != 250 || j.Progress.Total != 1000 {
		t.Fatalf("progress: %+v", j.Progress)
	}
	if age, ok := j.LastOutputAge(); !ok || age != 2*time.Minute {
		t.Errorf("age %v %v", age, ok)
	}
	// 250 in 40 min → 750 more take 120 min after mtime; 118 min from now; limit ends at 4 h.
	e := j.Estimate()
	if !e.Known || !e.OK || e.Left != 118*time.Minute {
		t.Errorf("estimate %+v", e)
	}
	if got := j.ProgressText(); got != "250/1000 · 1 h 58 min left · limit OK" {
		t.Errorf("%q", got)
	}
	if jobs[1].Progress != nil || jobs[1].ProgressText() != "" {
		t.Error("job without output has no progress")
	}

	// Past the limit: same rate, but a 2-hour limit.
	j.TimeLimit = 2 * time.Hour
	if got := j.ProgressText(); got != "250/1000 · 1 h 58 min left · ETA past limit" {
		t.Errorf("%q", got)
	}
}

func TestParseProgressForms(t *testing.T) {
	for line, want := range map[string]int{
		"##progress 250/3733": 250,
		" 25%|██▌       | 250/1000 [00:10<00:30, 24.5it/s]": 250,
		"[7/10] done":               7,
		"run started 10/19 at noon": -1,
		"ratio 3/4 of objects":      -1,
	} {
		p := ParseProgress([]string{line})
		got := -1
		if p != nil {
			got = p.Done
		}
		if got != want {
			t.Errorf("%q: got %d want %d", line, got, want)
		}
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
