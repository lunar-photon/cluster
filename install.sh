#!/usr/bin/env bash
#
# install.sh - build and install the cluster binary into ~/.local/bin
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PREFIX="${PREFIX:-$HOME/.local}"

if ! command -v go >/dev/null 2>&1; then
    echo "Go is required to build cluster (https://go.dev/dl/, or e.g. 'sudo pacman -S go')." >&2
    exit 1
fi

make -C "$SCRIPT_DIR" build
install -Dm755 "$SCRIPT_DIR/bin/cluster" "$PREFIX/bin/cluster"
echo "Installed $PREFIX/bin/cluster"

# Remove pieces of the previous bash/C++ install that the Go binary replaces.
for old in cluster-tui cluster-jupyter; do
    if [[ -e "$PREFIX/bin/$old" ]]; then
        rm -f "$PREFIX/bin/$old"
        echo "Removed old $PREFIX/bin/$old"
    fi
done

case ":$PATH:" in
    *":$PREFIX/bin:"*) ;;
    *) echo "Add $PREFIX/bin to your PATH." ;;
esac

if [[ ! -f "${XDG_CONFIG_HOME:-$HOME/.config}/cluster/config.toml" ]]; then
    if [[ -f "${XDG_CONFIG_HOME:-$HOME/.config}/cluster/config" ]]; then
        echo "Your old config will be read as-is; run 'cluster config migrate' to convert it."
    else
        echo "Run 'cluster init' to set up your cluster."
    fi
fi
