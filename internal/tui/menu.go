package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/lunar-photon/cluster/internal/lifecycle"
	"github.com/lunar-photon/cluster/internal/slurm"
)

// menu is a small pop-up of choices for one job or session. Destructive
// choices open a second menu asking for confirmation, defaulting to No.
type menu struct {
	title  string
	detail []string
	items  []menuItem
	cursor int
}

type menuItem struct {
	key   string
	label string
	run   func(m model) (tea.Model, tea.Cmd)
}

func closeMenu(m model) (tea.Model, tea.Cmd) { return m, nil }

// confirm wraps a destructive action in a yes/no menu, cursor on No.
func confirmMenu(title string, detail []string, yes string, do func(m model) (tea.Model, tea.Cmd)) func(m model) (tea.Model, tea.Cmd) {
	return func(m model) (tea.Model, tea.Cmd) {
		m.menu = &menu{title: title, detail: detail, cursor: 1, items: []menuItem{
			{"y", yes, do},
			{"n", "No, keep it", closeMenu},
		}}
		return m, nil
	}
}

func jobMenu(j slurm.Job) *menu {
	detail := []string{j.Name + " · " + j.State + " · " + j.Partition}
	if j.State == "RUNNING" {
		detail = append(detail, "on "+j.NodeList+", "+j.Time+" of "+j.TimeLimit)
	} else {
		detail = append(detail, strings.Trim(j.NodeList, "()"))
	}
	if !j.ByCluster() {
		detail = append(detail, "submitted outside cluster")
	}
	return &menu{title: "Job " + j.ID, detail: detail, items: []menuItem{
		{"l", "Follow its output", func(m model) (tea.Model, tea.Cmd) {
			return m, m.run("Following job "+j.ID, "logs", "-f", j.ID)
		}},
		{"x", "Cancel the job…", confirmMenu("Cancel job "+j.ID+"?",
			[]string{j.Name, "It stops now and its allocation is released.", "This can't be undone."},
			"Yes, cancel job "+j.ID,
			func(m model) (tea.Model, tea.Cmd) { return m, m.run("Cancel job "+j.ID, "stop", j.ID) })},
		{"esc", "Close", closeMenu},
	}}
}

func sessionMenu(s lifecycle.Status) *menu {
	detail := []string{string(s.Kind) + " · started " + s.Created.Format("Jan 2 15:04")}
	if s.URL != "" {
		detail = append(detail, s.URL)
	} else if s.Mount != "" {
		detail = append(detail, "mounted at "+s.Mount)
	}
	if !s.Alive && s.Err == nil {
		return &menu{title: "Session " + s.JobID + " (ended)", detail: detail, items: []menuItem{
			{"g", "Clean it up", func(m model) (tea.Model, tea.Cmd) { return m, m.run("Clean up", "gc") }},
			{"esc", "Close", closeMenu},
		}}
	}
	what := "Its job is cancelled"
	switch {
	case s.URL != "":
		what = "Jupyter stops (kernel variables are lost) and its job is cancelled"
	case s.Mount != "":
		what = "Its job is cancelled and " + s.Mount + " is unmounted"
	}
	return &menu{title: "Session " + s.JobID, detail: detail, items: []menuItem{
		{"a", "Connect", func(m model) (tea.Model, tea.Cmd) {
			return m, m.run("Attach "+s.JobID, "attach", s.JobID)
		}},
		{"x", "Stop the session…", confirmMenu("Stop session "+s.JobID+"?",
			[]string{what + ".", "This can't be undone."},
			"Yes, stop session "+s.JobID,
			func(m model) (tea.Model, tea.Cmd) { return m, m.run("Stop "+s.JobID, "stop", s.JobID) })},
		{"esc", "Close", closeMenu},
	}}
}

// updateMenu handles keys while a menu is open.
func (m model) updateMenu(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	mn := m.menu
	switch key := k.String(); key {
	case "ctrl+c":
		return m, tea.Quit
	case "esc", "q":
		m.menu = nil
		return m, nil
	case "up":
		if mn.cursor > 0 {
			mn.cursor--
		}
		return m, nil
	case "down":
		if mn.cursor < len(mn.items)-1 {
			mn.cursor++
		}
		return m, nil
	case "enter":
		m.menu = nil
		return mn.items[mn.cursor].run(m)
	default:
		for _, it := range mn.items {
			if it.key == key {
				m.menu = nil
				return it.run(m)
			}
		}
	}
	return m, nil
}

func (m model) menuView() string {
	mn := m.menu
	var b strings.Builder
	b.WriteString(accentSt.Render(mn.title) + "\n")
	for _, d := range mn.detail {
		b.WriteString(mutedSt.Render(d) + "\n")
	}
	b.WriteString("\n")
	for i, it := range mn.items {
		line := fmt.Sprintf("%-3s %s", it.key, it.label)
		if i == mn.cursor {
			b.WriteString(selSt.Render("▸ "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	b.WriteString("\n" + mutedSt.Render("↑↓ enter · letter · esc closes"))
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(0, 2).Render(b.String())
}

// overlay draws box over the middle of body, replacing whole lines so the
// colour codes of the lines underneath are never cut in half.
func overlay(body, box string, height, width int) string {
	lines := strings.Split(body, "\n")
	for len(lines) < height {
		lines = append(lines, "")
	}
	boxLines := strings.Split(box, "\n")
	top := max(0, (height-len(boxLines))/2)
	pad := strings.Repeat(" ", max(0, (width-lipgloss.Width(boxLines[0]))/2))
	for i, bl := range boxLines {
		if top+i < len(lines) {
			lines[top+i] = pad + bl
		}
	}
	return strings.Join(lines, "\n")
}

// partitionMenu asks which partition to start sub ("jupyter", "terminal")
// in, listing the ones the user may use with their limit and free cores.
// The cursor starts on the configured profile's partition.
func (m model) partitionMenu(title, sub, profile string) *menu {
	want := ""
	if p, err := m.cfg.Profile(profile); err == nil {
		want = p.Partition
	}
	mn := &menu{title: title, detail: []string{"Where should it run?"}}
	for _, s := range m.sums {
		if !s.Allowed {
			continue
		}
		label := fmt.Sprintf("%-10s up to %s · %d cores free", s.Name, slurm.Unlimited(s.MaxTime), s.CPUsFree)
		if s.GPUsTotal > 0 {
			label += fmt.Sprintf(", %d GPUs", s.GPUsFree)
		}
		if s.Name == want || (want == "" && s.Default) {
			label += " (default)"
			mn.cursor = len(mn.items)
		}
		key := ""
		if len(mn.items) < 9 {
			key = strconv.Itoa(len(mn.items) + 1)
		}
		mn.items = append(mn.items, menuItem{key, label, func(m model) (tea.Model, tea.Cmd) {
			return m.inPartition(s, title+" on "+s.Name, sub)
		}})
	}
	mn.items = append(mn.items, menuItem{"esc", "Cancel", closeMenu})
	return mn
}
