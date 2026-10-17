# cluster

A unified High-Performance Computing (HPC) developer suite bridging remote Slurm/OpenHPC clusters with your local workstation.

cluster turns remote HPC infrastructure into an intuitive extension of your laptop:
- Launch remote JupyterLab sessions with automatic port tunneling and browser opening.
- Mount cluster storage locally so you can copy-paste files, drag-and-drop datasets, and edit code natively in local editors.
- Borrow dedicated compute or GPU nodes interactively to run code instantly without waiting for batch job output logs.
- Automatic session lifecycle: closing your terminal window or typing exit automatically unmounts drives, cancels Slurm allocations, and releases compute resources.
- Synchronize project code bi-directionally in milliseconds.
- Monitor active Slurm queues, running jobs, and partition availability from your local terminal.

---

## Features

- Complete Workspace Mode (`cluster dev`): Mounts cluster storage locally, opens Dolphin, and connects an interactive compute or GPU shell. When you close the terminal or type exit, it automatically unmounts the drive and frees the compute node.
- Interactive Compute Shells (`cluster shell`): Jump directly into a dedicated Slurm node (CPU or GPU) with an exclusive bash prompt. Code runs and occupies resources interactively until you close the session.
- Seamless Local Drive Mount (`cluster mount` / `cluster unmount`): Uses SSHFS to mount your cluster home directly into your local file manager. Copy, paste, and view plots without manual downloads.
- Automated Jupyter Tunneling (`cluster jupyter`): Auto-discovers free ports, sets up secure multi-hop SSH tunnels, extracts session tokens, and launches your default browser.
- Smart Code Synchronization (`cluster sync`): Push and pull codebase changes rapidly with rsync, automatically ignoring heavy artifacts (.git, virtualenvs, __pycache__, checkpoints).
- Instant Status Dashboard (`cluster status`): View your queued jobs, remaining allocation runtimes, and cluster partition health in real time.
- Configurable Defaults: Store frequent settings (cluster host, Slurm accounts, default partitions) in ~/.config/cluster/config.

---

## Requirements

### Local Machine
- Linux or macOS
- Python 3.8+
- OpenSSH client
- sshfs (for drive mounting)
- rsync (for code syncing)

### Remote Cluster
- SSH access (configured in ~/.ssh/config or via key authentication)
- Slurm job scheduler (for compute node allocations and status checks)
- JupyterLab installed (for Jupyter tunneling)

---

## Installation

Clone this repository and run the installer:

```bash
git clone https://github.com/lunar-photon/cluster.git
cd cluster
chmod +x install.sh
./install.sh
```

Ensure `~/.local/bin` is in your PATH. If not, add the following to your `~/.bashrc` or `~/.zshrc`:

```bash
export PATH="$HOME/.local/bin:$PATH"
```

---

## First Run and Configuration

When you run `cluster` for the first time without arguments:

```bash
cluster
```

It welcomes you and prints a full capability guide with examples.

### Setting Up Your Cluster Profile

Copy the template configuration to your user config directory:

```bash
mkdir -p ~/.config/cluster
cp config.example ~/.config/cluster/config
```

Edit `~/.config/cluster/config`:

```bash
# Cluster SSH host (hostname or ~/.ssh/config alias)
HOST="cluster.university.edu"

# Default local port for JupyterLab
PORT=8888

# Default Slurm compute settings
SLURM_PARTITION="medium"
SLURM_ACCOUNT="myaccount"
SLURM_QOS="standard_qos"
SLURM_CPUS=4
SLURM_MEM="8G"
SLURM_TIME="04:00:00"

# Slurm GPU settings
GPU_PARTITION="gpu"
GPU_QOS="gpu_qos"

# Local mount directory
MOUNT_POINT="$HOME/Cluster"
```

---

## Commands and Usage

### 1. All-in-One Dev Workspace (`cluster dev`)
The fastest way to work on the cluster:

```bash
# Mounts ~/Cluster, opens Dolphin file manager, and gives you a dedicated compute shell
cluster dev -b

# Or request a GPU node
cluster dev --gpu -b
```
Automatic Cleanup on Terminal Close:
When you close the terminal window or type `exit`:
1. It automatically unmounts the `~/Cluster` local drive cleanly.
2. It automatically releases the allocated Slurm compute/GPU node so you do not waste cluster allocation hours.
3. It closes all tunnels and background sessions.

### 2. Interactive Compute Shell (`cluster shell`)
Borrow an exclusive compute node for interactive coding, testing, and debugging without mounting:

```bash
# Request standard compute node (uses your config defaults: 4 CPUs, 8GB RAM)
cluster shell

# Request a dedicated GPU node
cluster shell --gpu

# Customize resource request on the fly
cluster shell -c 8 -m 32G -t 02:00:00
```
This drops you straight into a terminal prompt running on the compute node. Type `exit` or close the terminal window to release the node immediately.

### 3. Local Storage Mount (`cluster mount` / `cluster unmount`)
Mount remote cluster files directly into your local filesystem:

```bash
# Mount remote home directory to ~/Cluster and open your file manager
cluster mount -b

# Detach the mount cleanly when finished
cluster unmount
```
Once mounted, you can copy-paste files using standard keyboard shortcuts (Ctrl+C / Ctrl+V), drag-and-drop datasets, or open generated figures directly in your local PDF/image viewer.

### 4. Remote JupyterLab (`cluster jupyter`)
Launch a full visual JupyterLab session with zero manual port management:

```bash
# Instant launch on head node
cluster jupyter

# Dedicated Slurm compute node launch
cluster jupyter --slurm

# Dedicated Slurm GPU node launch
cluster jupyter --gpu

# Start in a specific directory
cluster jupyter -d ~/projects/cosmology
```

### 5. Fast Code Sync (`cluster sync`)
Synchronize local code changes to the cluster without copying huge caches or git history:

```bash
# Push current local folder to remote directory
cluster sync push . ~/projects/mycode

# Pull remote changes back to local machine
cluster sync pull ~/projects/mycode .
```

### 6. Cluster Status Dashboard (`cluster status`)
Check your job queue and cluster health instantly from your local terminal:

```bash
cluster status
```

### 7. One-off Compute Execution (`cluster exec`)
Dispatch a single command to an allocated compute node without writing a job submission script:

```bash
cluster exec --cpus 8 "python train.py --epochs 50"
```

---

## Command Reference

```text
Usage: cluster <command> [OPTIONS]

Commands:
  dev [OPTIONS]           All-in-one workspace (mount + compute shell + auto-cleanup)
  shell [OPTIONS]         Allocate an interactive compute/GPU shell (auto-freed on close)
  mount [OPTIONS]         Mount cluster filesystem locally via SSHFS
  unmount                 Safely unmount the local cluster drive
  jupyter [OPTIONS]       Launch remote JupyterLab with automated tunneling
  sync <push|pull>        Fast bi-directional code sync via rsync
  status                  View active Slurm jobs, queues, and node availability
  exec "<command>"        Run a command directly inside an allocated compute node
  config                  View or edit your local cluster configuration
```

---

## License

MIT License. See LICENSE for details.
