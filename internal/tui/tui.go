// Package tui is the interactive dashboard. It never runs long operations
// itself: actions re-invoke this binary as a subprocess with the terminal
// handed over (tea.ExecProcess), and queries run as tea.Cmds whose results
// come back as messages, so all model state is touched only in Update.
package tui

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	"github.com/lunar-photon/cluster/internal/config"
	"github.com/lunar-photon/cluster/internal/lifecycle"
	"github.com/lunar-photon/cluster/internal/mount"
	"github.com/lunar-photon/cluster/internal/remote"
	"github.com/lunar-photon/cluster/internal/slurm"
)

const (
	tabActions = iota
	tabMonitor
	tabSessions
	tabSettings
)

var tabNames = []string{"Actions", "Monitor", "Sessions", "Settings"}

type action struct {
	key   string
	label string
	desc  string
	args  []string
}

var actions = []action{
	{"d", "Dev workspace (CPU)", "Mount storage + compute shell; both cleaned up on exit", []string{"dev", "--browse"}},
	{"D", "Dev workspace (GPU)", "Same, on a GPU node", []string{"dev", "--browse", "--gpu"}},
	{"s", "Compute shell (CPU)", "Shell on a dedicated node; survives Wi-Fi drops", []string{"shell"}},
	{"S", "Compute shell (GPU)", "Shell on a dedicated GPU node", []string{"shell", "--gpu"}},
	{"j", "JupyterLab (compute node)", "Tunneled to your browser; Ctrl+C stops it", []string{"jupyter"}},
	{"J", "JupyterLab (GPU node)", "", []string{"jupyter", "--gpu"}},
	{"h", "JupyterLab (login node)", "For light work only", []string{"jupyter", "--head"}},
	{"m", "Mount / unmount storage", "Toggle the sshfs mount", nil},
	{"p", "Push code ->", "rsync this directory to the cluster", []string{"sync", "push"}},
	{"P", "Pull code <-", "rsync it back", []string{"sync", "pull"}},
}

// ---- messages ----

type refreshMsg struct {
	jobs     []slurm.Job
	parts    []slurm.Partition
	sessions []lifecycle.Status
	err      error
}

type tickMsg time.Time
type autoRefreshMsg struct{}

type execDoneMsg struct {
	what string
	err  error
}

// ---- settings form ----

type field struct {
	label string
	get   func(*config.Config) string
	set   func(*config.Config, string) error
}

func intField(p *int) func(string) error {
	return func(s string) error {
		if s == "" {
			*p = 0
			return nil
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return fmt.Errorf("%q is not a whole number", s)
		}
		*p = n
		return nil
	}
}

func gpuProfile(c *config.Config) *config.Slurm {
	if c.Profiles == nil {
		c.Profiles = map[string]config.Slurm{}
	}
	p := c.Profiles["gpu"]
	return &p
}

func setGPU(c *config.Config, f func(*config.Slurm)) {
	p := gpuProfile(c)
	f(p)
	c.Profiles["gpu"] = *p
}

var fields = []field{
	{"Host", func(c *config.Config) string { return c.Host }, func(c *config.Config, v string) error { c.Host = v; return nil }},
	{"Mount point", func(c *config.Config) string { return c.MountPoint }, func(c *config.Config, v string) error { c.MountPoint = v; return nil }},
	{"Remote dir", func(c *config.Config) string { return c.RemoteDir }, func(c *config.Config, v string) error { c.RemoteDir = v; return nil }},
	{"Partition", func(c *config.Config) string { return c.Slurm.Partition }, func(c *config.Config, v string) error { c.Slurm.Partition = v; return nil }},
	{"Account", func(c *config.Config) string { return c.Slurm.Account }, func(c *config.Config, v string) error { c.Slurm.Account = v; return nil }},
	{"QOS", func(c *config.Config) string { return c.Slurm.QOS }, func(c *config.Config, v string) error { c.Slurm.QOS = v; return nil }},
	{"CPU cores", func(c *config.Config) string { return itoa(c.Slurm.CPUs) }, func(c *config.Config, v string) error { return intField(&c.Slurm.CPUs)(v) }},
	{"Memory", func(c *config.Config) string { return c.Slurm.Mem }, func(c *config.Config, v string) error { c.Slurm.Mem = v; return nil }},
	{"Walltime", func(c *config.Config) string { return c.Slurm.Time }, func(c *config.Config, v string) error { c.Slurm.Time = v; return nil }},
	{"GPU partition", func(c *config.Config) string { return gpuProfile(c).Partition }, func(c *config.Config, v string) error {
		setGPU(c, func(p *config.Slurm) { p.Partition = v })
		return nil
	}},
	{"GPU QOS", func(c *config.Config) string { return gpuProfile(c).QOS }, func(c *config.Config, v string) error {
		setGPU(c, func(p *config.Slurm) { p.QOS = v })
		return nil
	}},
	{"GPUs per job", func(c *config.Config) string { return itoa(gpuProfile(c).GPUs) }, func(c *config.Config, v string) error {
		var err error
		setGPU(c, func(p *config.Slurm) { err = intField(&p.GPUs)(v) })
		return err
	}},
	{"Jupyter setup", func(c *config.Config) string { return c.Jupyter.Setup }, func(c *config.Config, v string) error { c.Jupyter.Setup = v; return nil }},
}

