#!/usr/bin/env bash
#
# install.sh - build cluster, install it into ~/.local/bin, and check the cluster
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PREFIX="${PREFIX:-$HOME/.local}"
BACKUP="${XDG_DATA_HOME:-$HOME/.local/share}/cluster/previous-install/$(date +%Y%m%d-%H%M%S)"

if ! command -v go >/dev/null 2>&1; then
    echo "Go is required to build cluster (https://go.dev/dl/, or e.g. 'sudo pacman -S go')." >&2
    exit 1
fi

make -C "$SCRIPT_DIR" build

# Keep the previous install instead of overwriting it. Each run gets its own
# folder, so reinstalling never replaces an older backup.
saved=0
for old in cluster cluster-tui cluster-jupyter; do
    if [[ $old == cluster ]] && cmp -s "$PREFIX/bin/cluster" "$SCRIPT_DIR/bin/cluster"; then
        continue # same build already installed
    fi
    if [[ -e "$PREFIX/bin/$old" ]]; then
        mkdir -p "$BACKUP"
        mv -f "$PREFIX/bin/$old" "$BACKUP/$old"
        saved=1
    fi
done
if [[ $saved == 1 ]]; then
    echo "Previous install moved to $BACKUP (move the files back to $PREFIX/bin to restore it)."
fi

install -Dm755 "$SCRIPT_DIR/bin/cluster" "$PREFIX/bin/cluster"
echo "Installed $PREFIX/bin/cluster"

case ":$PATH:" in
    *":$PREFIX/bin:"*) ;;
    *) echo "Add $PREFIX/bin to your PATH." ;;
esac

# Check the cluster and offer to install tmux there. Needs a configured host.
if "$PREFIX/bin/cluster" config show 2>/dev/null | grep -Eq '^host = ".+"'; then
    echo
    "$PREFIX/bin/cluster" setup || echo "Cluster check failed; run 'cluster setup' later."
    if [[ ! -f "${XDG_CONFIG_HOME:-$HOME/.config}/cluster/config.toml" ]]; then
        echo
        echo "Your old config is read as-is; 'cluster config migrate' converts it to config.toml (the old file is kept)."
    fi
else
    echo "Run 'cluster init' to set up your cluster."
fi
