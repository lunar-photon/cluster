# cluster

A unified High-Performance Computing (HPC) developer suite and interactive Terminal User Interface (TUI) bridging remote Slurm/OpenHPC clusters with your local workstation.

cluster turns remote HPC infrastructure into an intuitive extension of your laptop:
- Modern Interactive TUI Dashboard: Browse, launch, monitor, and configure cluster tools with a responsive keyboard- and mouse-driven terminal interface.
- Complete Workspace Mode (`cluster dev`): Mounts cluster storage locally, opens Dolphin, and connects an interactive compute or GPU shell. When you close the terminal or type exit, it automatically unmounts the drive and frees the compute node.
- Interactive Compute Shells (`cluster shell`): Jump directly into a dedicated Slurm node (CPU or GPU) with an exclusive bash prompt. Code runs and occupies resources interactively until you close the session.
- Seamless Local Drive Mount (`cluster mount` / `cluster unmount`): Uses SSHFS to mount your cluster home directly into your local file manager. Copy, paste, and view plots without manual downloads.
- Automated Jupyter Tunneling (`cluster jupyter`): Auto-discovers free ports, sets up secure multi-hop SSH tunnels, extracts session tokens, and launches your default browser.
- Smart Code Synchronization (`cluster sync`): Push and pull codebase changes rapidly with rsync, automatically ignoring heavy artifacts (.git, virtualenvs, __pycache__, checkpoints).
- Live Cluster Monitor: Real-time Slurm job queue inspection and partition availability tables built right into the TUI and CLI.

---

## Interactive TUI Application

Simply running `cluster` in any terminal opens the full-screen interactive TUI dashboard:

```bash
cluster
```

(Or explicitly with `cluster tui`).

### What You Can Do in the TUI:
- Workspace & Actions: One-click buttons to launch Dev Workspaces (CPU/GPU), interactive shells, JupyterLab instances, toggle local drive mounts, and sync files.
- Cluster Monitor: Live tables displaying your active/queued Slurm jobs and cluster partition availability (`short`, `medium`, `gpu`) with an instant Refresh button.
- Visual Settings Editor: Configure your cluster host, Slurm accounts, partitions, CPU counts, memory limits, and mount directories without touching config files manually.
- Keyboard Shortcuts:
  - `q`: Quit TUI
  - `r`: Refresh cluster status
  - `d`: Launch Dev Workspace
  - `j`: Launch JupyterLab
  - `m`: Toggle drive mount

When you launch an action from the TUI, the interface smoothly suspends and hands over full terminal control to your interactive compute session or Jupyter session. When you exit, the dashboard automatically re-appears with updated states.

---

## Requirements

### Local Machine
- Linux or macOS
- Python 3.8+
- OpenSSH client
- sshfs (for drive mounting)
- rsync (for code syncing)
- textual (installed automatically by install.sh)
- jupyter-forward (installed automatically by install.sh)

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

## CLI Commands

For scripts, automation, or direct terminal usage, all actions can also be executed as CLI subcommands:

### 1. All-in-One Dev Workspace (`cluster dev`)
Mounts `~/Cluster`, opens Dolphin file manager, and connects to a dedicated compute node shell. When you close the window or type exit, it automatically unmounts the drive and frees the compute node:

```bash
cluster dev -b
# Or with GPU
cluster dev --gpu -b
```

### 2. Interactive Compute Shell (`cluster shell`)
Borrow an exclusive compute node for interactive coding without mounting:

```bash
cluster shell
# Or GPU
cluster shell --gpu
# Customize resources
cluster shell -c 8 -m 32G -t 02:00:00
```

### 3. Local Storage Mount (`cluster mount` / `cluster unmount`)
Mount remote cluster files directly into your local filesystem:

```bash
cluster mount -b     # Mount and open in Dolphin
cluster unmount      # Safely detach
```

### 4. Remote JupyterLab (`cluster jupyter`)
Launch a visual JupyterLab session with zero manual port management:

```bash
cluster jupyter            # Head node
cluster jupyter --slurm    # Compute node
cluster jupyter --gpu      # GPU node
```

### 5. Fast Code Sync (`cluster sync`)
Push or pull changes rapidly:

```bash
cluster sync push . ~/projects/mycode
cluster sync pull ~/projects/mycode .
```

### 6. Cluster Status Dashboard (`cluster status`)
Check your job queue and cluster health in plain text:

```bash
cluster status
```

### 7. One-off Compute Execution (`cluster exec`)
Dispatch a single command to an allocated compute node:

```bash
cluster exec --cpus 8 "python train.py --epochs 50"
```

---

## Command Reference

```text
Usage: cluster [command] [OPTIONS]

Commands:
  tui                     Open the full-screen interactive TUI dashboard (default when no args)
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
