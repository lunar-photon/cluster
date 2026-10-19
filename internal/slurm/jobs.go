package slurm

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lunar-photon/cluster/internal/remote"
)

// JobDetail is what `scontrol show job` knows about a job. Slurm keeps it
// only while the job is queued or running, plus MinJobAge (often 5 minutes).
type JobDetail struct {
	ID, Name, User, State, Reason string
	StdOut, StdErr, WorkDir       string
	TimeLimit                     time.Duration // 0 when unlimited
	RunTime                       time.Duration
	Start, End                    time.Time
	NumCPUs                       int
	ExitCode                      string
	Partition, NodeList           string
}

// JobDetail returns the job's details, or found=false once Slurm has
// forgotten it.
func (c *Client) JobDetail(ctx context.Context, id string) (JobDetail, bool, error) {
	out, err := c.R.Run(ctx, "scontrol", "show", "job", "-o", id)
	if err != nil {
		if strings.Contains(err.Error(), "Invalid job id") {
			return JobDetail{}, false, nil
		}
		return JobDetail{}, false, err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	d := ParseJobDetail(line)
	return d, d.ID != "", nil
}

// ParseJobDetail parses one `scontrol show job -o` line.
func ParseJobDetail(line string) JobDetail {
	kv := keyValues(line)
	d := JobDetail{
		ID:        kv["JobId"],
		Name:      kv["JobName"],
		State:     kv["JobState"],
		Reason:    kv["Reason"],
		WorkDir:   kv["WorkDir"],
		TimeLimit: ParseDuration(kv["TimeLimit"]),
		RunTime:   ParseDuration(kv["RunTime"]),
		Start:     parseSlurmTime(kv["StartTime"]),
		End:       parseSlurmTime(kv["EndTime"]),
		NumCPUs:   atoi(kv["NumCPUs"]),
		ExitCode:  kv["ExitCode"],
		Partition: kv["Partition"],
		NodeList:  kv["NodeList"],
	}
	if u := kv["UserId"]; u != "" {
		d.User, _, _ = strings.Cut(u, "(")
	}
	d.StdOut = ExpandOutputPath(kv["StdOut"], d)
	d.StdErr = ExpandOutputPath(kv["StdErr"], d)
	return d
}

// ExpandOutputPath fills in the sbatch filename patterns some Slurm versions
// leave unexpanded: %j/%J job ID, %x job name, %u user, %% a literal %.
func ExpandOutputPath(p string, d JobDetail) string {
	if !strings.Contains(p, "%") {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] != '%' || i+1 == len(p) {
			b.WriteByte(p[i])
			continue
		}
		i++
		switch p[i] {
		case 'j', 'J', 'A':
			b.WriteString(d.ID)
		case 'x':
			b.WriteString(d.Name)
		case 'u':
			b.WriteString(d.User)
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(p[i])
		}
	}
	return b.String()
}

// ParseDuration parses Slurm durations: [D-]HH:MM:SS, MM:SS, MM:SS.mmm, or
// minutes. UNLIMITED, NONE and empty give 0.
func ParseDuration(s string) time.Duration {
	s = strings.TrimSpace(s)
	switch s {
	case "", "UNLIMITED", "NONE", "INVALID", "Partition_Limit", "N/A":
		return 0
	}
	days := 0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		days = atoi(d)
		s = rest
	}
	frac := 0.0
	if whole, f, ok := strings.Cut(s, "."); ok {
		frac, _ = strconv.ParseFloat("0."+f, 64)
		s = whole
	}
	parts := strings.Split(s, ":")
	var h, m, sec int
	switch len(parts) {
	case 1:
		m = atoi(parts[0])
	case 2:
		m, sec = atoi(parts[0]), atoi(parts[1])
	case 3:
		h, m, sec = atoi(parts[0]), atoi(parts[1]), atoi(parts[2])
	}
	return time.Duration(days)*24*time.Hour + time.Duration(h)*time.Hour + time.Duration(m)*time.Minute +
		time.Duration(sec)*time.Second + time.Duration(frac*float64(time.Second))
}

// FormatDuration renders a duration the way Slurm does: [D-]HH:MM:SS.
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	days := int(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	s := fmt.Sprintf("%02d:%02d:%02d", int(d/time.Hour), int(d%time.Hour/time.Minute), int(d%time.Minute/time.Second))
	if days > 0 {
		return fmt.Sprintf("%d-%s", days, s)
	}
	return s
}

// FormatLimit is FormatDuration, with 0 meaning no limit.
func FormatLimit(d time.Duration) string {
	if d <= 0 {
		return "UNLIMITED"
	}
	return FormatDuration(d)
}

// HumanDuration is a short, rounded duration: "41 min", "2 h 5 min", "3 s".
func HumanDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Round(time.Minute).Minutes()))
	case d < 48*time.Hour:
		h := int(d / time.Hour)
		m := int((d % time.Hour).Round(time.Minute).Minutes())
		if m == 60 {
			h, m = h+1, 0
		}
		if m == 0 {
			return fmt.Sprintf("%d h", h)
		}
		return fmt.Sprintf("%d h %d min", h, m)
	}
	return fmt.Sprintf("%.1f days", d.Hours()/24)
}

