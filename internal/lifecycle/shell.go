package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lunar-photon/cluster/internal/mount"
	"github.com/lunar-photon/cluster/internal/remote"
	"github.com/lunar-photon/cluster/internal/session"
	"github.com/lunar-photon/cluster/internal/slurm"
)

// exitDetached is the status the attach wrapper returns when the user
// detached from tmux and the session is still alive. It is chosen to be an
// unlikely shell exit status.
const exitDetached = 197

type ShellOpts struct {
	Req     slurm.Request
	Profile string
	// Mount also mounts cluster storage for the lifetime of the session
	// ("node terminal + cluster mount").
	Mount     bool
	RemoteDir string
	Browse    bool
	// Persist keeps the shell in tmux on the node (when tmux exists there)
	// so a reconnect resumes it instead of starting a new shell.
	Persist bool
	// Overlap passes --overlap to srun (needed on Slurm >= 20.11 to share
	// the allocation with the holding batch step).
	Overlap bool
}

// findTmux sets $T to a working tmux: one on PATH, or the copy that
// `cluster setup` builds into ~/.local/bin.
const findTmux = `T=$(command -v tmux 2>/dev/null || true); ` +
	`if [ -z "$T" ] && [ -x "$HOME/.local/bin/tmux" ]; then T="$HOME/.local/bin/tmux"; fi; ` +
	`if [ -n "$T" ] && ! "$T" -V >/dev/null 2>&1; then T=; fi; `

// holdScript is the batch job that holds the allocation. With persist, it
// also starts the tmux server inside the batch step, so the shell outlives
// any one srun attach (Slurm kills a step's processes when the step ends).
// Users without their own tmux config get mouse scrolling and a status bar
// that says how to detach.
func holdScript(persist bool) string {
	s := "#!/bin/bash\n# Holds the allocation for a `cluster terminal` session.\n"
	if persist {
		s += findTmux + "\n" +
			`if [ -n "$T" ]; then
  S="cluster-$SLURM_JOB_ID"
  "$T" -L "$S" new-session -d -s main
  if [ ! -e "$HOME/.tmux.conf" ] && [ ! -e "${XDG_CONFIG_HOME:-$HOME/.config}/tmux/tmux.conf" ]; then
    "$T" -L "$S" set -g mouse on \; set -g history-limit 50000 \; \
      set -g status-left " job $SLURM_JOB_ID " \; set -g status-left-length 24 \; \
      set -g status-right " Ctrl+b d: detach (job keeps running) "
  fi
fi
`
	}
	return s + "exec sleep infinity\n"
}

// attachCommand is the remote command that connects the terminal to a job.
func attachCommand(jobID string, persist, overlap bool) string {
	inner := "exec bash -l"
	if persist {
		sock := remote.Quote("cluster-" + jobID)
		inner = findTmux + fmt.Sprintf(`if [ -n "$T" ] && "$T" -L %[1]s has-session -t main 2>/dev/null; then `+
			`"$T" -L %[1]s attach -t main; `+
			`if "$T" -L %[1]s has-session -t main 2>/dev/null; then exit %[2]d; fi; exit 0; `+
			`else echo "[cluster] tmux is not available on this node, so this shell ends if the connection drops. Run 'cluster setup' to install it." >&2; `+
			`exec bash -l; fi`, sock, exitDetached)
	}
	argv := []string{"srun", "--jobid=" + jobID}
	if overlap {
		argv = append(argv, "--overlap")
	}
	argv = append(argv, "--pty", "bash", "-c", inner)
	return remote.Join(argv...)
}

// busyCommand lists what each tmux pane in the job is running.
func busyCommand(jobID string, overlap bool) string {
	inner := findTmux + fmt.Sprintf(`[ -n "$T" ] && "$T" -L %s list-panes -a -F '#{pane_current_command}' 2>/dev/null; true`,
		remote.Quote("cluster-"+jobID))
	argv := []string{"srun", "--jobid=" + jobID}
	if overlap {
		argv = append(argv, "--overlap")
	}
	return remote.Join(append(argv, "bash", "-c", inner)...)
}

var shells = map[string]bool{"bash": true, "zsh": true, "sh": true, "fish": true, "tcsh": true, "csh": true, "ksh": true, "dash": true}