func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// ---- model ----

type model struct {
	cfg  config.Config
	env  *lifecycle.Env // nil when no host is configured
	self string

	tab         int
	cursor      [4]int
	width       int
	height      int
	mounted     bool
	connected   bool
	refreshing  bool
	lastRefresh time.Time

	jobs     []slurm.Job
	parts    []slurm.Partition
	sessions []lifecycle.Status

	status  string
	errMsg  string
	inputs  []textinput.Model
	editing bool
}

// Run starts the dashboard. r may be nil when no host is configured.
func Run(cfg config.Config, r *remote.Client, self string) error {
	m := model{cfg: cfg, self: self}
	if r != nil {
		m.env = lifecycle.New(cfg, r)
		m.connected = r.MasterAlive()
	}
	m.inputs = make([]textinput.Model, len(fields))
	for i, f := range fields {
		ti := textinput.New()
		ti.Prompt = ""
		ti.CharLimit = 256
		ti.SetValue(f.get(&m.cfg))
		m.inputs[i] = ti
	}
	if m.env == nil {
		m.tab = tabSettings
		m.status = "No cluster host configured — fill in Host and press Ctrl+S (or run `cluster init`)."
	}
	m.mounted = mount.IsMounted(cfg.LocalMountPoint())
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tick(), autoRefresh())
}

func tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func autoRefresh() tea.Cmd {
	return tea.Tick(30*time.Second, func(time.Time) tea.Msg { return autoRefreshMsg{} })
}

func (m *model) refresh() tea.Cmd {
	if m.env == nil || m.refreshing {
		return nil
	}
	m.refreshing = true
	env := m.env
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var msg refreshMsg
		msg.jobs, msg.err = env.Slurm.Jobs(ctx)
		if msg.err == nil {
			msg.parts, msg.err = env.Slurm.Partitions(ctx)
		}
		msg.sessions, _ = env.Sessions(ctx)
		return msg
	}
}

func (m model) run(what string, args ...string) tea.Cmd {
	cmd := exec.Command(m.self, args...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return execDoneMsg{what, err} })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		for i := range m.inputs {
			m.inputs[i].Width = max(10, msg.Width-24)
		}
		return m, nil

	case tickMsg:
		m.mounted = mount.IsMounted(m.cfg.LocalMountPoint())
		return m, tick()

	case autoRefreshMsg:
		return m, tea.Batch(m.refresh(), autoRefresh())

	case refreshMsg:
		m.refreshing = false
		m.lastRefresh = time.Now()
		if msg.err != nil {
			m.connected = false
			m.errMsg = "Cluster query failed: " + firstLine(msg.err.Error()) + " — press c to connect"
		} else {
			m.connected = true
			m.errMsg = ""
			m.jobs, m.parts = msg.jobs, msg.parts
		}
		m.sessions = msg.sessions
		m.clampCursors()
		return m, nil

	case execDoneMsg:
		if msg.err != nil {
			m.errMsg = msg.what + ": " + msg.err.Error()
		} else {
			m.status = msg.what + " finished."
		}
		m.mounted = mount.IsMounted(m.cfg.LocalMountPoint())
		return m, m.refresh()

	case tea.KeyMsg:
		if m.editing {
			return m.updateEditing(msg)
		}
		return m.updateKeys(msg)
	}
	return m, nil
}

