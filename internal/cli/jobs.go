package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/lunar-photon/cluster/internal/lifecycle"
	"github.com/lunar-photon/cluster/internal/remote"
	"github.com/lunar-photon/cluster/internal/session"
	"github.com/lunar-photon/cluster/internal/slurm"
)

// errJobFailed makes a command exit non-zero after it has printed why.
var errJobFailed = errors.New("job did not complete successfully")

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// ---- JSON shapes (stable field names) ----

type progressJSON struct {
	Done     int    `json:"done"`
	Total    int    `json:"total"`
	LeftS    *int64 `json:"left_s,omitempty"`
	Finish   string `json:"finish,omitempty"`
	LimitOK  *bool  `json:"limit_ok,omitempty"`
	LastLine string `json:"line"`
}

type outputJSON struct {
	Path        string        `json:"path,omitempty"`
	LastWriteS  *int64        `json:"last_write_ago_s,omitempty"`
	Progress    *progressJSON `json:"progress,omitempty"`
	LastLine    string        `json:"last_line,omitempty"`
	RecentLines []string      `json:"recent_lines,omitempty"`
}

func liveJSON(l slurm.Live, lines int) *outputJSON {
	o := &outputJSON{Path: l.StdOut}
	if age, ok := l.LastOutputAge(); ok {
		s := int64(age.Seconds())
		o.LastWriteS = &s
	}
	if n := len(l.Tail); n > 0 {
		o.LastLine = l.Tail[n-1]
		if lines > 0 {
			o.RecentLines = l.Tail[max(0, n-lines):]
		}
	}
	if p := l.Progress; p != nil {
		pj := &progressJSON{Done: p.Done, Total: p.Total, LastLine: p.Line}
		if e := l.Estimate(); e.Known {
			left := int64(e.Left.Seconds())
			pj.LeftS, pj.Finish, pj.LimitOK = &left, e.Finish.Format(time.RFC3339), &e.OK
		}
		o.Progress = pj
	}
	return o
}

type jobJSON struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Partition string          `json:"partition"`
	State     string          `json:"state"`
	Time      string          `json:"time"`
	TimeLimit string          `json:"time_limit"`
	Nodes     string          `json:"nodes"`
	Where     string          `json:"nodelist_or_reason"`
	CPUs      string          `json:"cpus"`
	Mem       string          `json:"mem"`
	GPUs      int             `json:"gpus"`
	ByCluster bool            `json:"submitted_by_cluster"`
	Output    *outputJSON     `json:"output,omitempty"`
	Suggested *suggestionJSON `json:"suggested_time_limit,omitempty"`
}

type suggestionJSON struct {
	LimitS     int64 `json:"limit_s"`
	Runs       int   `json:"based_on_runs"`
	LongestS   int64 `json:"longest_run_s"`
	LimitShort bool  `json:"current_limit_shorter"`
}

type finishedJSON struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Partition     string   `json:"partition,omitempty"`
	State         string   `json:"state"`
	ExitCode      string   `json:"exit_code"`
	ElapsedS      int64    `json:"elapsed_s"`
	TimeLimitS    int64    `json:"time_limit_s"`
	CPUs          int      `json:"cpus"`
	CPUEfficiency *float64 `json:"cpu_efficiency,omitempty"`
	MaxRSSMB      *int     `json:"max_rss_mb,omitempty"`
	Start         string   `json:"start,omitempty"`
	End           string   `json:"end,omitempty"`
	Failed        bool     `json:"failed"`
}

func accountingJSON(a slurm.Accounting) finishedJSON {
	f := finishedJSON{ID: a.ID, Name: a.Name, Partition: a.Partition, State: a.State, ExitCode: a.ExitCode,
		ElapsedS: int64(a.Elapsed.Seconds()), TimeLimitS: int64(a.TimeLimit.Seconds()), CPUs: a.AllocCPUs, Failed: a.Failed()}
	if eff := a.CPUEfficiency(); eff >= 0 {
		f.CPUEfficiency = &eff
	}
	if a.MaxRSSMB >= 0 {
		f.MaxRSSMB = &a.MaxRSSMB
	}
	if !a.Start.IsZero() {
		f.Start = a.Start.Format(time.RFC3339)
	}
	if !a.End.IsZero() {
		f.End = a.End.Format(time.RFC3339)
	}
	return f
}

// ---- output paths ----

// rememberPaths records where live jobs write, for after Slurm forgets.
func rememberPaths(live []slurm.Live) {
	m := map[string]session.LogPath{}
	for _, l := range live {
		if l.StdOut != "" {
			m[l.ID] = session.LogPath{Path: l.StdOut, Name: l.Name, Seen: time.Now()}
		}
	}
	_ = session.DefaultLogCache().Remember(m)
}

