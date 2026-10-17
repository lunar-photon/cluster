// Package lifecycle owns every resource this tool creates on the user's
// behalf — Slurm allocations, sshfs mounts, Jupyter servers and tunnels —
// from creation through reconnects to verified release.
//
// The rules:
//   - Every allocation is an sbatch job whose ID is recorded in the session
//     store before anything else happens, so nothing is ever untracked.
//   - Typing `exit` or closing the terminal releases the job (scancel, then
//     confirm it left the queue) and unmounts anything the session mounted.
//   - Losing the network does not: the job keeps running, the tool waits for
//     the cluster to become reachable and reattaches by itself.
//   - Anything that could not be released (cluster unreachable) stays in the
//     store and shows up in `cluster ps` and `cluster gc`.
package lifecycle

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lunar-photon/cluster/internal/config"
	"github.com/lunar-photon/cluster/internal/mount"
	"github.com/lunar-photon/cluster/internal/remote"
	"github.com/lunar-photon/cluster/internal/session"
	"github.com/lunar-photon/cluster/internal/slurm"
)

type Env struct {
	Cfg   config.Config
	R     *remote.Client
	Slurm *slurm.Client
	Store *session.Store
	Out   io.Writer
	In    io.Reader
	// NoDesktopNotify disables desktop notifications (tests).
	NoDesktopNotify bool
}

func New(cfg config.Config, r *remote.Client) *Env {
	return &Env{
		Cfg:   cfg,
		R:     r,
		Slurm: slurm.New(r),
		Store: session.DefaultStore(),
		Out:   os.Stderr,
		In:    os.Stdin,
	}
}

func (e *Env) printf(format string, a ...any) { fmt.Fprintf(e.Out, format, a...) }

// notify reports something the user may not see in the terminal (because it
// was just closed): a desktop notification when possible, the terminal, and
// a line in the event log.
func (e *Env) notify(msg string) {
	e.printf("%s\n", msg)
	_ = os.MkdirAll(session.Dir(), 0o700)
	if f, err := os.OpenFile(filepath.Join(session.Dir(), "events.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), msg)
		f.Close()
	}
	if e.NoDesktopNotify {
		return
	}
	var cmd *exec.Cmd
	switch {
	case runtime.GOOS == "darwin":
		cmd = exec.Command("osascript", "-e", "display notification "+strconv.Quote(msg)+` with title "cluster"`)
	default:
		if _, err := exec.LookPath("notify-send"); err == nil {
			cmd = exec.Command("notify-send", "cluster", msg)
		}
	}
	if cmd != nil {
		_ = cmd.Run()
	}
}

// timeout returns a context for one short remote operation.
func timeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// interrupts returns a context cancelled by Ctrl+C, SIGHUP or SIGTERM, and
// reports which signal arrived.
func interrupts() (context.Context, func() os.Signal, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	var got os.Signal
	done := make(chan struct{})
	go func() {
		select {
		case got = <-ch:
			cancel()
		case <-done:
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			signal.Stop(ch)
			close(done)
			cancel()
		})
	}
	which := func() os.Signal { <-ctx.Done(); return got }
	return ctx, which, stop
}

// Reconnect waits until the cluster is reachable and the shared ssh
// connection is back, retrying with backoff. Password or 2FA prompts appear
// in the terminal. It returns ctx.Err() if interrupted.
func (e *Env) Reconnect(ctx context.Context) error {
	delay := 2 * time.Second
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e.R.Reachable(ctx) {
			if err := e.R.EnsureMaster(); err == nil {
				return nil
			}
		}
		e.printf("\r\033[K  cluster unreachable (attempt %d) — retrying in %s, Ctrl+C to stop", attempt, delay)
		select {
		case <-ctx.Done():
			e.printf("\n")
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 30*time.Second {
			delay *= 2
		}
	}
}

// Release frees everything a session holds and removes it from the store.
// The session is only forgotten once its job is confirmed gone, so a release
// that fails while offline can be retried by `cluster gc`.
func (e *Env) Release(sess session.Session) error {
	ctx, cancel := timeout(30 * time.Second)
	defer cancel()

	if sess.TunnelPID > 0 {
		if p, err := os.FindProcess(sess.TunnelPID); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}

	jobErr := e.cancelAndConfirm(ctx, sess.JobID)
	if jobErr == nil && len(sess.RemoteFiles) > 0 {
		_, _ = e.R.Run(ctx, append([]string{"rm", "-f", "--"}, sess.RemoteFiles...)...)
	}

	var mountErr error
	if sess.Mount != "" {
		others, _ := e.Store.List()
		if !session.MountInUse(others, sess.Mount, sess.JobID) {
			if mountErr = mount.Unmount(e.R, sess.Mount); mountErr == nil {
				e.printf("Unmounted %s.\n", sess.Mount)
			}
		}
	}

	if jobErr != nil {
		e.printf("Could not release job %s: %v\n", sess.JobID, jobErr)
		e.printf("It stays tracked; run `cluster gc` once the cluster is reachable.\n")
		// Keep the record but drop the mount reference we already handled.
		if mountErr == nil && sess.Mount != "" {
			sess.Mount = ""
			_ = e.Store.Add(sess)
		}
		return jobErr
	}
	_ = e.Store.Delete(sess.JobID)
	return mountErr
}