// runningPrograms reports what is running in the job's tmux panes, other
// than idle shells. Without tmux the list is empty: the shell died with the
// connection, so there is nothing left to keep.
func (e *Env) runningPrograms(jobID string, overlap bool) ([]string, error) {
	ctx, cancel := timeout(30 * time.Second)
	defer cancel()
	out, err := e.R.Output(ctx, busyCommand(jobID, overlap), nil)
	if err != nil {
		return nil, err
	}
	var busy []string
	for _, cmd := range strings.Fields(out) {
		if !shells[strings.TrimPrefix(cmd, "-")] {
			busy = append(busy, cmd)
		}
	}
	return busy, nil
}

// terminalClosed decides what happens to a session whose terminal went away:
// release it if nothing is running in it, otherwise keep it and say so.
func (e *Env) terminalClosed(sess session.Session, overlap bool) error {
	busy, err := e.runningPrograms(sess.JobID, overlap)
	switch {
	case err != nil:
		e.notify(fmt.Sprintf("Terminal closed; couldn't check job %s (%v), so it was kept. `cluster stop %s` releases it.", sess.JobID, err, sess.JobID))
		return nil
	case len(busy) > 0:
		e.notify(fmt.Sprintf("Terminal closed while %s was running, so job %s was kept. `cluster attach %s` to return, `cluster stop %s` to release.",
			strings.Join(busy, ", "), sess.JobID, sess.JobID, sess.JobID))
		return nil
	}
	return e.Release(sess)
}

// StartShell allocates a node, optionally mounts storage, and attaches.
func (e *Env) StartShell(o ShellOpts) error {
	if err := e.R.EnsureMaster(); err != nil {
		return err
	}
	sess := session.Session{
		Kind:    session.Terminal,
		Host:    e.R.Host,
		Profile: o.Profile,
		Created: time.Now(),
	}

	if o.Mount {
		sess.Kind = session.TerminalMount
	}
	o.Req.Name = e.Cfg.JobName(strings.ReplaceAll(string(sess.Kind), "+", "-"))
	{
		ctx, cancel := timeout(30 * time.Second)
		err := e.CheckPolicy(ctx, o.Req)
		cancel()
		if err != nil {
			return err
		}
	}
	if o.Mount {
		mp := e.Cfg.LocalMountPoint()
		created, err := mount.Mount(e.R, o.RemoteDir, mp)
		switch {
		case err != nil:
			e.printf("Warning: %v — continuing without the mount.\n", err)
		case created:
			e.printf("Mounted %s:%s at %s.\n", e.R.Host, mount.DisplayDir(o.RemoteDir), mp)
			sess.Mount = mp
		default:
			// Share ownership with a tracked session that mounted it, so whichever
			// finishes last unmounts. A mount made by `cluster mount` stays.
			if others, _ := e.Store.List(); session.MountInUse(others, mp, "") {
				sess.Mount = mp
			}
			e.printf("%s is already mounted.\n", mp)
		}
		if o.Browse && err == nil {
			mount.Browse(mp)
		}
	}

	ctx, cancel := timeout(60 * time.Second)
	id, err := e.Slurm.Submit(ctx, o.Req, holdScript(o.Persist), "--output=/dev/null")
	cancel()
	if err != nil {
		if sess.Mount != "" {
			_ = mount.Unmount(e.R, sess.Mount)
		}
		return err
	}
	sess.JobID = id
	if err := e.Store.Add(sess); err != nil {
		e.printf("Warning: could not record session: %v\n", err)
	}
	e.printf("Submitted job %s: %s\n", id, o.Req)

	if err := e.waitRunning(id); err != nil {
		_ = e.Release(sess)
		return err
	}
	return e.attachLoop(sess, o.Persist, o.Overlap)
}

// Attach reconnects to a tracked terminal session, or re-tunnels a Jupyter one.
func (e *Env) Attach(jobID string, persist, overlap, browse bool) error {
	if err := e.R.EnsureMaster(); err != nil {
		return err
	}
	sess, ok, err := e.Store.Get(jobID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("job %s is not tracked; see `cluster ps`", jobID)
	}
	if sess.Kind == session.Jupyter {
		if sess.Node == "" {
			return fmt.Errorf("Jupyter session %s never finished starting; release it with `cluster stop %s`", jobID, jobID)
		}
		return e.serveJupyter(sess, browse)
	}
	ctx, cancel := timeout(30 * time.Second)
	_, alive, err := e.Slurm.Alive(ctx, jobID)
	cancel()
	if err != nil {
		return err
	}
	if !alive {
		e.printf("Job %s has ended.\n", jobID)
		return e.Release(sess)
	}
	return e.attachLoop(sess, persist, overlap)
}