// outputPath finds a job's output file: from Slurm while it knows the job,
// then from cluster's own record, then Slurm's default name in the job's
// working directory.
func outputPath(ctx context.Context, e *lifecycle.Env, id string) (string, error) {
	if d, found, err := e.Slurm.JobDetail(ctx, id); err != nil {
		return "", err
	} else if found && d.StdOut != "" {
		_ = session.DefaultLogCache().Remember(map[string]session.LogPath{id: {Path: d.StdOut, Name: d.Name, Seen: time.Now()}})
		return d.StdOut, nil
	}
	if lp, ok := session.DefaultLogCache().Lookup(id); ok {
		return lp.Path, nil
	}
	acc, err := e.Slurm.Accounting(ctx, []string{id}, "")
	if err != nil {
		return "", err
	}
	if len(acc) == 0 {
		return "", fmt.Errorf("Slurm has no record of job %s", id)
	}
	if wd := acc[0].WorkDir; wd != "" {
		guess := strings.TrimRight(wd, "/") + "/slurm-" + id + ".out"
		if _, err := e.R.Run(ctx, "test", "-r", guess); err == nil {
			return guess, nil
		}
	}
	return "", fmt.Errorf("Slurm no longer says where job %s wrote its output (it forgets a few minutes after a job ends, and cluster hadn't seen it running); pass --path FILE", id)
}

// latestJob picks a job for `logs` without an ID: the newest active job
// matching the name glob, else the newest such job from the last week.
func latestJob(ctx context.Context, e *lifecycle.Env, pattern string) (string, error) {
	jobs, err := e.Slurm.Jobs(ctx)
	if err != nil {
		return "", err
	}
	best := ""
	for _, j := range filterJobs(jobs, pattern) {
		if best == "" || jobNum(j.ID) > jobNum(best) {
			best = j.ID
		}
	}
	if best != "" {
		return best, nil
	}
	acc, err := e.Slurm.Accounting(ctx, nil, "now-7days")
	if err != nil {
		return "", err
	}
	for _, a := range acc {
		if nameMatches(pattern, a.Name) && (best == "" || jobNum(a.ID) > jobNum(best)) {
			best = a.ID
		}
	}
	if best == "" {
		return "", fmt.Errorf("no jobs found; give a job ID")
	}
	return best, nil
}

func jobNum(id string) int {
	n, _ := strconv.Atoi(strings.SplitN(strings.SplitN(id, "_", 2)[0], ".", 2)[0])
	return n
}

// ---- logs ----

