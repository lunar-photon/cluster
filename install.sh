#!/usr/bin/env bash
#
# install.sh - Installer for cluster
#

set -euo pipefail

INSTALL_DIR="${HOME}/.local/bin"
VENV_DIR="${HOME}/.venvs/jupyter-forward"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/cluster"

echo "Installing cluster suite..."

mkdir -p "$INSTALL_DIR"
mkdir -p "$CONFIG_DIR"

# Install jupyter-forward in dedicated venv if not already installed
if ! command -v jupyter-forward >/dev/null 2>&1 && [[ ! -x "$VENV_DIR/bin/jupyter-forward" ]]; then
    echo "Setting up virtual environment for jupyter-forward in $VENV_DIR..."
    python3 -m venv "$VENV_DIR"
    "$VENV_DIR/bin/pip" install --upgrade pip
    "$VENV_DIR/bin/pip" install jupyter-forward
fi

# Copy cluster script
cp -f "$(dirname "$0")/cluster" "$INSTALL_DIR/cluster"
chmod +x "$INSTALL_DIR/cluster"

# Create cluster-jupyter backward compatibility wrapper
cat << 'EOF' > "$INSTALL_DIR/cluster-jupyter"
#!/usr/bin/env bash
exec cluster jupyter "$@"
EOF
chmod +x "$INSTALL_DIR/cluster-jupyter"

# Copy example config if none exists
if [[ ! -f "$CONFIG_DIR/config" ]]; then
    cp -f "$(dirname "$0")/config.example" "$CONFIG_DIR/config.example"
    echo "Sample configuration created at $CONFIG_DIR/config.example"
    echo "Copy it to $CONFIG_DIR/config and customize with your cluster host and Slurm details."
fi

echo "Installation complete!"
echo "Make sure $INSTALL_DIR is in your PATH."
echo "Run 'cluster' to get started."
