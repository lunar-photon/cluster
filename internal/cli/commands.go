package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/lunar-photon/cluster/internal/config"
	"github.com/lunar-photon/cluster/internal/lifecycle"
	"github.com/lunar-photon/cluster/internal/mount"
	"github.com/lunar-photon/cluster/internal/remote"
	"github.com/lunar-photon/cluster/internal/slurm"
)

type persistFlags struct {
	noPersist bool
	noOverlap bool
}

func (p *persistFlags) register(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&p.noPersist, "no-tmux", false, "don't keep the shell in tmux (a reconnect then starts a fresh shell)")
	cmd.Flags().BoolVar(&p.noOverlap, "no-overlap", false, "omit srun --overlap (for Slurm older than 20.11)")
}

// terminalCmd is "Node terminal", and with --mount "Node terminal + cluster mount".
func terminalCmd(g *globals) *cobra.Command { return newTerminalCmd(g, false) }

// devCmd keeps the old `cluster dev` working as `cluster terminal --mount`.
func devCmd(g *globals) *cobra.Command { return newTerminalCmd(g, true) }

func newTerminalCmd(g *globals, alwaysMount bool) *cobra.Command {
	var sf slurmFlags
	var pf persistFlags
	var withMount, browse bool
	var remoteDir string
	cmd := &cobra.Command{
		Use:     "terminal",
		Aliases: []string{"term", "shell", "sh"},
		Short:   "Node terminal: a shell on a compute node (--mount adds the cluster mount)",
		Long: `Node terminal: allocate a compute node and open a shell on it.

Node terminal + cluster mount (--mount): also mount cluster storage at
mount_point on this machine for as long as the session lasts, so you can edit
files with local tools while running them on the node.

The allocation is released when you type exit. Closing the terminal releases
it too, unless a program is still running in the shell; then the job is kept
and you are notified. If the network drops, the job keeps running and cluster
reconnects by itself; with tmux on the cluster (see cluster setup) you come
back to the same shell, with running programs untouched. Detach on purpose
with Ctrl+b d, and come back later with cluster attach.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.env()
			if err != nil {
				return err
			}
			req, profile, err := sf.request(e.Cfg, "")
			if err != nil {
				return err
			}
			if remoteDir == "" {
				remoteDir = e.Cfg.RemoteDir
			}
			if !g.dryRun && !term.IsTerminal(int(os.Stdin.Fd())) {
				return fmt.Errorf("a node terminal needs an interactive terminal; from scripts use cluster exec or cluster submit")
			}
			mountToo := alwaysMount || withMount || browse || cmd.Flags().Changed("dir")
			return e.StartShell(lifecycle.ShellOpts{
				Req: req, Profile: profile, Mount: mountToo, RemoteDir: remoteDir, Browse: browse,
				Persist: !pf.noPersist, Overlap: !pf.noOverlap,
			})
		},
	}
	if alwaysMount {
		cmd.Use = "dev"
		cmd.Aliases = []string{"workspace"}
		cmd.Short = "Same as `cluster terminal --mount`"
		cmd.Hidden = true
	} else {
		cmd.Flags().BoolVar(&withMount, "mount", false, "also mount cluster storage locally for the session (node terminal + cluster mount)")
	}
	cmd.Flags().BoolVarP(&browse, "browse", "b", false, "open the mount in your file manager (implies --mount)")
	cmd.Flags().StringVarP(&remoteDir, "dir", "d", "", "remote directory to mount (implies --mount; default: remote home)")
	sf.register(cmd)
	pf.register(cmd)
	return cmd
}

func attachCmd(g *globals) *cobra.Command {
	var pf persistFlags
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "attach [JOBID]",
		Short: "Reconnect to a running session (default: the most recent)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.env()
			if err != nil {
				return err
			}
			id := ""
			if len(args) == 1 {
				id = args[0]
			} else {
				list, err := e.Store.List()
				if err != nil {
					return err
				}
				if len(list) == 0 {
					return fmt.Errorf("no tracked sessions")
				}
				id = list[0].JobID
			}
			return e.Attach(id, !pf.noPersist, !pf.noOverlap, !noBrowser)
		},
	}
	pf.register(cmd)
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "for a Jupyter session, print the URL without opening a browser")
	return cmd
}

func psCmd(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List sessions started by cluster and whether they are still running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.env()
			if err != nil {
				return err
			}
			if err := e.R.EnsureMaster(); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			list, err := e.Sessions(ctx)
			if err != nil {
				return err
			}
			if asJSON {
				out := []map[string]any{}
				for _, s := range list {
					state := s.Job.State
					if s.Err != nil {
						state = "UNKNOWN"
					} else if !s.Alive {
						state = "ENDED"
					}
					out = append(out, map[string]any{"job_id": s.JobID, "kind": s.Kind, "profile": s.Profile,
						"state": state, "node": firstNonEmpty(s.Job.NodeList, s.Node), "created": s.Created.Format(time.RFC3339),
						"mount": s.Mount, "url": s.URL})
				}
				return printJSON(map[string]any{"sessions": out})
			}
			if len(list) == 0 {
				fmt.Println("No sessions.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "JOB\tKIND\tPROFILE\tSTATE\tNODE\tAGE\tDETAIL")
			stale := false
			for _, s := range list {
				state := s.Job.State
				switch {
				case s.Err != nil:
					state = "unknown"
				case !s.Alive:
					state = "ended"
					stale = true
				case state == "":
					state = "RUNNING"
				}
				node := s.Job.NodeList
				if node == "" {
					node = s.Node
				}
				detail := s.Mount
				if s.URL != "" {
					detail = s.URL
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.JobID, s.Kind, s.Profile, state, node,
					time.Since(s.Created).Round(time.Minute), detail)
			}
			w.Flush()
			if stale {
				fmt.Println("\nRun `cluster gc` to clean up ended sessions.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

func stopCmd(g *globals) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "stop JOBID... | --all",
		Short: "Release sessions: cancel the job, close tunnels, unmount",
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.env()
			if err != nil {
				return err
			}
			if all {
				list, err := e.Store.List()
				if err != nil {
					return err
				}
				for _, s := range list {
					args = append(args, s.JobID)
				}
			}
			if len(args) == 0 {
				return fmt.Errorf("give job IDs or --all (see `cluster ps`)")
			}
			var firstErr error
			for _, id := range args {
				if err := e.Stop(id); err != nil && firstErr == nil {
					firstErr = err
				}
			}
			return firstErr
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "stop every tracked session")
	return cmd
}

func gcCmd(g *globals) *cobra.Command {
	var cancelUntracked bool
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Clean up ended sessions and find leftover cluster jobs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.env()
			if err != nil {
				return err
			}
			return e.GC(cancelUntracked)
		},
	}
	cmd.Flags().BoolVar(&cancelUntracked, "cancel-untracked", false, "also cancel running jobs submitted by cluster that aren't tracked on this machine")
	return cmd
}

func jupyterCmd(g *globals) *cobra.Command {
	var sf slurmFlags
	var noBrowser, renew bool
	var port int
	var dir string
	cmd := &cobra.Command{
		Use:     "jupyter",
		Aliases: []string{"lab", "jupyterlab"},
		Short:   "Jupyter session on node: JupyterLab on a compute node, in your browser",
		Long: `Jupyter session on node: run JupyterLab as a Slurm job on a compute node,
tunnel it to localhost, and open your browser. It never runs on the login node.

If the network drops, Jupyter keeps running and the tunnel is restored
automatically. Ctrl+C stops Jupyter and releases the node, asking first if
kernels are open. Closing the terminal keeps Jupyter running while kernels
are open; cluster attach reopens it.

Ten minutes before the time limit it warns you. With --renew, fifteen
minutes before the limit it submits a replacement job with the same token
and settings; when the old job ends the tunnel moves to the new one, so
your browser tab carries on at the same address. Saved notebooks carry
over; kernel variables don't, since the old job's processes end.

Jupyter must be on the remote PATH after [jupyter] setup runs, e.g.
  [jupyter]
  setup = "module load anaconda3 && conda activate ml"`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.env()
			if err != nil {
				return err
			}
			req, profile, err := sf.request(e.Cfg, e.Cfg.Jupyter.Profile)
			if err != nil {
				return err
			}
			if dir == "" {
				dir = e.Cfg.Jupyter.Dir
			}
			return e.Jupyter(lifecycle.JupyterOpts{
				Req: req, Profile: profile, Dir: dir, Port: port, Browse: !noBrowser, Renew: renew,
			})
		},
	}
	sf.register(cmd)
	cmd.Flags().IntVar(&port, "port", 0, "local port (default [jupyter] port, next free one if taken)")
	cmd.Flags().StringVarP(&dir, "dir", "d", "", "remote notebook directory")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the URL without opening a browser")
	cmd.Flags().BoolVar(&renew, "renew", false, "before the time limit, start a replacement job with the same token so the session carries on")
	// -s/--slurm from the bash version is now the default behavior.
	cmd.Flags().BoolP("slurm", "s", false, "run on a compute node (default)")
	_ = cmd.Flags().MarkHidden("slurm")
	// --head/--login (login-node Jupyter) were removed: no computation runs on
	// the login node. Keep the flags only to explain that.
	for _, name := range []string{"head", "login"} {
		cmd.Flags().Bool(name, false, "")
		_ = cmd.Flags().MarkHidden(name)
	}
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("head") || cmd.Flags().Changed("login") {
			return fmt.Errorf("JupyterLab no longer runs on the login node; it always runs on a compute node (use -c/-m/-t for a small allocation)")
		}
		return nil
	}
	return cmd
}

func mountCmd(g *globals) *cobra.Command {
	var browse bool
	var dir, mp string
	cmd := &cobra.Command{
		Use:   "mount [LOCAL_PATH]",
		Short: "Mount cluster storage locally with sshfs (stays until `cluster unmount`)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.env()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				mp = args[0]
			}
			if mp == "" {
				mp = e.Cfg.LocalMountPoint()
			}
			mp = config.ExpandHome(mp)
			if dir == "" {
				dir = e.Cfg.RemoteDir
			}
			if err := e.R.EnsureMaster(); err != nil {
				return err
			}
			created, err := mount.Mount(e.R, dir, mp)
			if err != nil {
				return err
			}
			if created {
				fmt.Printf("Mounted %s:%s at %s. Run `cluster unmount` when done.\n", e.Cfg.Host, mount.DisplayDir(dir), mp)
			} else {
				fmt.Printf("%s is already mounted.\n", mp)
			}
			if browse {
				mount.Browse(mp)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&browse, "browse", "b", false, "open the mount in your file manager")
	cmd.Flags().StringVarP(&dir, "dir", "d", "", "remote directory (default: remote home)")
	cmd.Flags().StringVarP(&mp, "mount", "m", "", "local mount point (default: mount_point)")
	return cmd
}

func unmountCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "unmount [LOCAL_PATH]",
		Aliases: []string{"umount"},
		Short:   "Detach mounted cluster storage",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			mp := cfg.LocalMountPoint()
			if len(args) == 1 {
				mp = config.ExpandHome(args[0])
			}
			r := remote.New(cfg.Host)
			r.DryRun = g.dryRun
			if !mount.IsMounted(mp) && !g.dryRun {
				fmt.Printf("%s is not mounted.\n", mp)
				return nil
			}
			if err := mount.Unmount(r, mp); err != nil {
				return err
			}
			fmt.Printf("Unmounted %s.\n", mp)
			return nil
		},
	}
}

func syncCmd(g *globals) *cobra.Command {
	var del, preview bool
	cmd := &cobra.Command{
		Use:   "sync push|pull [LOCAL_DIR] [REMOTE_DIR]",
		Short: "Copy code to or from the cluster with rsync, skipping caches and venvs",
		Long: `Copy code to or from the cluster with rsync.

  cluster sync push [LOCAL_DIR] [REMOTE_DIR]   local -> cluster
  cluster sync pull [LOCAL_DIR] [REMOTE_DIR]   cluster -> local

LOCAL_DIR defaults to the current directory; REMOTE_DIR defaults to a
directory of the same name in your remote home, or, inside a project whose
.cluster.toml sets remote_path, the matching place under that path. Patterns in [sync] exclude
are skipped.`,
		Args:      cobra.RangeArgs(1, 3),
		ValidArgs: []string{"push", "pull"},
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.env()
			if err != nil {
				return err
			}
			action := args[0]
			if action != "push" && action != "pull" {
				return fmt.Errorf("sync action must be push or pull, not %q", action)
			}
			local := "."
			if len(args) > 1 {
				local = args[1]
			}
			abs, err := filepath.Abs(local)
			if err != nil {
				return err
			}
			remoteDir := e.Cfg.RemotePathFor(abs)
			if len(args) > 2 {
				remoteDir = args[2]
			}
			if err := e.R.EnsureMaster(); err != nil {
				return err
			}
			rargs := []string{"-az", progressFlag(), "-e", e.R.SSHCommandLine()}
			if del {
				rargs = append(rargs, "--delete")
			}
			if preview {
				rargs = append(rargs, "--dry-run", "--itemize-changes")
			}
			for _, x := range e.Cfg.SyncExcludes() {
				rargs = append(rargs, "--exclude="+x)
			}
			src, dst := abs+"/", e.Cfg.Host+":"+remoteDir+"/"
			if action == "pull" {
				src, dst = dst, src
			} else if !preview && !g.dryRun {
				// rsync creates only the last directory level itself.
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				_, err := e.R.Output(ctx, "mkdir -p "+remoteDirExpr(remoteDir), nil)
				cancel()
				if err != nil {
					return err
				}
			}
			fmt.Printf("%s -> %s\n", src, dst)
			return e.R.Local("rsync", append(rargs, src, dst)...)
		},
	}
	cmd.Flags().BoolVar(&del, "delete", false, "delete files at the destination that don't exist at the source")
	cmd.Flags().BoolVar(&preview, "preview", false, "show what would change without copying")
	return cmd
}

// progressFlag picks rsync's whole-transfer progress when available (3.1+);
// macOS ships an older rsync that only has per-file --progress.
func progressFlag() string {
	out, err := exec.Command("rsync", "--version").Output()
	if err == nil {
		var major, minor int
		if _, err := fmt.Sscanf(strings.TrimPrefix(string(out), "rsync  version "), "%d.%d", &major, &minor); err == nil &&
			(major > 3 || major == 3 && minor >= 1) {
			return "--info=progress2"
		}
	}
	return "--progress"
}

func statusCmd(g *globals) *cobra.Command {
	var name, since string
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "status",
		Aliases: []string{"stat", "queue", "q"},
		Short:   "Show your jobs with their progress, then partitions and your limits",
		Long: `Show your queued and running jobs. For running jobs it also shows how long
ago they last wrote output, and their progress and estimated finish when
they print lines such as "##progress 250/3733" (see cluster logs --help).
Below that come the partitions, free resources and your limits.

SUGGESTED is a time limit from earlier runs with the same job name: the
longest of the last 10 that completed or timed out, plus 50%. It's marked
when a job's limit is shorter than that.

--since 1d adds jobs that ended in that window, from Slurm's accounting:
state, exit code, run time against the limit, CPU efficiency (CPU time used
over cores × run time) and peak memory where the cluster records it.`,
		Args: cobra.NoArgs,
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
			jobs, err := e.Slurm.Jobs(ctx)
			if err != nil {
				return err
			}
			jobs = filterJobs(jobs, name)
			live := map[string]slurm.Live{}
			var running []string
			for _, j := range jobs {
				if j.State == "RUNNING" {
					running = append(running, j.ID)
				}
			}
			if len(running) > 0 {
				ls, err := e.Slurm.LiveJobs(ctx, running...)
				if err == nil {
					rememberPaths(ls)
					for _, l := range ls {
						live[l.ID] = l
					}
				}
			}
			info, err := e.Slurm.ClusterInfo(ctx)
			if err != nil {
				return err
			}
			if p, err := e.Cfg.Profile(""); err == nil {
				info.PreferredQOS = p.QOS
			}

			// Accounting: 30 days for suggestions, longer if --since asks.
			histStart, cutoff := "now-30days", time.Time{}
			if since != "" {
				start, c, err := slurm.SinceArg(since, time.Now())
				if err != nil {
					return err
				}
				cutoff = c
				if c.Before(time.Now().Add(-30 * 24 * time.Hour)) {
					histStart = start
				}
			}
			history, herr := e.Slurm.Accounting(ctx, nil, histStart)
			if herr != nil && since != "" {
				return fmt.Errorf("reading accounting: %w", herr)
			}
			suggest := func(name string) (slurm.Suggestion, bool) { return slurm.SuggestLimit(history, name, 10, 1.5) }
			var finished []slurm.Accounting
			if since != "" {
				for _, a := range history {
					if a.Final() && !a.End.Before(cutoff) && nameMatches(name, a.Name) {
						finished = append(finished, a)
					}
				}
			}

			if asJSON {
				out := []jobJSON{}
				for _, j := range jobs {
					jj := jobJSON{ID: j.ID, Name: j.Name, Partition: j.Partition, State: j.State, Time: j.Time,
						TimeLimit: j.TimeLimit, Nodes: j.Nodes, Where: j.NodeList, CPUs: j.CPUs, Mem: j.Mem,
						GPUs: j.GPUs(), ByCluster: j.ByCluster()}
					if l, ok := live[j.ID]; ok {
						jj.Output = liveJSON(l, 0)
					}
					if s, ok := suggest(j.Name); ok {
						jj.Suggested = &suggestionJSON{LimitS: int64(s.Limit.Seconds()), Runs: s.Runs, LongestS: int64(s.Longest.Seconds()),
							LimitShort: slurm.ParseDuration(j.TimeLimit) > 0 && slurm.ParseDuration(j.TimeLimit) < s.Limit}
					}
					out = append(out, jj)
				}
				result := map[string]any{"jobs": out, "cluster": clusterJSON(e.Cfg, info)}
				if since != "" {
					f := []finishedJSON{}
					for _, a := range finished {
						f = append(f, accountingJSON(a))
					}
					result["finished"] = f
				}
				return printJSON(result)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "JOBID\tPARTITION\tNAME\tSTATE\tTIME\tLIMIT\tCPUs\tNODELIST(REASON)\tLAST OUTPUT\tPROGRESS\tSUGGESTED")
			for _, j := range jobs {
				last, prog, sug := "", "", ""
				if l, ok := live[j.ID]; ok {
					if age, ok := l.LastOutputAge(); ok {
						last = slurm.HumanDuration(age) + " ago"
					}
					prog = l.ProgressText()
				}
				if s, ok := suggest(j.Name); ok {
					sug = fmt.Sprintf("%s (%s)", slurm.FormatDuration(s.Limit), slurm.Plural(s.Runs, "run"))
					if lim := slurm.ParseDuration(j.TimeLimit); lim > 0 && lim < s.Limit {
						sug = "! " + sug + ", limit shorter"
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", j.ID, j.Partition, j.Name, j.State,
					j.Time, j.TimeLimit, j.CPUs, j.NodeList, last, prog, sug)
			}
			if len(jobs) == 0 {
				fmt.Fprintln(w, "(no jobs)")
			}
			w.Flush()
			if since != "" {
				fmt.Printf("\nEnded since %s:\n", cutoff.Format("2006-01-02 15:04"))
				printFinished(os.Stdout, finished)
			}
			fmt.Println()
			printInfo(os.Stdout, e.Cfg, info)
			return nil
		},
	}
	nameFlag(cmd, &name)
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	cmd.Flags().StringVar(&since, "since", "", "also list jobs that ended in this window: 30m, 6h, 1d, 2w or a date")
	return cmd
}

func execCmd(g *globals) *cobra.Command {
	var sf slurmFlags
	cmd := &cobra.Command{
		Use:     "exec [OPTIONS] COMMAND [ARGS...]",
		Aliases: []string{"run"},
		Short:   "Run one command on a compute node and stream its output",
		Long: `Run one command on a compute node with srun and stream its output.

Options must come before the command. A single argument is run as a shell
command line (so pipes work); several arguments are run as an argv:

  cluster exec "nvidia-smi | head"
  cluster exec -g python train.py --lr 0.1

exec holds its allocation only while this connection stays open. For work
longer than a few minutes, submit a batch job with cluster submit and follow
it with cluster logs -f.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.env()
			if err != nil {
				return err
			}
			req, _, err := sf.request(e.Cfg, "")
			if err != nil {
				return err
			}
			line := args[0]
			if len(args) > 1 {
				line = remote.Join(args...)
			}
			if err := e.R.EnsureMaster(); err != nil {
				return err
			}
			req.Name = e.Cfg.JobName("exec")
			{
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				err := e.CheckRequest(ctx, req)
				cancel()
				if err != nil {
					return err
				}
			}
			argv := append(append([]string{"srun"}, req.Args()...), "bash", "-lc", line)
			fmt.Fprintf(os.Stderr, "srun %s\n", req)
			err = e.R.Interactive(remote.Join(argv...))
			if code := remote.ExitCode(err); code > 0 {
				os.Exit(code)
			}
			return err
		},
	}
	sf.register(cmd)
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func configCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "config",
		Aliases: []string{"cfg"},
		Short:   "Edit the config file in $EDITOR (creating it if needed)",
		Long: `Edit the config file in $EDITOR, creating it from your current settings if
it doesn't exist. Example:

  host = "hpc.example.edu"        # or a ~/.ssh/config alias
  mount_point = "~/Cluster"
  default_profile = "cpu"

  [slurm]                          # defaults for every profile
  partition = "medium"
  account = "mylab"
  cpus = 4
  mem = "8G"
  time = "04:00:00"

  [profiles.cpu]

  [profiles.gpu]                   # only what differs from [slurm]
  partition = "gpu"
  qos = "gpu_qos"
  gpus = 1
  mem = "16G"

  [jupyter]
  port = 8888
  setup = "module load anaconda3"

  [sync]
  exclude = [".git/", "__pycache__/", ".venv/", "*.ckpt"]`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			if _, err := os.Stat(cfg.Path); os.IsNotExist(err) {
				if err := cfg.Save(); err != nil {
					return err
				}
				fmt.Printf("Created %s\n", cfg.Path)
			}
			editor := os.Getenv("VISUAL")
			if editor == "" {
				editor = os.Getenv("EDITOR")
			}
			if editor == "" {
				data, _ := os.ReadFile(cfg.Path)
				fmt.Printf("%s\n\n%s", cfg.Path, data)
				fmt.Println("\nSet $EDITOR to edit it here, or open the file directly.")
				return nil
			}
			c := exec.Command("sh", "-c", editor+` "$1"`, "sh", cfg.Path)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				return err
			}
			if _, err := config.Load(cfg.Path); err != nil {
				return fmt.Errorf("the edited config does not parse: %w", err)
			}
			return nil
		},
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "show",
			Short: "Print the effective configuration",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := g.load()
				if err != nil {
					return err
				}
				data, err := cfg.Encode()
				if err != nil {
					return err
				}
				fmt.Printf("# from %s", cfg.Path)
				if cfg.Legacy {
					fmt.Print(" (not yet created; values from the old shell-style config)")
				}
				fmt.Printf("\n%s", data)
				return nil
			},
		},
		&cobra.Command{
			Use:   "path",
			Short: "Print the config file path",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := g.load()
				fmt.Println(cfg.Path)
				return err
			},
		},
		&cobra.Command{
			Use:   "migrate",
			Short: "Convert the old shell-style config to config.toml",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := g.load()
				if err != nil {
					return err
				}
				if !cfg.Legacy {
					fmt.Printf("Nothing to migrate: %s is already in use.\n", cfg.Path)
					return nil
				}
				if err := cfg.Save(); err != nil {
					return err
				}
				fmt.Printf("Wrote %s. The old file is left in place and no longer read.\n", cfg.Path)
				return nil
			},
		},
	)
	return cmd
}

