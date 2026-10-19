package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lunar-photon/cluster/internal/lifecycle"
	"github.com/lunar-photon/cluster/internal/remote"
	"github.com/lunar-photon/cluster/internal/session"
	"github.com/lunar-photon/cluster/internal/slurm"
)

// logsDir holds the output of jobs started by `cluster submit`, inside the
// project's directory on the cluster. It is never synced up.
const logsDir = ".cluster-logs"

// remoteDirExpr is a remote path as a shell word: relative paths are under
// the remote home ($HOME expands on the cluster).
func remoteDirExpr(p string) string {
	if strings.HasPrefix(p, "/") {
		return remote.Quote(p)
	}
	return `"$HOME"/` + remote.Quote(strings.TrimPrefix(p, "~/"))
}

// rsyncArgs are the shared rsync options: compression, the shared ssh
// connection and the configured excludes.
func rsyncArgs(e *lifecycle.Env, extra ...string) []string {
	args := []string{"-az", "-e", e.R.SSHCommandLine()}
	for _, x := range e.Cfg.SyncExcludes() {
		args = append(args, "--exclude="+x)
	}
	return append(args, extra...)
}

// pushProject makes the remote directory and copies the local one into it.
func pushProject(e *lifecycle.Env, localAbs, remoteDir string, quiet bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	_, err := e.R.Output(ctx, "mkdir -p "+remoteDirExpr(remoteDir)+" "+remoteDirExpr(remoteDir+"/"+logsDir), nil)
	cancel()
	if err != nil {
		return err
	}
	extra := []string{"--exclude=/" + logsDir + "/"}
	if !quiet {
		extra = append(extra, progressFlag())
	}
	return e.R.Local("rsync", append(rsyncArgs(e, extra...), localAbs+"/", e.Cfg.Host+":"+remoteDir+"/")...)
}

// fetchPaths copies the named paths (relative to the project) back,
// keeping their place in the tree.
func fetchPaths(e *lifecycle.Env, localAbs, remoteDir string, paths []string) error {
	var srcs []string
	for _, p := range paths {
		p = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(p)), "./")
		if p == "." || strings.HasPrefix(p, "../") || filepath.IsAbs(p) {
			return fmt.Errorf("--fetch %q: give paths inside the project", p)
		}
		srcs = append(srcs, e.Cfg.Host+":"+remoteDir+"/./"+p)
	}
	args := append([]string{"-az", "--relative", "-e", e.R.SSHCommandLine()}, srcs...)
	return e.R.Local("rsync", append(args, localAbs+"/")...)
}

var nameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// jobKind names a job after what it runs: "python fit.py" → "fit".
func jobKind(argv []string) string {
	if len(argv) == 1 {
		argv = strings.Fields(argv[0])
	}
	pick := ""
	for i, a := range argv {
		base := filepath.Base(a)
		switch base {
		case "python", "python3", "bash", "sh", "Rscript", "julia", "srun", "mpirun", "uv", "run", "-u", "-m":
			continue
		}
		if strings.HasPrefix(a, "-") && i > 0 {
			continue
		}
		pick = strings.TrimSuffix(base, filepath.Ext(base))
		break
	}
	pick = strings.Trim(nameUnsafe.ReplaceAllString(pick, "_"), "_")
	if pick == "" {
		return "job"
	}
	return pick
}