func logsCmd(g *globals) *cobra.Command {
	var follow, asJSON bool
	var lines int
	var name, path string
	cmd := &cobra.Command{
		Use:   "logs [JOBID]",
		Short: "Show or follow the output of a job, however it was submitted",
		Long: `Show the last lines a job printed, with its state, progress and how long
ago it last wrote. -f follows the output until the job ends, then prints how
it ended and exits non-zero if it failed.

Works for any of your jobs, including ones submitted with sbatch by other
tools. Without a JOBID it picks your newest job (matching --name, if given).

Slurm forgets where a job wrote its output a few minutes after it ends.
cluster remembers the path whenever it sees a job running; otherwise give
--path.

Progress: lines like "##progress 250/3733", "[250/3733] ..." or tqdm's
"250/3733 [..." are read to show progress and an estimated finish time.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.env()
			if err != nil {
				return err
			}
			if err := e.R.EnsureMaster(); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			id := ""
			if len(args) == 1 {
				id = args[0]
			} else if id, err = latestJob(ctx, e, name); err != nil {
				return err
			}
			if path == "" {
				if path, err = outputPath(ctx, e, id); err != nil {
					return err
				}
			}
			return runLogs(e, id, path, lines, follow, asJSON)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new output until the job ends")
	cmd.Flags().IntVarP(&lines, "lines", "l", 40, "number of recent lines to show")
	cmd.Flags().StringVar(&path, "path", "", "read this output file instead of looking it up")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output (with -f: one JSON object per line)")
	nameFlag(cmd, &name)
	return cmd
}

func runLogs(e *lifecycle.Env, id, path string, n int, follow, asJSON bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	live, _ := e.Slurm.LiveJobs(ctx, id)
	cancel()
	var l *slurm.Live
	if len(live) == 1 {
		l = &live[0]
		rememberPaths(live)
	}

	if !follow {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		out, err := e.R.Run(ctx, "tail", "-n", strconv.Itoa(n), "--", path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		text := strings.TrimRight(out, "\n")
		if asJSON {
			obj := map[string]any{"id": id, "path": path, "lines": splitLines(text)}
			if l != nil {
				obj["name"], obj["state"] = l.Name, l.State
				obj["output"] = liveJSON(*l, 0)
			} else if acc := finalState(e, id); acc != nil {
				obj["name"], obj["state"], obj["finished"] = acc.Name, acc.State, accountingJSON(*acc)
			}
			return printJSON(obj)
		}
		logsHeader(os.Stderr, id, path, l)
		if text != "" {
			fmt.Println(text)
		}
		if l == nil {
			if acc := finalState(e, id); acc != nil {
				fmt.Fprintln(os.Stderr, endedText(*acc))
			}
		}
		return nil
	}

	if !asJSON {
		logsHeader(os.Stderr, id, path, l)
	}
	out := io.Writer(os.Stdout)
	var lw *jsonLineWriter
	if asJSON {
		lw = &jsonLineWriter{}
		out = lw
	}
	interrupted, err := followOutput(e, id, path, n, out)
	if lw != nil {
		lw.Flush()
	}
	if err != nil {
		return err
	}
	if interrupted {
		if !asJSON {
			fmt.Fprintf(os.Stderr, "\nStopped following; job %s keeps running (cluster logs -f %s to resume).\n", id, id)
		}
		return nil
	}
	acc := finalState(e, id)
	if acc == nil {
		return nil
	}
	if asJSON {
		_ = jsonLine(map[string]any{"type": "end", "job": accountingJSON(*acc)})
	} else {
		fmt.Fprintln(os.Stderr, endedText(*acc))
	}
	if acc.Failed() {
		return errJobFailed
	}
	return nil
}

// jsonLine writes one compact JSON object per line (for streaming).
func jsonLine(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func splitLines(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(s, "\n")
}

// jsonLineWriter turns streamed output into {"type":"line","text":...} objects.
type jsonLineWriter struct{ buf []byte }

func (w *jsonLineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexAny(string(w.buf), "\n\r")
		if i < 0 {
			break
		}
		if line := string(w.buf[:i]); strings.TrimSpace(line) != "" {
			_ = jsonLine(map[string]string{"type": "line", "text": line})
		}
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *jsonLineWriter) Flush() {
	if strings.TrimSpace(string(w.buf)) != "" {
		_ = jsonLine(map[string]string{"type": "line", "text": string(w.buf)})
	}
	w.buf = nil
}

func logsHeader(w io.Writer, id, path string, l *slurm.Live) {
	if l == nil {
		fmt.Fprintf(w, "job %s · %s\n", id, path)
		return
	}
	parts := []string{"job " + id + " " + l.Name, l.State}
	if l.RunTime > 0 {
		t := slurm.FormatDuration(l.RunTime)
		if l.TimeLimit > 0 {
			t += " of " + slurm.FormatDuration(l.TimeLimit)
		}
		parts = append(parts, t)
	}
	if age, ok := l.LastOutputAge(); ok {
		parts = append(parts, "last output "+slurm.HumanDuration(age)+" ago")
	}
	if p := l.ProgressText(); p != "" {
		parts = append(parts, p)
	}
	fmt.Fprintf(w, "%s\n%s\n", strings.Join(parts, " · "), path)
}

// finalState is the job's sacct record, or nil if Slurm has none yet.
func finalState(e *lifecycle.Env, id string) *slurm.Accounting {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	acc, err := e.Slurm.Accounting(ctx, []string{id}, "")
	if err != nil || len(acc) == 0 {
		return nil
	}
	return &acc[0]
}

func endedText(a slurm.Accounting) string {
	s := fmt.Sprintf("job %s %s: %s", a.ID, a.Name, a.State)
	if !a.Final() {
		return s
	}
	s += ", exit code " + a.ExitCode + ", ran " + slurm.FormatDuration(a.Elapsed)
	if a.TimeLimit > 0 {
		s += " of " + slurm.FormatDuration(a.TimeLimit)
	}
	return s
}

// followOutput streams the file with `tail -F` until the job ends or the
// user presses Ctrl+C. The remote side ends tail when our stdin closes, so
// no tail is left behind on the login node.
func followOutput(e *lifecycle.Env, id, path string, n int, out io.Writer) (interrupted bool, err error) {
	script := "tail -n " + strconv.Itoa(n) + " -F -- " + remote.Quote(path) + " 2>/dev/null & t=$!; cat >/dev/null; kill $t 2>/dev/null"
	cmd := e.R.Command([]string{"-T", "-o", "BatchMode=yes"}, script)
	cmd.Stdout, cmd.Stderr = out, os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return false, err
	}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()

	stop := func() {
		stdin.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	for {
		select {
		case <-sig:
			stop()
			return true, nil
		case err := <-done:
			// tail ended on its own: the connection dropped.
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return false, fmt.Errorf("lost the connection while following job %s; run cluster logs -f %s again", id, id)
			}
			return false, err
		case <-tick.C:
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			_, alive, err := e.Slurm.Alive(ctx, id)
			cancel()
			if err == nil && !alive {
				time.Sleep(3 * time.Second) // let tail print the last lines
				stop()
				return false, nil
			}
		}
	}
}

// ---- wait ----

func waitCmd(g *globals) *cobra.Command {
	var notify, asJSON bool
	var name string
	var interval, limit time.Duration
	cmd := &cobra.Command{
		Use:   "wait [JOBID...]",
		Short: "Block until jobs end; exit non-zero if any failed",
		Long: `Wait until every given job (or every active job matching --name) has ended,
then print each one's final state, exit code and run time from Slurm's
accounting. Exits 1 if any job failed, timed out or was cancelled, so it
fits in scripts: cluster wait 123 && cluster logs 123.

Jobs that already ended are reported straight away.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.env()
			if err != nil {
				return err
			}
			if err := e.R.EnsureMaster(); err != nil {
				return err
			}
			ids := args
			if len(ids) == 0 {
				if name == "" {
					return fmt.Errorf("give job IDs or --name GLOB")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				jobs, err := e.Slurm.Jobs(ctx)
				cancel()
				if err != nil {
					return err
				}
				for _, j := range filterJobs(jobs, name) {
					ids = append(ids, j.ID)
				}
				if len(ids) == 0 {
					return fmt.Errorf("no active jobs match %q", name)
				}
			}
			return runWait(e, ids, interval, limit, notify, asJSON)
		},
	}
	cmd.Flags().BoolVar(&notify, "notify", false, "desktop notification when they end")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable result")
	cmd.Flags().DurationVar(&interval, "interval", 15*time.Second, "how often to check")
	cmd.Flags().DurationVar(&limit, "timeout", 0, "give up after this long (exit 2); 0 waits indefinitely")
	nameFlag(cmd, &name)
	return cmd
}

