package slurm

import (
	"testing"
	"time"
)

func TestSuggestLimit(t *testing.T) {
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)
	run := func(i int, state string, elapsed, limit time.Duration) Accounting {
		return Accounting{ID: "1", Name: "fit", State: state, Elapsed: elapsed, TimeLimit: limit, End: day.Add(time.Duration(i) * time.Hour)}
	}
	history := []Accounting{
		run(1, "COMPLETED", 18*time.Minute, time.Hour),
		run(2, "COMPLETED", 12*time.Minute, time.Hour),
		run(3, "FAILED", 2*time.Hour, 3*time.Hour),         // failures don't count
		run(4, "COMPLETED", 1*time.Minute, 30*time.Minute), // the quick redraw run
		{Name: "other", State: "COMPLETED", Elapsed: 5 * time.Hour, End: day},
	}
	s, ok := SuggestLimit(history, "fit", 10, 1.5)
	// Longest is 18 min, not the latest 1 min; ×1.5 = 27 min → 30 min.
	if !ok || s.Runs != 3 || s.Longest != 18*time.Minute || s.Limit != 30*time.Minute {
		t.Errorf("%+v %v", s, ok)
	}
	// A timeout counts with its limit.
	history = append(history, run(5, "TIMEOUT", 40*time.Minute, 40*time.Minute))
	if s, _ := SuggestLimit(history, "fit", 10, 1.5); s.Longest != 40*time.Minute || s.Limit != time.Hour {
		t.Errorf("with timeout: %+v", s)
	}
	// Only the last n runs count.
	if s, _ := SuggestLimit(history, "fit", 2, 1.5); s.Runs != 2 || s.Longest != 40*time.Minute {
		t.Errorf("last 2: %+v", s)
	}
	if _, ok := SuggestLimit(history, "unknown", 10, 1.5); ok {
		t.Error("no history, no suggestion")
	}
	// Tiny jobs still get at least 5 minutes.
	if s, _ := SuggestLimit([]Accounting{{Name: "x", State: "COMPLETED", Elapsed: 10 * time.Second}}, "x", 10, 1.5); s.Limit != 5*time.Minute {
		t.Errorf("minimum: %+v", s)
	}
}

func TestSinceArg(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	for in, want := range map[string]string{"1d": "now-1days", "6h": "now-6hours", "30m": "now-30minutes", "2w": "now-2weeks"} {
		got, cutoff, err := SinceArg(in, now)
		if err != nil || got != want || !cutoff.Before(now) {
			t.Errorf("%s: %q %v %v", in, got, cutoff, err)
		}
	}
	if got, cutoff, err := SinceArg("2026-10-18", now); err != nil || got != "2026-10-18" || cutoff.Day() != 18 {
		t.Errorf("date: %q %v %v", got, cutoff, err)
	}
	if _, _, err := SinceArg("yesterday", now); err == nil {
		t.Error("bad value should fail")
	}
}
