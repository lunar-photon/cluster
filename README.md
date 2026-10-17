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

`make dist` builds static binaries for Linux and macOS (amd64 and arm64).

## Commands

| Command | What it does |
|---|---|
| `cluster` | Opens the dashboard (when run in a terminal) |
| `cluster info [PARTITION]` | Partitions, node hardware, free CPUs/GPUs, and what your account and QOS let you use |
| `cluster terminal [-g]` | **Node terminal**: allocates a node (`-g`: GPU node) and opens a shell on it |
| `cluster terminal --mount [-g] [-b]` | **Node terminal + cluster mount**: also mounts storage at `mount_point`; `-b` opens it in your file manager |
| `cluster jupyter [-g]` | **Jupyter session on node**: JupyterLab on a compute node, opened in your browser |
| `cluster attach [JOBID]` | Reconnects to a running session |
| `cluster ps` | Lists sessions this tool started and whether they are still running |
| `cluster stop JOBID \| --all` | Cancels the job, closes tunnels, unmounts |
| `cluster gc` | Cleans up ended sessions and lists leftover `cluster:*` jobs |
| `cluster setup` | Checks the cluster and offers to install tmux there |
| `cluster mount` / `unmount` | Mounts storage with sshfs until you unmount it |
| `cluster sync push\|pull [LOCAL] [REMOTE]` | rsync, skipping `.git`, venvs and caches |
| `cluster exec CMD...` | Runs one command on a compute node with `srun` |
| `cluster status` | Your jobs, then the same partition table as `cluster info` |
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

1. **Actions:** start a node terminal (with or without the cluster mount) or a Jupyter session, mount storage, sync code. Below the list, what each button will request and whether it should start right away or queue.
2. **Cluster:** the partition table above with free-capacity bars. The selected partition's full detail appears underneath; `t` starts a node terminal there and `j` a Jupyter session, adding the QOS the partition needs and a GPU on GPU partitions.
3. **Jobs:** your jobs with CPUs, memory, GPUs, time used and limit.
4. **Sessions:** sessions this tool started; attach, stop or clean up.
5. **Settings:** edit the config. Blank fields use the cluster's defaults.

It refreshes every 30 seconds (`r` for now).

## Sessions and network drops

`terminal` and `jupyter` submit a placeholder batch job and record its ID in `~/.local/state/cluster/sessions.json` before doing anything else. What happens next depends on how the session ends:

- **You type `exit`.** The job is cancelled with `scancel`, and the tool waits until Slurm stops listing it. Anything the session mounted is unmounted, unless another running session still uses the mount.
- **You close the terminal.** If a program is still running in the shell (a training run, an editor), the job is kept and you get a desktop notification saying so; `cluster attach` returns to it. If the shell is idle, the job is released as with `exit`. Notices also go to `~/.local/state/cluster/events.log`.
- **The network drops.** SSH keepalives tolerate about 45 seconds of silence, so a short outage does not disconnect you. After a longer one, the job keeps running, and `cluster` retries with backoff until the cluster is reachable, then reattaches. Press Ctrl+C to stop retrying; the job stays up and `cluster attach` resumes it later.
- **You detach from tmux (Ctrl+b d).** The job keeps running. Use `cluster attach` to come back or `cluster stop` to release it.

With tmux on the cluster, the shell runs inside a tmux session started by the batch job, so reattaching after a drop returns you to the same shell with your programs still running. If you don't have your own tmux config, the session also gets mouse scrolling and a status bar showing the job ID and the detach key. Without tmux, a reattach opens a new shell on the same node, and programs started in the old shell are gone. Pass `--no-tmux` to opt out.

Jupyter runs as its own job, so kernels survive a drop and only the local tunnel restarts. Ctrl+C in the `cluster jupyter` terminal stops the server and releases the node, but asks first if kernels are open, since their variables would be lost (saved notebooks are kept). Closing the terminal keeps Jupyter running while kernels are open; `cluster attach` reopens the tunnel. The token is passed to Jupyter in the environment and never appears on a command line that other users could see with `ps`.

A job still ends at its time limit (`time` in the config, or `-t`), whatever is running in it.

All ssh, rsync and sshfs calls share one OpenSSH connection (`ControlMaster`). A password or 2FA prompt appears once, and later commands reuse the login for 10 minutes after their last use. The Jupyter tunnel uses its own connection, so on password-only clusters it asks again.

## Configuration

`~/.config/cluster/config.toml` (run `cluster config` to edit it):

```toml
# Example values; `cluster init` fills in your cluster's.
host = "hpc.example.edu"        # or user@host, or a Host alias from ~/.ssh/config
mount_point = "~/Cluster"
default_profile = "cpu"

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
```

Anything left out uses the cluster's own default: with no `partition`, jobs go to the default partition; with no `mem` or `time`, the partition's default memory and time limit apply.

Command-line flags override environment variables (`CLUSTER_HOST`, `CLUSTER_PROFILE`), which override the profile, which overrides `[slurm]`. The file is parsed as TOML and never executed.

If you used the earlier bash version, its `~/.config/cluster/config` is read until you run `cluster config migrate`.

## Requirements

On your machine: OpenSSH, plus `sshfs` for mounting (macFUSE on macOS) and `rsync` for syncing.

On the cluster: Slurm 20.11 or newer for `srun --overlap` (on older versions pass `--no-overlap`), and Jupyter on the `PATH` after your `[jupyter] setup` command runs. tmux is optional but recommended; `cluster setup` can install it. Home directories must be shared between login and compute nodes, which is the usual setup.

## Development

```bash
make test       # go vet + go test -race
```

The lifecycle tests run against fake `ssh`, `sbatch`, `squeue`, `srun` and `scancel` scripts (`internal/lifecycle/*_test.go`). They cover normal exit, connection loss and reattach, tmux detach, closing the terminal on a busy and an idle node terminal, a job ending while disconnected, `gc`, a Jupyter tunnel being killed and restored, and Jupyter's kernel checks on Ctrl+C and terminal close. The cluster-information code is tested against synthetic `scontrol`/`sacctmgr` output covering idle, busy, down and drained nodes, GPU partitions that require a QOS, partitions you can't use, and QOS limits.

The earlier bash + C++ implementation is in `legacy/`.

## License

MIT
