package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/lunar-photon/cluster/internal/config"
	"github.com/lunar-photon/cluster/internal/slurm"
)

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// Cancelling a job takes two deliberate steps, and the second defaults to No.
func TestJobMenuConfirmsCancel(t *testing.T) {
	m := model{menu: jobMenu(slurm.Job{ID: "42", Name: "fit", State: "RUNNING"})}
	next, _ := m.updateMenu(key("x"))
	m = next.(model)
	if m.menu == nil || !strings.Contains(m.menu.title, "Cancel job 42?") {
		t.Fatalf("x should ask first: %+v", m.menu)
	}
	if got := m.menu.items[m.menu.cursor].label; got != "No, keep it" {
		t.Errorf("default must be No, got %q", got)
	}
	next, cmd := m.updateMenu(key("enter")) // Enter on the default: nothing happens
	m = next.(model)
	if m.menu != nil || cmd != nil {
		t.Error("choosing No closes the menu without doing anything")
	}

	m = model{menu: jobMenu(slurm.Job{ID: "42"})}
	next, _ = m.updateMenu(key("esc"))
	if next.(model).menu != nil {
		t.Error("esc closes the menu")
	}
}

func TestOverlayKeepsLinesWhole(t *testing.T) {
	body := "line1\nline2\nline3\nline4\nline5"
	out := strings.Split(overlay(body, "BOX\nBOX", 5, 20), "\n")
	if len(out) != 5 || out[0] != "line1" || strings.TrimSpace(out[1]) != "BOX" || strings.TrimSpace(out[2]) != "BOX" || out[4] != "line5" {
		t.Errorf("%q", out)
	}
}

// Jupyter asks which partition to use, starting on the usual one, and keeps
// the time limit within the chosen partition's.
func TestJupyterPartitionMenu(t *testing.T) {
	m := model{cfg: config.Defaults()}
	m.cfg.Slurm.Partition, m.cfg.Slurm.Time = "medium", "04:00:00"
	m.sums = []slurm.Summary{
		{PartitionInfo: &slurm.PartitionInfo{Name: "short", MaxTime: "02:00:00"}, Allowed: true},
		{PartitionInfo: &slurm.PartitionInfo{Name: "medium", MaxTime: "2-00:00:00"}, Allowed: true},
		{PartitionInfo: &slurm.PartitionInfo{Name: "restricted"}, Allowed: false},
	}
	mn := m.partitionMenu("Jupyter", "jupyter", "")
	if len(mn.items) != 3 || mn.items[0].key != "1" || !strings.Contains(mn.items[0].label, "short") {
		t.Fatalf("usable partitions plus Cancel: %+v", mn.items)
	}
	if !strings.Contains(mn.items[mn.cursor].label, "medium") || !strings.Contains(mn.items[mn.cursor].label, "(default)") {
		t.Errorf("cursor should start on the configured partition: %q", mn.items[mn.cursor].label)
	}
	if got := strings.Join(m.partitionArgs(m.sums[0], "jupyter"), " "); got != "jupyter -p short -t 02:00:00" {
		t.Errorf("short: %q", got)
	}
	if got := strings.Join(m.partitionArgs(m.sums[1], "jupyter"), " "); got != "jupyter -p medium" {
		t.Errorf("medium: %q", got)
	}
}