func (m model) updateKeys(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	switch key {
	case "q", "ctrl+c", "esc":
		return m, tea.Quit
	case "tab", "right":
		m.tab = (m.tab + 1) % len(tabNames)
		return m, nil
	case "shift+tab", "left":
		m.tab = (m.tab + len(tabNames) - 1) % len(tabNames)
		return m, nil
	case "1", "2", "3", "4":
		m.tab = int(key[0] - '1')
		return m, nil
	case "r":
		m.status = "Refreshing..."
		return m, m.refresh()
	case "c":
		return m, m.run("Connect", "connect")
	case "up":
		if m.cursor[m.tab] > 0 {
			m.cursor[m.tab]--
		}
		return m, nil
	case "down":
		m.cursor[m.tab]++
		m.clampCursors()
		return m, nil
	}

	switch m.tab {
	case tabActions:
		if key == "enter" {
			return m.runAction(actions[m.cursor[tabActions]])
		}
		for _, a := range actions {
			if a.key == key {
				return m.runAction(a)
			}
		}
	case tabSessions:
		if len(m.sessions) == 0 {
			break
		}
		s := m.sessions[m.cursor[tabSessions]]
		switch key {
		case "enter", "a":
			return m, m.run("Attach "+s.JobID, "attach", s.JobID)
		case "x":
			return m, m.run("Stop "+s.JobID, "stop", s.JobID)
		case "g":
			return m, m.run("Clean up", "gc")
		}
	case tabSettings:
		switch key {
		case "enter", "e":
			m.editing = true
			return m, m.inputs[m.cursor[tabSettings]].Focus()
		case "ctrl+s":
			return m.save()
		}
	}
	return m, nil
}

func (m model) updateEditing(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	i := m.cursor[tabSettings]
	switch k.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.inputs[i].Blur()
		m.editing = false
		return m, nil
	case "ctrl+s":
		m.inputs[i].Blur()
		m.editing = false
		return m.save()
	case "enter", "down", "tab":
		m.inputs[i].Blur()
		if i+1 < len(m.inputs) {
			m.cursor[tabSettings] = i + 1
			return m, m.inputs[i+1].Focus()
		}
		m.editing = false
		return m, nil
	case "up", "shift+tab":
		m.inputs[i].Blur()
		if i > 0 {
			m.cursor[tabSettings] = i - 1
			return m, m.inputs[i-1].Focus()
		}
		m.editing = false
		return m, nil
	}
	var cmd tea.Cmd
	m.inputs[i], cmd = m.inputs[i].Update(k)
	return m, cmd
}

func (m model) save() (tea.Model, tea.Cmd) {
	cfg := m.cfg
	// Deep-copy profiles so a validation error leaves m.cfg untouched.
	cfg.Profiles = map[string]config.Slurm{}
	for k, v := range m.cfg.Profiles {
		cfg.Profiles[k] = v
	}
	for i, f := range fields {
		if err := f.set(&cfg, strings.TrimSpace(m.inputs[i].Value())); err != nil {
			m.errMsg = f.label + ": " + err.Error()
			return m, nil
		}
	}
	if err := cfg.Save(); err != nil {
		m.errMsg = "Save failed: " + err.Error()
		return m, nil
	}
	hostChanged := cfg.Host != m.cfg.Host
	cfg.Legacy = false
	m.cfg = cfg
	m.errMsg = ""
	m.status = "Saved " + cfg.Path
	if hostChanged && cfg.Host != "" {
		m.env = lifecycle.New(cfg, remote.New(cfg.Host))
		m.status += " — press c to connect to " + cfg.Host
	} else if m.env != nil {
		m.env.Cfg = cfg
	}
	return m, nil
}

func (m model) runAction(a action) (tea.Model, tea.Cmd) {
	if m.env == nil {
		m.errMsg = "Configure a host first (Settings tab)."
		return m, nil
	}
	if a.args == nil { // mount toggle
		if m.mounted {
			return m, m.run("Unmount", "unmount")
		}
		return m, m.run("Mount", "mount", "--browse")
	}
	return m, m.run(a.label, a.args...)
}