// printFinished lists ended jobs with what they used. The peak-memory column
// appears only if the cluster records it.
func printFinished(out io.Writer, finished []slurm.Accounting) {
	if len(finished) == 0 {
		fmt.Fprintln(out, "(none)")
		return
	}
	rss := false
	for _, a := range finished {
		rss = rss || a.MaxRSSMB >= 0
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	head := "JOBID\tNAME\tSTATE\tEXIT\tRAN / LIMIT\tCPUs\tCPU EFFICIENCY"
	if rss {
		head += "\tPEAK MEMORY"
	}
	fmt.Fprintln(w, head)
	for _, a := range finished {
		eff := "-"
		if e := a.CPUEfficiency(); e >= 0 {
			eff = fmt.Sprintf("%.0f%%", e*100)
		}
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s / %s\t%d\t%s", a.ID, a.Name, a.State, a.ExitCode,
			slurm.FormatDuration(a.Elapsed), slurm.FormatLimit(a.TimeLimit), a.AllocCPUs, eff)
		if rss {
			m := "-"
			if a.MaxRSSMB >= 0 {
				m = slurm.FormatMem(a.MaxRSSMB)
			}
			row += "\t" + m
		}
		fmt.Fprintln(w, row)
	}
	w.Flush()
	if !rss {
		fmt.Fprintln(out, "Peak memory isn't recorded by this cluster's accounting.")
	}
}
