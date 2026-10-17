#!/usr/bin/env bash
#
# install.sh - Installer for cluster HPC suite
# Compiles C++ TUI binary and installs cluster executables.
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_DIR="${HOME}/.local/bin"
VENV_DIR="${HOME}/.venvs/jupyter-forward"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/cluster"

echo "=========================================="
echo "Installing cluster HPC Developer Suite"
echo "=========================================="

mkdir -p "$INSTALL_DIR"
mkdir -p "$CONFIG_DIR"

# 1. Compile C++ TUI application
echo "Building C++ cluster-tui binary..."
cmake -B "$SCRIPT_DIR/build" -DCMAKE_BUILD_TYPE=Release "$SCRIPT_DIR"
cmake --build "$SCRIPT_DIR/build" -j"$(nproc)"

cp -f "$SCRIPT_DIR/build/cluster-tui" "$INSTALL_DIR/cluster-tui"
chmod +x "$INSTALL_DIR/cluster-tui"
echo "Installed cluster-tui to $INSTALL_DIR/cluster-tui"

# 2. Install main cluster script
cp -f "$SCRIPT_DIR/cluster" "$INSTALL_DIR/cluster"
chmod +x "$INSTALL_DIR/cluster"
echo "Installed cluster to $INSTALL_DIR/cluster"

# 3. Create backward-compatibility alias
cat << 'EOF' > "$INSTALL_DIR/cluster-jupyter"
#!/usr/bin/env bash
exec cluster jupyter "$@"
EOF
chmod +x "$INSTALL_DIR/cluster-jupyter"

# 4. Install backend engine for JupyterLab tunneling
if ! command -v jupyter-forward >/dev/null 2>&1 && [[ ! -x "$VENV_DIR/bin/jupyter-forward" ]]; then
    echo "Setting up virtual environment for jupyter-forward engine in $VENV_DIR..."
    python3 -m venv "$VENV_DIR"
    "$VENV_DIR/bin/pip" install --upgrade pip
    "$VENV_DIR/bin/pip" install jupyter-forward
fi

# 5. Create template config if none exists
if [[ ! -f "$CONFIG_DIR/config" ]]; then
    cp -f "$SCRIPT_DIR/config.example" "$CONFIG_DIR/config.example"
    echo "Sample configuration created at $CONFIG_DIR/config.example"
    echo "Copy it to $CONFIG_DIR/config and customize with your cluster host and Slurm details."
fi

echo "=========================================="
echo "Installation complete!"
echo "Make sure $INSTALL_DIR is in your PATH."
echo "Run 'cluster' to launch the C++ TUI dashboard."
echo "=========================================="
