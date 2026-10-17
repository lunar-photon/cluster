# cluster-jupyter

Seamlessly launch remote JupyterLab sessions on an HPC cluster and automatically tunnel them into your local web browser.

cluster-jupyter automates remote Jupyter execution on Slurm and OpenHPC clusters. It eliminates manual port forwarding, SSH tunnel setups, token copy-pasting, and batch job script boilerplate.

---

## Features

- Zero-configuration tunneling: Finds free ports on your local machine and forwards the remote session automatically.
- Automatic browser launch: Extracts the Jupyter authentication token and opens your default browser immediately.
- Interactive Slurm integration: Allocate compute nodes or GPU resources with simple flags (--slurm, --gpu) without writing job scripts.
- Configurable defaults: Store frequently used cluster hosts, partitions, and allocations in a local config file (~/.config/cluster-jupyter/config).
- Headless cluster support: Works on standard SSH-accessible clusters without requiring administrative web portals (such as Open OnDemand or JupyterHub).

---

## Requirements

### Local Machine
- Linux or macOS
- Python 3.8+
- OpenSSH client
- jupyter-forward (automatically installed by install.sh)

### Remote Cluster
- SSH access (configured in ~/.ssh/config or via key authentication)
- JupyterLab installed on the cluster (e.g. via Anaconda or module load anaconda3)

---

## Installation

Clone this repository and run the installer:

```bash
git clone https://github.com/lunar-photon/cluster-jupyter.git
cd cluster-jupyter
chmod +x install.sh
./install.sh
```

Ensure `~/.local/bin` is in your PATH. If it is not, add the following to your `~/.bashrc` or `~/.zshrc`:

```bash
export PATH="$HOME/.local/bin:$PATH"
```

---

## Configuration

You can define your default cluster host and Slurm settings so you do not have to type them every time.

Copy the example configuration:

```bash
mkdir -p ~/.config/cluster-jupyter
cp config.example ~/.config/cluster-jupyter/config
```

Edit `~/.config/cluster-jupyter/config`:

```bash
# Cluster SSH host (hostname or ~/.ssh/config Host alias)
HOST="cluster.university.edu"

# Default local port (will auto-increment if in use)
PORT=8888

# Slurm parameters for --slurm
SLURM_PARTITION="medium"
SLURM_ACCOUNT="myaccount"
SLURM_QOS="myqos"
SLURM_CPUS=4
SLURM_MEM="8G"

# Slurm parameters for --gpu
GPU_PARTITION="gpu"
GPU_QOS="gpu_qos"
```

---

## Usage

### 1. Head Node Launch
Connect to your saved default cluster host and start JupyterLab instantly:

```bash
cluster-jupyter
```

Or specify a different host directly:

```bash
cluster-jupyter -H mycluster.university.edu
```

### 2. Slurm Compute Node Allocation
Allocate a compute node via Slurm and launch JupyterLab inside the job:

```bash
cluster-jupyter --slurm
```

### 3. Slurm GPU Node Allocation
Allocate a GPU node and launch JupyterLab:

```bash
cluster-jupyter --gpu
```

### 4. Custom Notebook Directory
Open directly into a specific project folder on the cluster:

```bash
cluster-jupyter -d ~/projects/cosmology_runs
```

### 5. Custom Slurm Flags
Pass arbitrary srun / sbatch commands:

```bash
cluster-jupyter -c "srun -p test -c 8 --mem=16G -t 02:00:00"
```

---

## Command Line Options

```text
Usage: cluster-jupyter [OPTIONS]

Options:
  -H, --host HOST        Cluster SSH host or ~/.ssh/config alias
  -s, --slurm            Launch inside a Slurm CPU allocation
  -g, --gpu              Launch inside a Slurm GPU allocation
  -A, --account ACCT     Slurm project/account name
  -q, --qos QOS          Slurm Quality of Service (QOS)
  -p, --port PORT        Local port to forward to (default: 8888, auto-increments if busy)
  -d, --dir DIR          Starting directory for Jupyter notebooks on remote host
  -c, --custom "CMD"     Custom launch command (overrides automatic srun flags)
  -h, --help             Show this help message and exit
```

---

## License

MIT License. See LICENSE for details.
