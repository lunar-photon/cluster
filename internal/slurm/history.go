package slurm

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// Suggestion is a time limit derived from a job name's past runs.
type Suggestion struct {
	Limit   time.Duration
	Runs    int           // runs it is based on
	Longest time.Duration // the longest of them
}

// SuggestLimit looks at the last n finished runs of a job name that
// completed or hit their time limit, takes the longest, and adds a margin,
// rounding up to 5 minutes (minimum 5). Taking the longest rather than the
// latest keeps a quick partial rerun from lowering the limit for a full run.
// A run that timed out counts with its limit, the least it would have needed.
func SuggestLimit(history []Accounting, name string, n int, margin float64) (Suggestion, bool) {
	var runs []Accounting
	for _, a := range history {
		if a.Name == name && (a.State == "COMPLETED" || a.State == "TIMEOUT") && a.Elapsed > 0 {
			runs = append(runs, a)
		}
	}
	if len(runs) == 0 {
		return Suggestion{}, false
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].End.After(runs[j].End) })
	if len(runs) > n {
		runs = runs[:n]
	}
	s := Suggestion{Runs: len(runs)}
	for _, r := range runs {
		need := r.Elapsed
		if r.State == "TIMEOUT" && r.TimeLimit > need {
			need = r.TimeLimit
		}
		s.Longest = max(s.Longest, need)
	}
	limit := time.Duration(float64(s.Longest) * margin)
	step := 5 * time.Minute
	limit = (limit + step - 1) / step * step
	s.Limit = max(limit, step)
	return s, true
}

var sinceRE = regexp.MustCompile(`^(\d+)([mhdw])$`)

// SinceArg turns "30m", "6h", "1d" or "2w" into a sacct start time
// ("now-6hours") and the cutoff it means. Anything else (a date such as
// 2026-10-18) is passed to sacct as given.
func SinceArg(s string, now time.Time) (sacct string, cutoff time.Time, err error) {
	if m := sinceRE.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit, d := map[string]struct {
			word string
			d    time.Duration
		}{
			"m": {"minutes", time.Minute}, "h": {"hours", time.Hour},
			"d": {"days", 24 * time.Hour}, "w": {"weeks", 7 * 24 * time.Hour},
		}[m[2]].word, map[string]time.Duration{
			"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour,
		}[m[2]]
		return fmt.Sprintf("now-%d%s", n, unit), now.Add(-time.Duration(n) * d), nil
	}
	for _, layout := range []string{"2006-01-02", "2006-01-02T15:04", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return s, t, nil
		}
	}
	return "", time.Time{}, fmt.Errorf("--since %q: use e.g. 30m, 6h, 1d, 2w or a date like 2026-10-18", s)
}