func parseSlurmTime(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02T15:04:05", s, time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ---- accounting (sacct) ----

// Accounting is one finished or running job as `sacct` records it, with its
// steps' CPU time and peak memory folded in.
type Accounting struct {
	ID, Name, Partition string
	State               string // first word: CANCELLED, not "CANCELLED by 123"
	ExitCode            string // "exit:signal", e.g. "0:0", "1:0", "0:9"
	Elapsed, TimeLimit  time.Duration
	AllocCPUs           int
	TotalCPU            time.Duration
	MaxRSSMB            int // -1 when the cluster doesn't record it
	Start, End          time.Time
	WorkDir             string
}

// Failed reports whether the job ended in any way other than success.
func (a Accounting) Failed() bool {
	return a.State != "COMPLETED" || (a.ExitCode != "" && a.ExitCode != "0:0")
}

// Final reports whether the job has ended.
func (a Accounting) Final() bool { return !IsActive(a.State) }

// CPUEfficiency is TotalCPU / (Elapsed × AllocCPUs), or -1 when unknown.
func (a Accounting) CPUEfficiency() float64 {
	if a.Elapsed <= 0 || a.AllocCPUs <= 0 || a.TotalCPU <= 0 {
		return -1
	}
	return a.TotalCPU.Seconds() / (a.Elapsed.Seconds() * float64(a.AllocCPUs))
}

var sacctFields = "JobID,JobName,Partition,State,ExitCode,Elapsed,Timelimit,AllocCPUS,TotalCPU,MaxRSS,Start,End,WorkDir"

// Accounting queries sacct for the given job IDs, or, with no IDs, for the
// user's jobs since `since` (a sacct time such as "now-1days").
func (c *Client) Accounting(ctx context.Context, ids []string, since string) ([]Accounting, error) {
	argv := []string{"sacct", "--noheader", "--parsable2", "--delimiter=" + sep, "--format=" + sacctFields}
	if len(ids) > 0 {
		argv = append(argv, "--jobs="+strings.Join(ids, ","))
	} else {
		argv = append(argv, "--starttime="+since)
	}
	// $USER must expand on the cluster, so it can't go through Join's quoting.
	cmd := remote.Join(argv...)
	if len(ids) == 0 {
		cmd += ` --user="$USER"`
	}
	out, err := c.R.Output(ctx, cmd, nil)
	if err != nil {
		return nil, err
	}
	return ParseAccounting(out), nil
}

// ParseAccounting folds step lines (123.batch, 123.0) into their job.
func ParseAccounting(out string) []Accounting {
	var order []string
	byID := map[string]*Accounting{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, sep)
		if len(f) < 13 {
			continue
		}
		id, step, isStep := strings.Cut(f[0], ".")
		_ = step
		a := byID[id]
		if a == nil {
			a = &Accounting{ID: id, MaxRSSMB: -1}
			byID[id] = a
			order = append(order, id)
		}
		a.TotalCPU += ParseDuration(f[8])
		if rss := memMBOrUnknown(f[9]); rss > a.MaxRSSMB {
			a.MaxRSSMB = rss
		}
		if isStep {
			continue
		}
		state, _, _ := strings.Cut(f[3], " ")
		a.Name, a.Partition, a.State, a.ExitCode = f[1], f[2], state, f[4]
		a.Elapsed, a.TimeLimit = ParseDuration(f[5]), ParseDuration(f[6])
		a.AllocCPUs = atoi(f[7])
		a.Start, a.End = parseSlurmTime(f[10]), parseSlurmTime(f[11])
		a.WorkDir = f[12]
	}
	out2 := make([]Accounting, 0, len(order))
	for _, id := range order {
		if a := byID[id]; a.State != "" {
			out2 = append(out2, *a)
		}
	}
	return out2
}

func memMBOrUnknown(v string) int {
	if strings.TrimSpace(v) == "" {
		return -1
	}
	return memMB(v)
}

// ---- live output of running jobs ----

// Live is a running job together with what its output file shows.
type Live struct {
	JobDetail
	MTime    time.Time // last write to the output file; zero if unknown
	Now      time.Time // the cluster's clock when this was read
	Tail     []string
	Progress *Progress
}

// LastOutputAge is how long ago the job last wrote output.
func (l Live) LastOutputAge() (time.Duration, bool) {
	if l.MTime.IsZero() || l.Now.IsZero() {
		return 0, false
	}
	return max(0, l.Now.Sub(l.MTime)), true
}

// liveScript prints, for each given job (or all the user's jobs), its
// scontrol line, the output file's modification time and its last lines.
// tqdm-style output uses \r, so carriage returns become line breaks.
const liveScript = `ids="$*"
[ -n "$ids" ] || ids=$(squeue -h -u "$USER" -t R,CG -o %i)
echo "##NOW $(date +%s)"
for j in $ids; do
  line=$(scontrol show job -o "$j" 2>/dev/null) || continue
  echo "##JOB"; echo "$line"
  f=$(printf '%s\n' "$line" | sed -n 's/.* StdOut=\([^ ]*\).*/\1/p')
  case "$f" in *%*) f= ;; esac
  if [ -n "$f" ] && [ -r "$f" ]; then
    echo "##MTIME $(stat -c %Y "$f")"
    echo "##TAIL"; tail -c 16384 "$f" | tr '\r' '\n' | grep -v '^[[:space:]]*$' | tail -n 30
  fi
done
true`