func submitCmd(g *globals) *cobra.Command {
	var sf slurmFlags
	var name, dir, remotePath string
	var fetch []string
	var noSync, detach, noFollow, asJSON bool
	cmd := &cobra.Command{
		Use:   "submit [OPTIONS] -- COMMAND [ARGS...]",
		Short: "Sync the project, run a command as a batch job, follow it, fetch results",
		Long: `Run one command as a batch job, the whole cycle in one go:

  1. copy the project to the cluster (sync rules from the config),
  2. check the usage rules ([policy]),
  3. submit a batch job that runs COMMAND in the project's directory there,
  4. print its output as it comes, and how it ended,
  5. copy back the paths named with --fetch (also when the job failed).

The project is the directory holding .cluster.toml (else the current one);
on the cluster it goes to remote_path from .cluster.toml, else a directory
of the same name in your home. Job output is kept in .cluster-logs/ there.

Without -t, and with no time limit in the config, the limit is suggested
from earlier runs with the same job name.

Ctrl+C stops following; the job keeps running (cluster logs -f / wait).

  cluster submit -c 4 -t 01:00:00 --fetch results/ -- python fit.py --all
  cluster submit --detach -- ./run.sh`,
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

			local := dir
			if local == "" {
				local = "."
				if e.Cfg.Project != nil {
					local = e.Cfg.Project.Dir()
				}
			}
			localAbs, err := filepath.Abs(local)
			if err != nil {
				return err
			}
			if remotePath == "" {
				remotePath = e.Cfg.RemotePathFor(localAbs)
			}
			req.Name = name
			if req.Name == "" {
				req.Name = e.Cfg.JobName(jobKind(args))
			}
			line := args[0]
			if len(args) > 1 {
				line = remote.Join(args...)
			}

			if err := e.R.EnsureMaster(); err != nil {
				return err
			}
			say := func(format string, a ...any) {
				if !asJSON {
					fmt.Fprintf(os.Stderr, format, a...)
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if req.Time == "" && !cmd.Flags().Changed("time") {
				if hist, err := e.Slurm.Accounting(ctx, nil, "now-30days"); err == nil {
					if s, ok := slurm.SuggestLimit(hist, req.Name, 10, 1.5); ok {
						req.Time = slurm.FormatDuration(s.Limit)
						say("Time limit %s, suggested from %s of %s (longest %s).\n",
							req.Time, slurm.Plural(s.Runs, "earlier run"), req.Name, slurm.HumanDuration(s.Longest))
					}
				}
			}
			if err := e.CheckRequest(ctx, req); err != nil {
				return err
			}

			if !noSync {
				say("Syncing %s -> %s:%s\n", localAbs, e.Cfg.Host, remotePath)
				if err := pushProject(e, localAbs, remotePath, asJSON); err != nil {
					return fmt.Errorf("sync: %w", err)
				}
			} else {
				_, _ = e.R.Output(ctx, "mkdir -p "+remoteDirExpr(remotePath+"/"+logsDir), nil)
			}

			// The output path is relative to the remote home, where sbatch runs.
			outRel := strings.TrimPrefix(remotePath, "~/") + "/" + logsDir + "/%x_%j.out"
			script := "#!/bin/bash -l\n" +
				"# Submitted by cluster submit.\n" +
				"cd " + remoteDirExpr(remotePath) + " || exit 1\n" +
				line + "\n"
			id, err := e.Slurm.Submit(ctx, req, script, "--output="+outRel)
			if err != nil {
				return err
			}
			outPath := strings.NewReplacer("%x", req.Name, "%j", id).Replace(outRel)
			if !strings.HasPrefix(outPath, "/") {
				home, herr := e.R.Run(ctx, "printenv", "HOME")
				if herr == nil {
					outPath = strings.TrimSpace(home) + "/" + outPath
				}
			}
			_ = session.DefaultLogCache().Remember(map[string]session.LogPath{id: {Path: outPath, Name: req.Name, Seen: time.Now()}})

			if asJSON {
				_ = jsonLine(map[string]any{"type": "submitted", "id": id, "name": req.Name, "remote_dir": remotePath, "output": outPath, "request": req.Args()})
			} else {
				fmt.Fprintf(os.Stderr, "Submitted job %s (%s): %s\nOutput: %s\n", id, req.Name, req, outPath)
			}
			if detach {
				if !asJSON {
					fmt.Fprintf(os.Stderr, "Follow it with: cluster logs -f %s\n", id)
				}
				return nil
			}

			// Follow (or just wait), then report and fetch.
			var interrupted bool
			if noFollow {
				err = runWaitQuiet(e, id)
				interrupted = err == errInterrupted
			} else {
				var out io.Writer = os.Stdout
				var lw *jsonLineWriter
				if asJSON {
					lw = &jsonLineWriter{}
					out = lw
				}
				interrupted, err = followOutput(e, id, outPath, -1, out)
				if lw != nil {
					lw.Flush()
				}
			}
			if err != nil && err != errInterrupted {
				return err
			}
			if interrupted {
				say("\nStopped following; job %s keeps running (cluster logs -f %s, cluster wait %s).\n", id, id, id)
				return nil
			}
			acc := finalStateWait(e, id)
			if acc != nil {
				if asJSON {
					_ = jsonLine(map[string]any{"type": "end", "job": accountingJSON(*acc)})
				} else {
					fmt.Fprintln(os.Stderr, endedText(*acc))
				}
			}
			if len(fetch) > 0 {
				say("Fetching %s\n", strings.Join(fetch, ", "))
				if err := fetchPaths(e, localAbs, remotePath, fetch); err != nil {
					return fmt.Errorf("fetch: %w", err)
				}
				if asJSON {
					_ = jsonLine(map[string]any{"type": "fetched", "paths": fetch})
				}
			}
			if acc != nil && acc.Failed() {
				return errJobFailed
			}
			return nil
		},
	}
	sf.register(cmd)
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "job name (default: the job prefix plus the script's name)")
	f.StringVar(&dir, "dir", "", "local project directory (default: the one holding .cluster.toml, else the current one)")
	f.StringVar(&remotePath, "remote-path", "", "where the project goes on the cluster (default: remote_path, else same name in home)")
	f.StringArrayVar(&fetch, "fetch", nil, "path in the project to copy back when the job ends (repeatable)")
	f.BoolVar(&noSync, "no-sync", false, "don't copy the project first")
	f.BoolVar(&detach, "detach", false, "return after submitting")
	f.BoolVar(&noFollow, "no-follow", false, "wait quietly instead of printing the output")
	f.BoolVar(&asJSON, "json", false, "one JSON object per line: submitted, line..., end, fetched")
	f.SetInterspersed(false)
	return cmd
}

// finalStateWait reads the job's final record, giving sacct a few seconds
// to catch up after squeue lets go of the job.
func finalStateWait(e *lifecycle.Env, id string) *slurm.Accounting {
	for i := 0; i < 6; i++ {
		if a := finalState(e, id); a != nil && a.Final() {
			return a
		}
		time.Sleep(5 * time.Second)
	}
	return finalState(e, id)
}
