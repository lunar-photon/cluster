# cluster

`cluster` lets you work on a Slurm cluster from your laptop. It has three ways to work on a compute node:

- **Node terminal:** a shell on a compute node.
- **Node terminal + cluster mount:** the same shell, with cluster storage mounted on your laptop for the session, so you can edit files with local tools while running them on the node.
- **Jupyter session on node:** JupyterLab on a compute node, tunneled to your browser.

All three run as Slurm jobs on compute nodes, never on the login node. It also shows what the cluster has and what you can use (node hardware, free cores and GPUs per partition, your QOS limits), syncs code with rsync, and has a terminal dashboard.

Nothing about a particular cluster is built in: host, account, partitions and QOS come from your config, and `cluster init` discovers them from the cluster.

It is a single Go binary with no runtime dependencies beyond OpenSSH. Mounting needs `sshfs` and syncing needs `rsync`.

## Install

```bash
git clone https://github.com/lunar-photon/cluster.git
cd cluster
./install.sh          # builds with Go 1.23+, installs ~/.local/bin/cluster, runs `cluster setup`
cluster init          # first time only: asks the cluster which accounts, QOS and partitions you can use
```

An existing `cluster`, `cluster-tui` or `cluster-jupyter` in `~/.local/bin` is moved to `~/.local/share/cluster/previous-install` rather than overwritten.

`cluster setup` checks the cluster's Slurm version and Jupyter. If tmux is missing, it explains what tmux is for (see below) and asks before installing it into `~/.local/bin` on the cluster, with no root. The login node only downloads the official tmux, libevent and ncurses releases and checks them against pinned SHA-256 sums; the build runs on a compute node as a short job (4 CPUs, 2 GB, usually 2-3 minutes). libevent and ncurses are linked statically, so the 2 MB result runs on every node with the same OS. Run `cluster setup` again at any time, or `cluster setup --reinstall-tmux` to rebuild.

If JupyterLab isn't available, `cluster setup` offers to install it the same way, with nothing needed from the cluster's internet:

1. A short job finds a Python 3.9+ that runs on the compute nodes, on the default path or from a `python/*` environment module. Login nodes often have different Pythons than compute nodes.
2. Your computer downloads JupyterLab for that Python and platform (about 50 MB of wheels) and copies it over.
3. A compute node installs it into `~/.local/share/cluster/jupyter`.
4. `[jupyter] setup` is set to load the module (if any) and activate that environment, unless you've set it yourself.

Add packages for your notebooks with `--with numpy,matplotlib`; compute nodes usually can't install packages from inside a notebook. `--install-jupyter` installs it even if a Jupyter is already there. Your computer needs `pip` for the download.

The first JupyterLab start on a node can take a few minutes while it reads its files off the network filesystem; `cluster jupyter` waits, says so, and opens the browser once it answers.

`make dist` builds static binaries for Linux and macOS (amd64 and arm64).

## Commands

| Command | What it does |
|---|---|
| `cluster` | Opens the dashboard (when run in a terminal) |
| `cluster info [PARTITION]` | Partitions, node hardware, free CPUs/GPUs, and what your account and QOS let you use |
| `cluster terminal [-g]` | **Node terminal**: allocates a node (`-g`: GPU node) and opens a shell on it |
| `cluster terminal --mount [-g] [-b]` | **Node terminal + cluster mount**: also mounts storage at `mount_point`; `-b` opens it in your file manager |
| `cluster jupyter [-g] [--renew]` | **Jupyter session on node**: JupyterLab on a compute node, opened in your browser; `--renew` carries it past the time limit |
| `cluster submit [OPTIONS] -- CMD...` | Syncs the project, runs CMD as a batch job, follows its output, fetches results |
| `cluster logs [JOBID] [-f]` | Shows or follows any job's output, with progress and last-output age |
| `cluster wait JOBID... \| --name GLOB` | Waits until jobs end; exits non-zero if any failed |
| `cluster attach [JOBID]` | Reconnects to a running session |
| `cluster ps` | Lists sessions this tool started and whether they are still running |
| `cluster stop JOBID \| --all` | Cancels the job, closes tunnels, unmounts |
| `cluster gc` | Cleans up ended sessions and lists leftover jobs `cluster` submitted |
| `cluster setup` | Checks the cluster and offers to install tmux and JupyterLab there |
| `cluster mount` / `unmount` | Mounts storage with sshfs until you unmount it |
| `cluster sync push\|pull [LOCAL] [REMOTE]` | rsync, skipping `.git`, venvs and caches |
| `cluster exec CMD...` | Runs one command on a compute node with `srun` |
| `cluster status [--since 1d] [--name GLOB]` | Your jobs with progress and suggested limits, recently ended ones, then the partition table |
| `cluster config [show\|migrate]` | Edits, prints or converts the config |