// waitRunning polls until the job starts. Ctrl+C or closing the terminal
// while queued returns an error, and the caller cancels the job.
func (e *Env) waitRunning(id string) error {
	if e.R.DryRun {
		return nil
	}
	ctx, which, stop := interrupts()
	defer stop()
	last := ""
	for {
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		job, alive, err := e.Slurm.Alive(pctx, id)
		cancel()
		switch {
		case ctx.Err() != nil:
			e.printf("\nInterrupted (%v) while queued; cancelling job %s.\n", which(), id)
			return ctx.Err()
		case errors.Is(err, remote.ErrConnection):
			e.printf("\nLost connection while queued.\n")
			if rerr := e.Reconnect(ctx); rerr != nil {
				return rerr
			}
			continue
		case err != nil:
			return err
		case !alive:
			return fmt.Errorf("job %s ended before it started (state %q)", id, job.State)
		case job.State == "RUNNING":
			e.printf("\r\033[KJob %s running on %s.\n", id, job.NodeList)
			e.describeAllocation(job)
			return nil
		}
		status := fmt.Sprintf("%s %s", job.State, job.NodeList)
		if status != last {
			e.printf("\r\033[KWaiting in queue: %s (Ctrl+C to cancel)", status)
			last = status
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// attachLoop keeps the terminal attached to the job until the user ends the
// session, reconnecting through network loss.
func (e *Env) attachLoop(sess session.Session, persist, overlap bool) error {
	if persist {
		e.printf("Connected to job %s. `exit` releases the node; Ctrl+b d detaches and leaves it running.\n"+
			"Closing the terminal releases it only if nothing is running.\n", sess.JobID)
	} else {
		e.printf("Connected to job %s. `exit` or closing this terminal releases the node.\n", sess.JobID)
	}

	// SIGINT is not handled here: while ssh -t runs the terminal is in raw
	// mode, so Ctrl+C goes to the remote shell. SIGHUP/SIGTERM mean the
	// terminal is closing.
	// Catching SIGINT (rather than signal.Ignore) keeps it from being ignored
	// in child processes too, which would break Ctrl+C at ssh prompts.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(hup)
	discard := make(chan os.Signal, 1)
	signal.Notify(discard, os.Interrupt)
	defer signal.Stop(discard)

	for {
		err := e.R.InteractiveUntil(attachCommand(sess.JobID, persist, overlap), hup)
		if errors.Is(err, remote.ErrStopped) {
			return e.terminalClosed(sess, overlap)
		}
		code := 0
		if err != nil {
			code = remote.ExitCode(err)
		}

		// ssh may die of the same SIGHUP that tells us the terminal closed;
		// give the signal a moment to arrive before deciding what happened.
		select {
		case <-hup:
			return e.terminalClosed(sess, overlap)
		case <-time.After(200 * time.Millisecond):
		}

		switch code {
		case exitDetached:
			e.printf("Detached. Job %s is still running: `cluster attach %s` to return, `cluster stop %s` to release.\n",
				sess.JobID, sess.JobID, sess.JobID)
			return nil
		case 255:
			e.printf("\nConnection lost. Job %s keeps running; reconnecting...\n", sess.JobID)
			ctx, which, stop := interrupts()
			rerr := e.Reconnect(ctx)
			sig := os.Signal(nil)
			if rerr != nil {
				sig = which()
			}
			stop()
			if rerr != nil {
				e.printf("Stopped reconnecting (%v). Job %s is still running until its time limit:\n"+
					"  cluster attach %s   # resume\n  cluster stop %s     # release it\n", sig, sess.JobID, sess.JobID, sess.JobID)
				return nil
			}
			ctx2, cancel := timeout(30 * time.Second)
			_, alive, aerr := e.Slurm.Alive(ctx2, sess.JobID)
			cancel()
			if aerr == nil && !alive {
				e.printf("Job %s ended while disconnected (time limit?).\n", sess.JobID)
				return e.Release(sess)
			}
			e.printf("Reconnected; reattaching to job %s.\n", sess.JobID)
		default:
			if code != 0 && code != exitDetached && err != nil && !errors.Is(err, remote.ErrConnection) {
				// A non-zero status is usually just the shell's last exit code,
				// but srun failures land here too, so show it.
				e.printf("Session ended (status %d).\n", code)
			}
			return e.Release(sess)
		}
	}
}