func runWait(e *lifecycle.Env, ids []string, interval, limit time.Duration, notify, asJSON bool) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	deadline := time.Time{}
	if limit > 0 {
		deadline = time.Now().Add(limit)
	}
	states := map[string]string{}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		jobs, err := e.Slurm.Jobs(ctx, ids...)
		cancel()
		if err != nil && !errors.Is(err, remote.ErrConnection) {
			return err
		}
		active := 0
		if err == nil {
			for _, j := range jobs {
				if !slurm.IsActive(j.State) {
					continue
				}
				active++
				if states[j.ID] != j.State && !asJSON {
					fmt.Fprintf(os.Stderr, "%s job %s %s: %s\n", time.Now().Format("15:04:05"), j.ID, j.Name, j.State)
				}
				states[j.ID] = j.State
			}
			if active == 0 {
				break
			}
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			if !asJSON {
				fmt.Fprintf(os.Stderr, "Gave up after %s; %d job(s) still active.\n", limit, active)
			}
			os.Exit(2)
		}
		select {
		case <-sig:
			fmt.Fprintln(os.Stderr, "Stopped waiting; the jobs keep running.")
			os.Exit(130)
		case <-time.After(interval):
		}
	}

	// squeue has let go of them; sacct has the final word. It can lag a few
	// seconds behind, so retry briefly until every job shows a final state.
	var acc []slurm.Accounting
	for i := 0; i < 6; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		a, err := e.Slurm.Accounting(ctx, ids, "")
		cancel()
		if err == nil {
			acc = a
			if allFinal(acc, len(ids)) {
				break
			}
		}
		time.Sleep(5 * time.Second)
	}
	failed := 0
	for _, a := range acc {
		if a.Failed() {
			failed++
		}
	}
	if asJSON {
		out := []finishedJSON{}
		for _, a := range acc {
			out = append(out, accountingJSON(a))
		}
		_ = printJSON(map[string]any{"jobs": out, "all_ok": failed == 0})
	} else {
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "JOBID\tNAME\tSTATE\tEXIT\tRAN\tLIMIT")
		for _, a := range acc {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Name, a.State, a.ExitCode,
				slurm.FormatDuration(a.Elapsed), slurm.Unlimited(slurm.FormatLimit(a.TimeLimit)))
		}
		w.Flush()
	}
	if notify {
		msg := fmt.Sprintf("%d job(s) finished", len(acc))
		if failed > 0 {
			msg = fmt.Sprintf("%d of %d job(s) failed", failed, len(acc))
		}
		lifecycle.DesktopNotify(msg)
	}
	if failed > 0 {
		return errJobFailed
	}
	return nil
}

func allFinal(acc []slurm.Accounting, want int) bool {
	if len(acc) < want {
		return false
	}
	for _, a := range acc {
		if !a.Final() {
			return false
		}
	}
	return true
}
