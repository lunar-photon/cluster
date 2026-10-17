#!/usr/bin/env bash
#
# install.sh - Installer for cluster-jupyter
#

set -euo pipefail

INSTALL_DIR="${HOME}/.local/bin"
VENV_DIR="${HOME}/.venvs/jupyter-forward"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/cluster-jupyter"

echo "Installing cluster-jupyter..."

mkdir -p "$INSTALL_DIR"
mkdir -p "$CONFIG_DIR"

# Install jupyter-forward in dedicated venv if not already installed
if ! command -v jupyter-forward >/dev/null 2>&1 && [[ ! -x "$VENV_DIR/bin/jupyter-forward" ]]; then
    echo "Setting up virtual environment for jupyter-forward in $VENV_DIR..."
    python3 -m venv "$VENV_DIR"
    "$VENV_DIR/bin/pip" install --upgrade pip
    "$VENV_DIR/bin/pip" install jupyter-forward
fi

# Copy cluster-jupyter script
cp -f "$(dirname "$0")/cluster-jupyter" "$INSTALL_DIR/cluster-jupyter"
chmod +x "$INSTALL_DIR/cluster-jupyter"

# Copy example config if none exists
if [[ ! -f "$CONFIG_DIR/config" ]]; then
    cp -f "$(dirname "$0")/config.example" "$CONFIG_DIR/config.example"
    echo "Sample configuration created at $CONFIG_DIR/config.example"
    echo "Copy it to $CONFIG_DIR/config and customize with your cluster host and Slurm details."
fi

echo "Installation complete!"
echo "Make sure $INSTALL_DIR is in your PATH."
echo "Run 'cluster-jupyter --help' to get started."
