package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

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

func shellCmd(g *globals, dev bool) *cobra.Command {
	var sf slurmFlags
	var pf persistFlags
	var browse bool
	var remoteDir string
	cmd := &cobra.Command{
		Use:     "shell",
		Aliases: []string{"sh", "interactive"},
		Short:   "Allocate a compute node and open a shell on it",
		Long: `Allocate a compute node and open a shell on it.

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
			return e.StartShell(lifecycle.ShellOpts{
				Req: req, Profile: profile, Dev: dev, RemoteDir: remoteDir, Browse: browse,
				Persist: !pf.noPersist, Overlap: !pf.noOverlap,
			})
		},
	}
	if dev {
		cmd.Use = "dev"
		cmd.Aliases = []string{"workspace", "session"}
		cmd.Short = "Mount cluster storage and open a compute-node shell; both are cleaned up on exit"
		cmd.Long = strings.Replace(cmd.Long, "Allocate a compute node and open a shell on it.",
			"Mount cluster storage locally, then allocate a compute node and open a shell\non it. The mount is removed when the session ends.", 1)
		cmd.Flags().BoolVarP(&browse, "browse", "b", false, "open the mount in your file manager")
		cmd.Flags().StringVarP(&remoteDir, "dir", "d", "", "remote directory to mount (default: remote home)")
	}
	sf.register(cmd)
	pf.register(cmd)
	return cmd
}

func attachCmd(g *globals) *cobra.Command {
	var pf persistFlags
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
			return e.Attach(id, !pf.noPersist, !pf.noOverlap)
		},
	}
	pf.register(cmd)
	return cmd
}

func psCmd(g *globals) *cobra.Command {
	return &cobra.Command{
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
	cmd.Flags().BoolVar(&cancelUntracked, "cancel-untracked", false, "also cancel running cluster:* jobs not tracked on this machine")
	return cmd
}

func jupyterCmd(g *globals) *cobra.Command {
	var sf slurmFlags
	var head, noBrowser bool
	var port int
	var dir string
	cmd := &cobra.Command{
		Use:     "jupyter",
		Aliases: []string{"lab", "jupyterlab"},
		Short:   "Run JupyterLab on a compute node and open it in your browser",
		Long: `Run JupyterLab on a compute node (or the login node with --head), tunnel it
to localhost, and open your browser. If the network drops, Jupyter keeps
running and the tunnel is restored automatically. Ctrl+C or closing the
terminal stops Jupyter and releases the node.

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
			if head {
				profile = "login-node"
			}
			return e.Jupyter(lifecycle.JupyterOpts{
				Head: head, Req: req, Profile: profile, Dir: dir, Port: port, Browse: !noBrowser,
			})
		},
	}
	sf.register(cmd)
	cmd.Flags().BoolVar(&head, "head", false, "run on the login node instead of a compute node")
	cmd.Flags().BoolVar(&head, "login", false, "alias for --head")
	cmd.Flags().IntVar(&port, "port", 0, "local port (default [jupyter] port, next free one if taken)")
	cmd.Flags().StringVarP(&dir, "dir", "d", "", "remote notebook directory")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the URL without opening a browser")
	// -s/--slurm from the bash version is now the default behavior.
	cmd.Flags().BoolP("slurm", "s", false, "run on a compute node (default)")
	_ = cmd.Flags().MarkHidden("slurm")
	_ = cmd.Flags().MarkHidden("login")
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
				fmt.Printf("Mounted %s:%s at %s. Run `cluster unmount` when done.\n", e.Cfg.Host, dir, mp)
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
directory of the same name in your remote home. Patterns in [sync] exclude
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
			remoteDir := filepath.Base(abs)
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
			for _, x := range e.Cfg.Sync.Exclude {
				rargs = append(rargs, "--exclude="+x)
			}
			src, dst := abs+"/", e.Cfg.Host+":"+remoteDir+"/"
			if action == "pull" {
				src, dst = dst, src
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
	return &cobra.Command{
		Use:     "status",
		Aliases: []string{"stat", "queue", "q"},
		Short:   "Show your Slurm jobs and partition availability",
		Args:    cobra.NoArgs,
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
			jobs, err := e.Slurm.Jobs(ctx)
			if err != nil {
				return err
			}
			parts, err := e.Slurm.Partitions(ctx)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "JOBID\tPARTITION\tNAME\tSTATE\tTIME\tLIMIT\tNODES\tNODELIST(REASON)")
			for _, j := range jobs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", j.ID, j.Partition, j.Name, j.State, j.Time, j.TimeLimit, j.Nodes, j.NodeList)
			}
			if len(jobs) == 0 {
				fmt.Fprintln(w, "(no jobs)")
			}
			fmt.Fprintln(w)
			fmt.Fprintln(w, "PARTITION\tAVAIL\tTIMELIMIT\tNODES\tSTATE\tNODELIST")
			for _, p := range parts {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", p.Name, p.Avail, p.TimeLimit, p.Nodes, p.State, p.NodeList)
			}
			return w.Flush()
		},
	}
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
  cluster exec -g python train.py --lr 0.1`,
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
			req.Name = slurm.JobPrefix + "exec"
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