// LiveJobs reads the output state of the given running jobs (all of the
// user's running jobs when ids is empty) in one round trip.
func (c *Client) LiveJobs(ctx context.Context, ids ...string) ([]Live, error) {
	cmd := "bash -s -- " + remote.Join(ids...)
	if len(ids) == 0 {
		cmd = "bash -s"
	}
	out, err := c.R.Output(ctx, cmd, strings.NewReader(liveScript))
	if err != nil {
		return nil, err
	}
	return ParseLive(out), nil
}

// ParseLive parses liveScript output.
func ParseLive(out string) []Live {
	var now time.Time
	var jobs []Live
	var cur *Live
	inTail := false
	expectJob := false
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "##NOW "):
			now = time.Unix(int64(atoi(strings.TrimPrefix(line, "##NOW "))), 0)
			continue
		case line == "##JOB":
			jobs = append(jobs, Live{Now: now})
			cur, inTail, expectJob = &jobs[len(jobs)-1], false, true
			continue
		case cur == nil:
			continue
		case expectJob:
			cur.JobDetail = ParseJobDetail(line)
			expectJob = false
			continue
		case strings.HasPrefix(line, "##MTIME "):
			cur.MTime = time.Unix(int64(atoi(strings.TrimPrefix(line, "##MTIME "))), 0)
			continue
		case line == "##TAIL":
			inTail = true
			continue
		}
		if inTail && strings.TrimSpace(line) != "" {
			cur.Tail = append(cur.Tail, line)
		}
	}
	for i := range jobs {
		jobs[i].Progress = ParseProgress(jobs[i].Tail)
	}
	return jobs
}

// ---- progress ----

// Progress is the latest "done/total" a job printed.
type Progress struct {
	Done, Total int
	Line        string
}

var progressPatterns = []*regexp.Regexp{
	regexp.MustCompile(`##progress\s+(\d+)\s*/\s*(\d+)`), // the explicit convention
	regexp.MustCompile(`^\s*\[(\d+)/(\d+)\]`),            // "[250/3733] ..."
	regexp.MustCompile(`\b(\d+)/(\d+) \[`),               // tqdm: " 25%|██| 250/1000 [00:10<..."
}

// ParseProgress finds the most recent progress line. Only these forms
// count, so dates like 10/19 are never mistaken for progress.
func ParseProgress(lines []string) *Progress {
	for i := len(lines) - 1; i >= 0; i-- {
		for _, re := range progressPatterns {
			if m := re.FindStringSubmatch(lines[i]); m != nil {
				done, total := atoi(m[1]), atoi(m[2])
				if total > 0 && done <= total {
					return &Progress{Done: done, Total: total, Line: strings.TrimSpace(lines[i])}
				}
			}
		}
	}
	return nil
}

// Estimate projects when a job will finish from its progress, assuming a
// steady rate since it started. Its OK field compares the finish time with
// the end of the time limit.
type Estimate struct {
	Rate     float64 // items per minute
	Left     time.Duration
	Finish   time.Time
	LimitEnd time.Time // zero when unlimited
	OK       bool
	Known    bool // false until at least one item is done
}

func (l Live) Estimate() Estimate {
	var e Estimate
	if l.TimeLimit > 0 && !l.Start.IsZero() {
		e.LimitEnd = l.Start.Add(l.TimeLimit)
	}
	p := l.Progress
	if p == nil || p.Done <= 0 || l.Start.IsZero() || l.MTime.IsZero() {
		return e
	}
	took := l.MTime.Sub(l.Start)
	if took <= 0 {
		return e
	}
	perItem := took / time.Duration(p.Done)
	e.Known = true
	e.Rate = float64(p.Done) / took.Minutes()
	e.Finish = l.MTime.Add(perItem * time.Duration(p.Total-p.Done))
	e.Left = e.Finish.Sub(l.Now)
	e.OK = e.LimitEnd.IsZero() || !e.Finish.After(e.LimitEnd)
	return e
}

// ProgressText is the one-line summary for status, e.g.
// "250/3733 · 41 min left · limit OK".
func (l Live) ProgressText() string {
	p := l.Progress
	if p == nil {
		return ""
	}
	text := fmt.Sprintf("%d/%d", p.Done, p.Total)
	e := l.Estimate()
	switch {
	case p.Done == p.Total:
		return text + " · done"
	case !e.Known:
		return text + " · no rate yet"
	case e.Left > 0:
		text += " · " + HumanDuration(e.Left) + " left"
	default:
		text += " · due now"
	}
	if e.OK {
		return text + " · limit OK"
	}
	return text + " · ETA past limit"
}
