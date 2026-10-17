#!/usr/bin/env python3
"""
cluster_tui.py - Terminal User Interface for cluster HPC Suite
Powered by Textual
"""

import os
import re
import shlex
import subprocess
import sys
from pathlib import Path

from textual import on, work
from textual.app import App, ComposeResult
from textual.binding import Binding
from textual.containers import Container, Horizontal, Vertical, VerticalScroll
from textual.reactive import reactive
from textual.widgets import (
    Button,
    DataTable,
    Footer,
    Header,
    Input,
    Label,
    Rule,
    Static,
    TabbedContent,
    TabPane,
)

CONFIG_DIR = Path(os.environ.get("XDG_CONFIG_HOME", Path.home() / ".config")) / "cluster"
CONFIG_FILE = CONFIG_DIR / "config"
LEGACY_CONFIG = Path(os.environ.get("XDG_CONFIG_HOME", Path.home() / ".config")) / "cluster-jupyter" / "config"


def load_config() -> dict[str, str]:
    defaults = {
        "HOST": "",
        "PORT": "8888",
        "SLURM_PARTITION": "medium",
        "SLURM_ACCOUNT": "",
        "SLURM_QOS": "",
        "SLURM_CPUS": "4",
        "SLURM_MEM": "8G",
        "SLURM_TIME": "04:00:00",
        "GPU_PARTITION": "gpu",
        "GPU_QOS": "",
        "MOUNT_POINT": str(Path.home() / "Cluster"),
        "NOTEBOOK_DIR": "",
    }
    cfg_file = CONFIG_FILE if CONFIG_FILE.exists() else (LEGACY_CONFIG if LEGACY_CONFIG.exists() else None)
    if cfg_file and cfg_file.exists():
        with open(cfg_file, "r", encoding="utf-8") as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#"):
                    continue
                match = re.match(r'^([A-Z_]+)=["\']?(.*?)["\']?$', line)
                if match:
                    k, v = match.group(1), match.group(2)
                    v = v.replace("$HOME", str(Path.home()))
                    defaults[k] = v
    return defaults


def save_config(cfg: dict[str, str]) -> None:
    CONFIG_DIR.mkdir(parents=True, exist_ok=True)
    with open(CONFIG_FILE, "w", encoding="utf-8") as f:
        f.write("# cluster configuration file\n")
        f.write("# Generated via cluster TUI\n\n")
        for k, v in cfg.items():
            f.write(f'{k}="{v}"\n')


def is_mounted(path: str) -> bool:
    expanded = Path(path).expanduser()
    return expanded.is_mount()


class StatusBadge(Static):
    """Badge widget to display connectivity and mount state."""

    mount_state = reactive(False)
    host_name = reactive("")

    def render(self) -> str:
        h = self.host_name or "No Host"
        m_color = "green" if self.mount_state else "dim"
        m_status = f"Mounted at {Path.home() / 'Cluster'}" if self.mount_state else "Unmounted"
        return f"Host: [bold cyan]{h}[/]   |   Drive: [{m_color}]{m_status}[/]"