func (m *model) clampCursors() {
	limits := [4]int{len(actions), len(m.jobs), len(m.sessions), len(fields)}
	for i, n := range limits {
		if m.cursor[i] >= n {
			m.cursor[i] = max(0, n-1)
		}
	}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

// ---- view ----

var (
	accent   = lipgloss.AdaptiveColor{Light: "#0b7285", Dark: "#66d9e8"}
	muted    = lipgloss.AdaptiveColor{Light: "#666666", Dark: "#8a8a8a"}
	good     = lipgloss.AdaptiveColor{Light: "#2b8a3e", Dark: "#8ce99a"}
	bad      = lipgloss.AdaptiveColor{Light: "#c92a2a", Dark: "#ff8787"}
	warn     = lipgloss.AdaptiveColor{Light: "#e67700", Dark: "#ffd43b"}
	titleSt  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#000000")).Background(accent).Padding(0, 1)
	mutedSt  = lipgloss.NewStyle().Foreground(muted)
	accentSt = lipgloss.NewStyle().Foreground(accent).Bold(true)
	selSt    = lipgloss.NewStyle().Foreground(accent).Bold(true)
	tabOn    = lipgloss.NewStyle().Bold(true).Foreground(accent).Underline(true).Padding(0, 2)
	tabOff   = lipgloss.NewStyle().Foreground(muted).Padding(0, 2)
)

func (m model) View() string {
	if m.width == 0 {
		return ""
	}
	var b strings.Builder

	host := m.cfg.Host
	if host == "" {
		host = "(no host)"
	}
	conn := lipgloss.NewStyle().Foreground(bad).Render("● offline")
	if m.connected {
		conn = lipgloss.NewStyle().Foreground(good).Render("● connected")
	}
	mnt := mutedSt.Render("not mounted")
	if m.mounted {
		mnt = lipgloss.NewStyle().Foreground(good).Render("mounted at " + m.cfg.MountPoint)
	}
	left := titleSt.Render("cluster") + " " + accentSt.Render(host) + "  " + conn
	right := "drive: " + mnt
	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right))
	b.WriteString(left + strings.Repeat(" ", gap) + right + "\n")

	var tabs []string
	for i, n := range tabNames {
		label := fmt.Sprintf("%d %s", i+1, n)
		if i == tabSessions && len(m.sessions) > 0 {
			label += fmt.Sprintf(" (%d)", len(m.sessions))
		}
		if i == m.tab {
			tabs = append(tabs, tabOn.Render(label))
		} else {
			tabs = append(tabs, tabOff.Render(label))
		}
	}
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, tabs...) + "\n\n")

	bodyHeight := max(5, m.height-6)
	var body string
	switch m.tab {
	case tabActions:
		body = m.viewActions()
	case tabMonitor:
		body = m.viewMonitor(bodyHeight)
	case tabSessions:
		body = m.viewSessions()
	case tabSettings:
		body = m.viewSettings()
	}
	body = lipgloss.NewStyle().Height(bodyHeight).MaxHeight(bodyHeight).Render(body)
	b.WriteString(body + "\n")

	switch {
	case m.errMsg != "":
		b.WriteString(lipgloss.NewStyle().Foreground(bad).Render(truncate(m.errMsg, m.width)) + "\n")
	case m.status != "":
		b.WriteString(lipgloss.NewStyle().Foreground(warn).Render(truncate(m.status, m.width)) + "\n")
	default:
		b.WriteString("\n")
	}
	b.WriteString(mutedSt.Render(truncate(m.help(), m.width)))
	return b.String()
}

func (m model) help() string {
	common := "tab/1-4 switch · r refresh · c connect · q quit"
	switch m.tab {
	case tabActions:
		return "↑↓ select · enter run · letter shortcut · " + common
	case tabSessions:
		return "↑↓ select · a/enter attach · x stop · g clean up · " + common
	case tabSettings:
		if m.editing {
			return "type to edit · enter/↓ next · ↑ previous · ctrl+s save · esc done"
		}
		return "↑↓ select · enter edit · ctrl+s save · " + common
	}
	return common
}

func (m model) viewActions() string {
	var b strings.Builder
	for i, a := range actions {
		cursor := "  "
		label := a.label
		if i == m.cursor[tabActions] {
			cursor = selSt.Render("▸ ")
			label = selSt.Render(label)
		}
		fmt.Fprintf(&b, "%s%s  %-28s %s\n", cursor, accentSt.Render(a.key), label, mutedSt.Render(a.desc))
	}
	return b.String()
}