// cancelAndConfirm scancels a job and waits until Slurm no longer reports it
// as holding resources.
func (e *Env) cancelAndConfirm(ctx context.Context, id string) error {
	if e.R.DryRun {
		return e.Slurm.Cancel(ctx, id)
	}
	if err := e.Slurm.Cancel(ctx, id); err != nil {
		return err
	}
	for i := 0; i < 10; i++ {
		job, alive, err := e.Slurm.Alive(ctx, id)
		if err != nil {
			return err
		}
		if !alive {
			e.printf("Released job %s.\n", id)
			return nil
		}
		if i == 9 {
			return fmt.Errorf("scancel sent but job %s is still %s", id, job.State)
		}
		time.Sleep(time.Second)
	}
	return nil
}

// Stop releases a tracked session, or scancels an untracked job ID.
func (e *Env) Stop(jobID string) error {
	if err := e.R.EnsureMaster(); err != nil {
		return err
	}
	sess, ok, err := e.Store.Get(jobID)
	if err != nil {
		return err
	}
	if !ok {
		ctx, cancel := timeout(30 * time.Second)
		defer cancel()
		return e.cancelAndConfirm(ctx, jobID)
	}
	return e.Release(sess)
}

// Status pairs a tracked session with its live Slurm state.
type Status struct {
	session.Session
	Job   slurm.Job
	Alive bool
	// Err is set when liveness could not be determined.
	Err error
}

// Sessions returns tracked sessions with their live state, using one squeue
// call for all of them.
func (e *Env) Sessions(ctx context.Context) ([]Status, error) {
	list, err := e.Store.List()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, s := range list {
		ids = append(ids, s.JobID)
	}
	var jobs []slurm.Job
	var qerr error
	if len(ids) > 0 {
		jobs, qerr = e.Slurm.Jobs(ctx, ids...)
	}
	byID := map[string]slurm.Job{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	out := make([]Status, 0, len(list))
	for _, s := range list {
		st := Status{Session: s}
		switch {
		case qerr != nil:
			st.Err = qerr
		default:
			st.Job = byID[s.JobID]
			st.Alive = slurm.IsActive(st.Job.State)
		}
		out = append(out, st)
	}
	return out, nil
}

// GC forgets sessions whose jobs have ended, unmounting what they mounted,
// and reports (or with cancel, cancels) running jobs named cluster:* that no
// tracked session owns — e.g. from a machine that crashed.
func (e *Env) GC(cancelUntracked bool) error {
	if err := e.R.EnsureMaster(); err != nil {
		return err
	}
	ctx, cancel := timeout(60 * time.Second)
	defer cancel()

	statuses, err := e.Sessions(ctx)
	if err != nil {
		return err
	}
	tracked := map[string]bool{}
	cleaned := 0
	for _, st := range statuses {
		tracked[st.JobID] = true
		if st.Err != nil || st.Alive {
			continue
		}
		e.printf("Job %s has ended; cleaning up.\n", st.JobID)
		st.TunnelPID = 0
		if err := e.Release(st.Session); err == nil {
			cleaned++
		}
	}

	jobs, err := e.Slurm.Jobs(ctx)
	if err != nil {
		return err
	}
	var orphans []string
	for _, j := range jobs {
		if len(j.Name) > len(slurm.JobPrefix) && j.Name[:len(slurm.JobPrefix)] == slurm.JobPrefix && !tracked[j.ID] && slurm.IsActive(j.State) {
			orphans = append(orphans, j.ID)
			e.printf("Untracked job %s (%s, %s, %s)\n", j.ID, j.Name, j.State, j.NodeList)
		}
	}
	if len(orphans) > 0 {
		if cancelUntracked {
			for _, id := range orphans {
				_ = e.cancelAndConfirm(ctx, id)
			}
		} else {
			e.printf("These were started by cluster but are not tracked on this machine. Cancel them with `cluster gc --cancel-untracked`.\n")
		}
	}
	if cleaned == 0 && len(orphans) == 0 {
		e.printf("Nothing to clean up.\n")
	}
	return nil
}

// describeAllocation prints the node's hardware and what the job was given,
// e.g. "Node: 28 cores (2 sockets × 14), 125 GB RAM · Your job: 4 CPUs, 8G".
// It is informational, so lookup failures are ignored.
func (e *Env) describeAllocation(job slurm.Job) {
	var parts []string
	if node := job.NodeList; node != "" && !strings.ContainsAny(node, "[,(") {
		ctx, cancel := timeout(15 * time.Second)
		n, err := e.Slurm.NodeInfo(ctx, node)
		cancel()
		if err == nil {
			parts = append(parts, "Node: "+n.Shape().String())
		}
	}
	if job.CPUs != "" {
		mine := "Your job: " + job.CPUs + " CPUs"
		if job.Mem != "" && job.Mem != "0" {
			mine += ", " + job.Mem + " memory"
		}
		if g := job.GPUs(); g > 0 {
			mine += ", " + slurm.Plural(g, "GPU")
		}
		parts = append(parts, mine)
	}
	if len(parts) > 0 {
		e.printf("  %s\n", strings.Join(parts, " · "))
	}
}