The old names still work: `cluster shell` is `cluster terminal`, and `cluster dev` is `cluster terminal --mount`.

Every command accepts `--dry-run` (`-n`), which prints the ssh, sbatch, rsync and sshfs commands instead of running them. Allocation flags are shared: `-P profile`, `-p partition`, `-A account`, `-q qos`, `-c cpus`, `-m mem`, `-t time`, `--gpus N`.

## Cluster information

`cluster info` (and the dashboard's Cluster tab) shows, for every partition:

- the hardware of each node: cores, sockets × cores per socket, threads per core, RAM, GPUs and their type
- how many nodes are idle, partly used, full or unavailable (down, draining, not responding), with their names
- free CPUs and GPUs, and the most cores a single-node job could get right now
- the time limit, the max nodes per job, and whether your account and QOS may use it
- **what's available to you now:** the free resources, capped by what your QOS still allows after your running jobs

At the top are your account, your QOS and their per-user limits, and what your jobs currently use. `cluster info medium` shows one partition in full, including why unavailable nodes are down.

On clusters where Slurm doesn't reserve memory per job, free memory is the amount the OS reports on nodes that still have free cores, and the tool says so.

When a node terminal starts, it prints the node's hardware and what your job was given, e.g. `Node: 28 cores (2 sockets × 14), 125 GB RAM · Your job: 4 CPUs, 8G memory`.

## Dashboard

`cluster` opens a dashboard with five tabs:

1. **Actions:** start a node terminal (with or without the cluster mount) or a Jupyter session, mount storage, sync code. `j` (Jupyter) first asks which partition to use, with each one's time limit and free cores, so you can pick one that's free right now. Below the list, what each button will request and whether it should start right away or queue.
2. **Cluster:** the partition table above with free-capacity bars. The selected partition's full detail appears underneath; `t` starts a node terminal there and `j` a Jupyter session, adding the QOS the partition needs, a GPU on GPU partitions, and a shorter time limit if your usual one is longer than the partition allows.
3. **Jobs:** your jobs with cores, time used and limit, how long ago each last wrote output, and progress with an ETA. The selected job's latest output appears underneath; `l` follows it (Ctrl+C returns), `/` filters by name.
4. **Sessions:** sessions this tool started; attach, stop or clean up.
5. **Settings:** edit the config. Blank fields use the cluster's defaults.

It refreshes every 30 seconds (`r` for now).

## Batch jobs

`cluster submit` runs one command as a batch job and handles the whole cycle:

```bash
cluster submit -c 4 -t 01:00:00 --fetch results/ -- python fit.py --all
```

1. It copies the project to the cluster with the `[sync]` excludes. The project is the directory holding `.cluster.toml` (see below), else the current one. It goes to `remote_path` from `.cluster.toml`, else a directory of the same name in your remote home.
2. It checks the request against the partition and the usage rules.
3. It submits a batch job that runs the command in that directory. The job is named after the script (`fit` here, after the job prefix) and its output goes to `.cluster-logs/` there.
4. It prints the output as it comes, then how the job ended.
5. It copies back the `--fetch` paths, even after a failure.

Ctrl+C stops following; the job keeps running. `--detach` returns right after submitting, and `--no-follow` waits quietly. Without `-t` and with no time limit in the config, the limit comes from earlier runs with the same job name.

Before anything is submitted (by `submit`, `terminal`, `jupyter`, `exec` or `setup`), the request is checked against its partition. A time limit longer than the partition allows, or a partition that doesn't exist, is refused with the fix instead of leaving a job queued that Slurm would never start.

## Following jobs

These work for any of your jobs, including ones another tool submitted with `sbatch`:

- `cluster logs JOBID` prints the job's state, how long ago it last wrote output, its progress, and its last lines. `-f` follows the output until the job ends, then prints how it ended and exits non-zero if it failed. Without a JOBID it picks your newest job (matching `--name`).
- `cluster wait JOBID...` (or `--name GLOB`) blocks until the jobs end, prints each one's state, exit code and run time from Slurm's accounting, and exits 1 if any failed (2 on `--timeout`). `--notify` adds a desktop notification.
- `cluster status` shows, for running jobs, when they last wrote output and their progress. `--since 1d` adds jobs that ended in that window, with exit code, run time against the limit, CPU efficiency and peak memory where the cluster records it.

**Progress.** If a job prints lines like `##progress 250/3733`, `[250/3733] ...` or tqdm's `250/3733 [`, the latest one gives a done/total count, a rate, and an estimated finish checked against the time limit: `250/3733 · 41 min left · limit OK` or `ETA past limit`. Only these forms count, so a date like 10/19 is never read as progress.

**Suggested time limits.** `status` suggests a limit for each job from earlier runs with the same name. It takes the longest of the last 10 runs that completed or timed out, adds 50%, and rounds up to 5 minutes. It flags jobs whose limit is shorter than that. Taking the longest rather than the latest run keeps a quick partial rerun from lowering the limit for a full run.

**Where output is.** Slurm forgets a job's output file a few minutes after it ends, and its accounting doesn't record it. `cluster` notes the path whenever it sees a job running, falls back to Slurm's default `slurm-<id>.out` in the job's directory, and takes `--path` otherwise.

## Scripts and agents

`status`, `info`, `ps`, `logs`, `wait` and `submit` take `--json`, with stable field names. With `-f`, `logs` and `submit` emit one JSON object per line. Without a terminal, nothing prompts. `init --yes` accepts the detected values, `setup --yes` installs tmux, node terminals point to `exec`/`submit`, and a Jupyter session stops on an interrupt instead of asking.

## Sessions and network drops

`terminal` and `jupyter` submit a placeholder batch job and record its ID in `~/.local/state/cluster/sessions.json` before doing anything else. What happens next depends on how the session ends:

- **You type `exit`.** The job is cancelled with `scancel`, and the tool waits until Slurm stops listing it. Anything the session mounted is unmounted, unless another running session still uses the mount.
- **You close the terminal.** If a program is still running in the shell (a training run, an editor), the job is kept and you get a desktop notification saying so; `cluster attach` returns to it. If the shell is idle, the job is released as with `exit`. Notices also go to `~/.local/state/cluster/events.log`.
- **The network drops.** SSH keepalives tolerate about 45 seconds of silence, so a short outage does not disconnect you. After a longer one, the job keeps running, and `cluster` retries with backoff until the cluster is reachable, then reattaches. Press Ctrl+C to stop retrying; the job stays up and `cluster attach` resumes it later.
- **You detach from tmux (Ctrl+b d).** The job keeps running. Use `cluster attach` to come back or `cluster stop` to release it.

With tmux on the cluster, the shell runs inside a tmux session started by the batch job, so reattaching after a drop returns you to the same shell with your programs still running. If you don't have your own tmux config, the session also gets mouse scrolling and a status bar showing the job ID and the detach key. Without tmux, a reattach opens a new shell on the same node, and programs started in the old shell are gone. Pass `--no-tmux` to opt out.

Jupyter runs as its own job, so kernels survive a drop and only the local tunnel restarts. Ctrl+C in the `cluster jupyter` terminal stops the server and releases the node, but asks first if kernels are open, since their variables would be lost (saved notebooks are kept). Closing the terminal keeps Jupyter running while kernels are open; `cluster attach` reopens the tunnel. The token is passed to Jupyter in the environment and never appears on a command line that other users could see with `ps`.

**Time limits.** Ten minutes before a Jupyter job's time limit, `cluster jupyter` warns in the terminal and with a desktop notification. With `cluster jupyter --renew`, fifteen minutes before the limit it submits a replacement job with the same token and settings. When the old job ends, the tunnel moves to the new one on the same local port, so the browser tab carries on at the same address. Saved notebooks carry over; kernel variables don't, because the old job's processes end. The replacement counts towards any usage rules, and if the queue is busy it may start a little after the old job ends. Renewal continues after `cluster attach`.

A job still ends at its time limit (`time` in the config, or `-t`), whatever is running in it.

All ssh, rsync and sshfs calls share one OpenSSH connection (`ControlMaster`). A password or 2FA prompt appears once, and later commands reuse the login for 10 minutes after their last use. The Jupyter tunnel uses its own connection, so on password-only clusters it asks again.

## Configuration

`~/.config/cluster/config.toml` (run `cluster config` to edit it):

```toml
# Example values; `cluster init` fills in your cluster's.
host = "hpc.example.edu"        # or user@host, or a Host alias from ~/.ssh/config
mount_point = "~/Cluster"
default_profile = "cpu"
job_prefix = "cluster:"         # starts the name of every job cluster submits

[slurm]                         # defaults shared by every profile
partition = "medium"
account = "mylab"
cpus = 4
mem = "8G"
time = "04:00:00"

[profiles.cpu]

[profiles.gpu]                  # only what differs from [slurm]
partition = "gpu"
qos = "gpu_qos"
gpus = 1
mem = "16G"

[jupyter]
port = 8888
setup = "module load anaconda3 && conda activate ml"

[sync]
exclude = [".git/", "__pycache__/", ".venv/", "*.ckpt"]

[policy]                        # optional usage rules, see below
max_cpus = 16
```

Anything left out uses the cluster's own default: with no `partition`, jobs go to the default partition; with no `mem` or `time`, the partition's default memory and time limit apply.

Command-line flags override environment variables (`CLUSTER_HOST`, `CLUSTER_PROFILE`), which override the profile, which overrides `[slurm]`. The file is parsed as TOML and never executed.

If you used the earlier bash version, its `~/.config/cluster/config` is read until you run `cluster config migrate`.

### Project settings and usage rules

A project can keep its own settings in a `.cluster.toml` at its root. `cluster` finds it from any subdirectory, applies it on top of your config while you work there, and never copies it into your config:

```toml
# .cluster.toml (example)
job_prefix  = "proj_"           # names of this project's jobs
remote_path = "projects/proj"   # where the project lives on the cluster

[slurm]                         # this project's defaults
cpus = 8
time = "01:00:00"

[sync]
exclude = [".git/", "data/raw/"]

[policy]                        # rules for everyone working here
job_prefix     = "proj_"        # every job name must start with this
partitions     = ["batch", "short"]
max_jobs       = 3              # active jobs at once (with this prefix)
max_cpus       = 8              # per job
max_total_cpus = 24             # across active jobs (with this prefix)
```

Every command that submits work checks the rules first and refuses with a message naming the rule, e.g. `policy: at most 3 proj_* jobs at once; 3 active (2 running, 1 pending)`. A `[policy]` in your own config applies everywhere. `cluster info` and the dashboard's Cluster tab show the rules in force and where they come from, so they're visible before anything is submitted. Jobs submitted by other tools don't go through these checks.

**Job names.** Every job `cluster` submits carries a Slurm comment marking it as such, and `cluster gc` finds leftovers by that comment. So a project prefix shared with jobs from other tools never makes those jobs look like leftovers.

## Requirements

On your machine: OpenSSH, plus `sshfs` for mounting (macFUSE on macOS) and `rsync` for syncing.

On the cluster: Slurm 20.11 or newer for `srun --overlap` (on older versions pass `--no-overlap`), and Jupyter on the `PATH` after your `[jupyter] setup` command runs. tmux is optional but recommended; `cluster setup` can install both. Home directories must be shared between login and compute nodes, which is the usual setup.

## Development

```bash
make test       # go vet + go test -race
```

The lifecycle tests run against fake `ssh`, `sbatch`, `squeue`, `srun` and `scancel` scripts (`internal/lifecycle/*_test.go`). They cover normal exit, connection loss and reattach, tmux detach, closing the terminal on a busy and an idle node terminal, a job ending while disconnected, `gc`, a Jupyter tunnel being killed and restored, and Jupyter's kernel checks on Ctrl+C and terminal close. `internal/lifecycle/policy_test.go` covers every usage rule and the partition time-limit check. The job-following code is tested on synthetic `scontrol`, `sacct` and log output: output paths with `%j`/`%x`, step CPU time folded into jobs, the progress forms and ETA, and the time-limit suggestion. The cluster-information code is tested against synthetic `scontrol`/`sacctmgr` output covering idle, busy, down and drained nodes, GPU partitions that require a QOS, partitions you can't use, and QOS limits.

The earlier bash + C++ implementation is in `legacy/`.

## License

MIT