func (m model) viewMonitor(height int) string {
	if m.env == nil {
		return mutedSt.Render("No host configured.")
	}
	var b strings.Builder
	when := "never"
	if !m.lastRefresh.IsZero() {
		when = m.lastRefresh.Format("15:04:05")
	}
	b.WriteString(accentSt.Render("Your jobs") + mutedSt.Render("  updated "+when+", every 30s") + "\n")

	maxJobs := max(1, height/2-4)
	jobRows := [][]string{}
	for i, j := range m.jobs {
		if i == maxJobs {
			jobRows = append(jobRows, []string{"…", fmt.Sprintf("%d more", len(m.jobs)-maxJobs), "", "", "", ""})
			break
		}
		jobRows = append(jobRows, []string{j.ID, j.Partition, j.Name, j.State, j.Time + " / " + j.TimeLimit, j.NodeList})
	}
	if len(jobRows) == 0 {
		b.WriteString(mutedSt.Render("  no jobs") + "\n")
	} else {
		b.WriteString(m.table([]string{"JOB", "PARTITION", "NAME", "STATE", "TIME / LIMIT", "NODES (REASON)"}, jobRows, 3) + "\n")
	}

	b.WriteString("\n" + accentSt.Render("Partitions") + "\n")
	partRows := [][]string{}
	for _, p := range m.parts {
		partRows = append(partRows, []string{p.Name, p.Avail, p.TimeLimit, p.Nodes, p.State})
	}
	if len(partRows) == 0 {
		b.WriteString(mutedSt.Render("  no data") + "\n")
	} else {
		b.WriteString(m.table([]string{"PARTITION", "AVAIL", "TIMELIMIT", "NODES", "STATE"}, partRows, 4))
	}
	return b.String()
}

func (m model) table(headers []string, rows [][]string, stateCol int) string {
	return table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(mutedSt).
		Headers(headers...).
		Rows(rows...).
		StyleFunc(func(row, col int) lipgloss.Style {
			s := lipgloss.NewStyle().Padding(0, 1)
			if row == table.HeaderRow {
				return s.Bold(true).Foreground(accent)
			}
			if col == stateCol && row >= 0 && row < len(rows) {
				switch v := rows[row][col]; {
				case v == "RUNNING" || v == "idle" || v == "mixed":
					return s.Foreground(good)
				case v == "PENDING" || strings.HasPrefix(v, "alloc"):
					return s.Foreground(warn)
				case strings.HasPrefix(v, "down") || strings.HasPrefix(v, "drain"):
					return s.Foreground(bad)
				}
			}
			return s
		}).
		Render()
}

func (m model) viewSessions() string {
	if len(m.sessions) == 0 {
		return mutedSt.Render("No sessions. Start one from Actions; it will be listed here until released.")
	}
	var b strings.Builder
	for i, s := range m.sessions {
		state := s.Job.State
		col := good
		switch {
		case s.Err != nil:
			state, col = "unknown (offline)", warn
		case !s.Alive:
			state, col = "ended — g to clean up", bad
		case state == "":
			state = "RUNNING"
		}
		cursor := "  "
		id := s.JobID
		if i == m.cursor[tabSessions] {
			cursor = selSt.Render("▸ ")
			id = selSt.Render(id)
		}
		node := s.Job.NodeList
		if node == "" {
			node = s.Node
		}
		fmt.Fprintf(&b, "%s%-10s %-8s %-8s %s  %s  %s\n", cursor, id, s.Kind, s.Profile,
			lipgloss.NewStyle().Foreground(col).Render(state), node,
			mutedSt.Render("started "+s.Created.Format("Jan 2 15:04")))
		if s.URL != "" {
			b.WriteString(mutedSt.Render("    "+s.URL) + "\n")
		} else if s.Mount != "" {
			b.WriteString(mutedSt.Render("    mounted at "+s.Mount) + "\n")
		}
	}
	return b.String()
}

func (m model) viewSettings() string {
	var b strings.Builder
	b.WriteString(mutedSt.Render("Saved to "+m.cfg.Path+". Profiles beyond cpu/gpu: `cluster config`.") + "\n\n")
	for i, f := range fields {
		label := fmt.Sprintf("%-15s", f.label)
		cursor := "  "
		if i == m.cursor[tabSettings] {
			cursor = selSt.Render("▸ ")
			label = selSt.Render(label)
		} else {
			label = mutedSt.Render(label)
		}
		b.WriteString(cursor + label + " " + m.inputs[i].View() + "\n")
	}
	return b.String()
}

func truncate(s string, w int) string {
	if w <= 1 || lipgloss.Width(s) <= w {
		return s
	}
	r := []rune(s)
	if len(r) > w-1 {
		r = r[:w-1]
	}
	return string(r) + "…"
}