class ClusterApp(App):
    CSS = """
    Screen {
        background: #12151c;
        color: #e2e8f0;
    }

    Header {
        background: #1e2430;
        color: #38bdf8;
    }

    Footer {
        background: #1e2430;
        color: #94a3b8;
    }

    #status-bar {
        background: #181d27;
        padding: 0 1;
        height: 2;
        border-bottom: solid #2a3445;
        content-align: center middle;
    }

    .action-card {
        background: #1a202c;
        border: round #334155;
        padding: 1;
        margin: 1;
        height: auto;
    }

    .action-title {
        color: #38bdf8;
        text-style: bold;
        margin-bottom: 0;
    }

    .action-desc {
        color: #94a3b8;
        margin-bottom: 1;
    }

    Button {
        margin-right: 1;
        min-width: 14;
    }

    .btn-primary {
        background: #0284c7;
        color: #ffffff;
    }

    .btn-primary:hover {
        background: #0369a1;
    }

    .btn-secondary {
        background: #334155;
        color: #e2e8f0;
    }

    .btn-secondary:hover {
        background: #475569;
    }

    .btn-danger {
        background: #b91c1c;
        color: #ffffff;
    }

    .btn-danger:hover {
        background: #991b1b;
    }

    DataTable {
        background: #181d27;
        height: 12;
        margin-bottom: 1;
        border: round #2a3445;
    }

    .config-label {
        width: 22;
        padding-top: 1;
        color: #94a3b8;
    }

    .config-row {
        height: 3;
        margin-bottom: 0;
    }

    Input {
        width: 1fr;
        background: #1e2430;
        border: tall #334155;
    }

    #feedback-box {
        height: 2;
        color: #22c55e;
        padding: 0 1;
    }
    """

    BINDINGS = [
        Binding("q", "quit", "Quit", show=True),
        Binding("r", "refresh_status", "Refresh", show=True),
        Binding("d", "quick_dev", "Dev Workspace", show=True),
        Binding("m", "toggle_mount", "Toggle Mount", show=True),
        Binding("j", "quick_jupyter", "JupyterLab", show=True),
    ]

    TITLE = "cluster"
    SUB_TITLE = "HPC Developer Suite"

    def __init__(self):
        super().__init__()
        self.cfg = load_config()

    def compose(self) -> ComposeResult:
        yield Header(show_clock=True)
        yield StatusBadge(id="status-bar")

        with TabbedContent(initial="actions-tab"):
            with TabPane("Workspace & Actions", id="actions-tab"):
                with VerticalScroll():
                    # Card 1: Dev Workspace
                    with Vertical(classes="action-card"):
                        yield Label("All-in-One Dev Workspace", classes="action-title")
                        yield Label(
                            "Mounts cluster to ~/Cluster, opens Dolphin file manager, and connects an interactive compute shell. Auto-unmounts on exit.",
                            classes="action-desc",
                        )
                        with Horizontal():
                            yield Button("Launch Dev Workspace (CPU)", id="btn-dev-cpu", classes="btn-primary")
                            yield Button("Launch Dev Workspace (GPU)", id="btn-dev-gpu", classes="btn-primary")

                    # Card 2: Interactive Shells
                    with Vertical(classes="action-card"):
                        yield Label("Interactive Compute Shell", classes="action-title")
                        yield Label(
                            "Borrow dedicated node CPUs or GPUs without mounting drives. Auto-releases when closed.",
                            classes="action-desc",
                        )
                        with Horizontal():
                            yield Button("CPU Shell", id="btn-shell-cpu", classes="btn-secondary")
                            yield Button("GPU Shell", id="btn-shell-gpu", classes="btn-secondary")

                    # Card 3: JupyterLab
                    with Vertical(classes="action-card"):
                        yield Label("Remote JupyterLab", classes="action-title")
                        yield Label(
                            "Auto port tunneling and browser launch. No manual tokens or port setups.",
                            classes="action-desc",
                        )
                        with Horizontal():
                            yield Button("Head Node", id="btn-jup-head", classes="btn-secondary")
                            yield Button("Compute Node (Slurm)", id="btn-jup-slurm", classes="btn-secondary")
                            yield Button("GPU Node", id="btn-jup-gpu", classes="btn-secondary")

                    # Card 4: Filesystem Mount
                    with Vertical(classes="action-card"):
                        yield Label("Storage Mount (SSHFS)", classes="action-title")
                        yield Label(
                            "Mount cluster storage as a local folder for native copy-pasting, drag-and-drop, and editing.",
                            classes="action-desc",
                        )
                        with Horizontal():
                            yield Button("Mount Drive", id="btn-mount", classes="btn-secondary")
                            yield Button("Unmount Drive", id="btn-unmount", classes="btn-danger")

                    # Card 5: Code Sync
                    with Vertical(classes="action-card"):
                        yield Label("Fast Code Sync", classes="action-title")
                        yield Label(
                            "Rapidly push or pull project directories via rsync (ignoring .git and caches).",
                            classes="action-desc",
                        )
                        with Horizontal():
                            yield Button("Push Code -> Cluster", id="btn-sync-push", classes="btn-secondary")
                            yield Button("Pull Code <- Cluster", id="btn-sync-pull", classes="btn-secondary")

            with TabPane("Cluster Monitor", id="monitor-tab"):
                with Vertical():
                    with Horizontal():
                        yield Label("[bold cyan]My Active & Queued Jobs[/]")
                        yield Button("Refresh", id="btn-refresh-mon", classes="btn-secondary")
                    yield DataTable(id="table-jobs")

                    yield Label("[bold cyan]Partition & Node Availability[/]")
                    yield DataTable(id="table-nodes")

            with TabPane("Settings", id="settings-tab"):
                with VerticalScroll():
                    yield Label("[bold cyan]Cluster & Slurm Defaults[/]", classes="action-title")
                    yield Label("Changes are saved to ~/.config/cluster/config.", classes="action-desc")

                    with Horizontal(classes="config-row"):
                        yield Label("Cluster Host:", classes="config-label")
                        yield Input(self.cfg.get("HOST", ""), id="cfg-host")

                    with Horizontal(classes="config-row"):
                        yield Label("Slurm Partition:", classes="config-label")
                        yield Input(self.cfg.get("SLURM_PARTITION", "medium"), id="cfg-partition")

                    with Horizontal(classes="config-row"):
                        yield Label("Slurm Account:", classes="config-label")
                        yield Input(self.cfg.get("SLURM_ACCOUNT", ""), id="cfg-account")

                    with Horizontal(classes="config-row"):
                        yield Label("Slurm QOS:", classes="config-label")
                        yield Input(self.cfg.get("SLURM_QOS", ""), id="cfg-qos")

                    with Horizontal(classes="config-row"):
                        yield Label("CPU Count:", classes="config-label")
                        yield Input(self.cfg.get("SLURM_CPUS", "4"), id="cfg-cpus")

                    with Horizontal(classes="config-row"):
                        yield Label("Memory:", classes="config-label")
                        yield Input(self.cfg.get("SLURM_MEM", "8G"), id="cfg-mem")

                    with Horizontal(classes="config-row"):
                        yield Label("GPU Partition:", classes="config-label")
                        yield Input(self.cfg.get("GPU_PARTITION", "gpu"), id="cfg-gpu-part")

                    with Horizontal(classes="config-row"):
                        yield Label("GPU QOS:", classes="config-label")
                        yield Input(self.cfg.get("GPU_QOS", ""), id="cfg-gpu-qos")

                    with Horizontal(classes="config-row"):
                        yield Label("Mount Point:", classes="config-label")
                        yield Input(self.cfg.get("MOUNT_POINT", str(Path.home() / "Cluster")), id="cfg-mount")

                    yield Rule()
                    with Horizontal():
                        yield Button("Save Configuration", id="btn-save-cfg", classes="btn-primary")
                        yield Label("", id="feedback-box")

        yield Footer()

    def on_mount(self) -> None:
        self.update_status_bar()
        self.init_tables()
        self.refresh_monitor_data()

    def update_status_bar(self) -> None:
        badge = self.query_one(StatusBadge)
        badge.host_name = self.cfg.get("HOST", "")
        badge.mount_state = is_mounted(self.cfg.get("MOUNT_POINT", str(Path.home() / "Cluster")))

    def init_tables(self) -> None:
        jobs_table = self.query_one("#table-jobs", DataTable)
        jobs_table.add_columns("Job ID", "Partition", "Name", "User", "State", "Time", "Nodes", "Node List")

        nodes_table = self.query_one("#table-nodes", DataTable)
        nodes_table.add_columns("Partition", "Avail", "Time Limit", "Nodes", "State", "Node List")

    @work(thread=True)
    def refresh_monitor_data(self) -> None:
        host = self.cfg.get("HOST", "")
        if not host:
            return

        # Fetch jobs
        res_jobs = subprocess.run(
            ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=4", host, 'squeue -u $USER -o "%i|%P|%j|%u|%T|%M|%D|%R"'],
            capture_output=True,
            text=True,
        )

        # Fetch nodes
        res_nodes = subprocess.run(
            ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=4", host, 'sinfo -o "%P|%a|%l|%D|%T|%N"'],
            capture_output=True,
            text=True,
        )

        def update_ui():
            # Update jobs table
            jobs_table = self.query_one("#table-jobs", DataTable)
            jobs_table.clear()
            if res_jobs.returncode == 0:
                lines = [line.strip() for line in res_jobs.stdout.strip().split("\n") if line.strip()]
                for line in lines[1:]:  # skip header
                    parts = line.split("|")
                    if len(parts) >= 8:
                        jobs_table.add_row(*parts[:8])

            # Update nodes table
            nodes_table = self.query_one("#table-nodes", DataTable)
            nodes_table.clear()
            if res_nodes.returncode == 0:
                lines = [line.strip() for line in res_nodes.stdout.strip().split("\n") if line.strip()]
                for line in lines[1:]:  # skip header
                    parts = line.split("|")
                    if len(parts) >= 6:
                        nodes_table.add_row(*parts[:6])

            self.update_status_bar()

        self.call_from_thread(update_ui)

    def run_external_cmd(self, cmd_str: str) -> None:
        """Suspends Textual to run interactive shell command, then resumes."""
        with self.suspend():
            os.system(cmd_str)
        self.update_status_bar()
        self.refresh_monitor_data()

    @on(Button.Pressed)
    def handle_buttons(self, event: Button.Pressed) -> None:
        btn_id = event.button.id

        if btn_id == "btn-dev-cpu":
            self.run_external_cmd("cluster dev -b")
        elif btn_id == "btn-dev-gpu":
            self.run_external_cmd("cluster dev --gpu -b")
        elif btn_id == "btn-shell-cpu":
            self.run_external_cmd("cluster shell")
        elif btn_id == "btn-shell-gpu":
            self.run_external_cmd("cluster shell --gpu")
        elif btn_id == "btn-jup-head":
            self.run_external_cmd("cluster jupyter")
        elif btn_id == "btn-jup-slurm":
            self.run_external_cmd("cluster jupyter --slurm")
        elif btn_id == "btn-jup-gpu":
            self.run_external_cmd("cluster jupyter --gpu")
        elif btn_id == "btn-mount":
            self.run_external_cmd("cluster mount -b")
        elif btn_id == "btn-unmount":
            self.run_external_cmd("cluster unmount")
        elif btn_id == "btn-sync-push":
            self.run_external_cmd("cluster sync push")
        elif btn_id == "btn-sync-pull":
            self.run_external_cmd("cluster sync pull")
        elif btn_id == "btn-refresh-mon":
            self.refresh_monitor_data()
        elif btn_id == "btn-save-cfg":
            self.save_settings()

    def save_settings(self) -> None:
        self.cfg["HOST"] = self.query_one("#cfg-host", Input).value.strip()
        self.cfg["SLURM_PARTITION"] = self.query_one("#cfg-partition", Input).value.strip()
        self.cfg["SLURM_ACCOUNT"] = self.query_one("#cfg-account", Input).value.strip()
        self.cfg["SLURM_QOS"] = self.query_one("#cfg-qos", Input).value.strip()
        self.cfg["SLURM_CPUS"] = self.query_one("#cfg-cpus", Input).value.strip()
        self.cfg["SLURM_MEM"] = self.query_one("#cfg-mem", Input).value.strip()
        self.cfg["GPU_PARTITION"] = self.query_one("#cfg-gpu-part", Input).value.strip()
        self.cfg["GPU_QOS"] = self.query_one("#cfg-gpu-qos", Input).value.strip()
        self.cfg["MOUNT_POINT"] = self.query_one("#cfg-mount", Input).value.strip()

        save_config(self.cfg)
        fb = self.query_one("#feedback-box", Label)
        fb.update("Configuration saved successfully!")
        self.update_status_bar()

    def action_refresh_status(self) -> None:
        self.refresh_monitor_data()

    def action_quick_dev(self) -> None:
        self.run_external_cmd("cluster dev -b")

    def action_quick_jupyter(self) -> None:
        self.run_external_cmd("cluster jupyter")

    def action_toggle_mount(self) -> None:
        mp = self.cfg.get("MOUNT_POINT", str(Path.home() / "Cluster"))
        if is_mounted(mp):
            self.run_external_cmd("cluster unmount")
        else:
            self.run_external_cmd("cluster mount -b")


if __name__ == "__main__":
    app = ClusterApp()
    app.run()
