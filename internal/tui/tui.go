// Package tui is the interactive dashboard. It never runs long operations
// itself: actions re-invoke this binary as a subprocess with the terminal
// handed over (tea.ExecProcess), and queries run as tea.Cmds whose results
// come back as messages, so all model state is touched only in Update.
package tui

import (
	"context"
	"fmt"
	"os/exec"
	"path"
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
	"github.com/lunar-photon/cluster/internal/session"
	"github.com/lunar-photon/cluster/internal/slurm"
)

const (
	tabActions = iota
	tabCluster
	tabJobs
	tabSessions
	tabSettings
	numTabs
)

var tabNames = []string{"Actions", "Cluster", "Jobs", "Sessions", "Settings"}

type action struct {
	key   string
	label string
	desc  string
	args  []string
}

var actions = []action{
	{"t", "Node terminal", "Shell on a compute node; survives Wi-Fi drops", []string{"terminal"}},
	{"T", "Node terminal (GPU)", "Same, on a GPU node", []string{"terminal", "--gpu"}},
	{"d", "Node terminal + cluster mount", "Also mounts cluster storage locally for the session", []string{"terminal", "--mount", "--browse"}},
	{"D", "Node terminal + cluster mount (GPU)", "Same, on a GPU node", []string{"terminal", "--mount", "--browse", "--gpu"}},
	{"j", "Jupyter session on node", "JupyterLab in your browser; asks which partition", []string{"jupyter"}},
	{"J", "Jupyter session on node (GPU)", "Same, on a GPU node", []string{"jupyter", "--gpu"}},
	{"m", "Mount / unmount storage", "Toggle the sshfs mount", nil},
	{"p", "Push code ->", "rsync this directory to the cluster", []string{"sync", "push"}},
	{"P", "Pull code <-", "rsync it back", []string{"sync", "pull"}},
}

// ---- messages ----

type refreshMsg struct {
	jobs     []slurm.Job
	live     map[string]slurm.Live
	info     *slurm.Info
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
	{"Time limit", func(c *config.Config) string { return c.Slurm.Time }, func(c *config.Config, v string) error { c.Slurm.Time = v; return nil }},
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
	cursor      [numTabs]int
	width       int
	height      int
	mounted     bool
	connected   bool
	refreshing  bool
	lastRefresh time.Time

	jobs     []slurm.Job
	info     *slurm.Info
	sums     []slurm.Summary
	sessions []lifecycle.Status

	status  string
	errMsg  string
	inputs  []textinput.Model
	menu    *menu // the pop-up for a job or session, when open
	editing bool

	live      map[string]slurm.Live // output state of running jobs
	filter    textinput.Model       // job-name glob for the Jobs tab
	filtering bool

	// skew is the cluster's clock minus ours, so live timers match Slurm's.
	skew time.Duration
	// stream follows the selected running job's output while the Jobs tab
	// is open; streamLines/streamAt are what it has delivered.
	stream      *outStream
	streamLines []string
	streamAt    time.Time
}

// Run starts the dashboard. r may be nil when no host is configured.
func Run(cfg config.Config, r *remote.Client, self string) error {
	m := model{cfg: cfg, self: self, tab: tabCluster}
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
	m.filter = textinput.New()
	m.filter.Prompt = "/"
	m.filter.Placeholder = "name glob, e.g. proj_*"
	m.filter.CharLimit = 128
	if m.env == nil {
		m.tab = tabSettings
		m.status = "No cluster host configured — fill in Host and press Ctrl+S (or run `cluster init`)."
	}
	m.mounted = mount.IsMounted(cfg.LocalMountPoint())
	final, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if fm, ok := final.(model); ok && fm.stream != nil {
		fm.stream.stop()
	}
	return err
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tick(), autoRefresh())
}

// tick redraws every second for the live timers; it also rechecks the mount.
func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
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
	preferred := ""
	if p, err := m.cfg.Profile(""); err == nil {
		preferred = p.QOS
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var msg refreshMsg
		msg.jobs, msg.err = env.Slurm.Jobs(ctx)
		if msg.err == nil {
			var running []string
			for _, j := range msg.jobs {
				if j.State == "RUNNING" {
					running = append(running, j.ID)
				}
			}
			if len(running) > 0 {
				if ls, err := env.Slurm.LiveJobs(ctx, running...); err == nil {
					msg.live = map[string]slurm.Live{}
					paths := map[string]session.LogPath{}
					for _, l := range ls {
						msg.live[l.ID] = l
						if l.StdOut != "" {
							paths[l.ID] = session.LogPath{Path: l.StdOut, Name: l.Name, Seen: time.Now()}
						}
					}
					_ = session.DefaultLogCache().Remember(paths)
				}
			}
		}
		if msg.err == nil {
			msg.info, msg.err = env.Slurm.ClusterInfo(ctx)
			if msg.info != nil {
				msg.info.PreferredQOS = preferred
			}
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
			m.jobs, m.info, m.live = msg.jobs, msg.info, msg.live
			m.sums = m.info.Summarize()
			for _, l := range m.live {
				if !l.Now.IsZero() {
					m.skew = l.Now.Sub(time.Now()).Round(time.Second)
					break
				}
			}
		}
		m.sessions = msg.sessions
		m.clampCursors()
		cmd := m.syncStream() // before returning m: it updates m
		return m, cmd

	case streamMsg:
		if msg.s != m.stream {
			return m, nil // from a stream already replaced
		}
		m.streamLines = append(m.streamLines, msg.lines...)
		if over := len(m.streamLines) - 300; over > 0 {
			m.streamLines = append([]string(nil), m.streamLines[over:]...)
		}
		m.streamAt = time.Now()
		return m, m.stream.listen()

	case streamEndMsg:
		if msg.s == m.stream {
			m.stream = nil // restarted on the next refresh if the job still runs
		}
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
		var next tea.Model
		var cmd tea.Cmd
		switch {
		case m.menu != nil:
			next, cmd = m.updateMenu(msg)
		case m.editing:
			next, cmd = m.updateEditing(msg)
		case m.filtering:
			next, cmd = m.updateFilter(msg)
		default:
			next, cmd = m.updateKeys(msg)
		}
		nm := next.(model)
		scmd := nm.syncStream() // before returning nm: it updates nm
		return nm, tea.Batch(cmd, scmd)
	}
	return m, nil
}

// syncStream keeps exactly one output stream: for the selected running job
// while the Jobs tab is shown, none otherwise.
func (m *model) syncStream() tea.Cmd {
	want, path := "", ""
	if m.tab == tabJobs && m.env != nil {
		if jobs := m.visibleJobs(); m.cursor[tabJobs] < len(jobs) {
			j := jobs[m.cursor[tabJobs]]
			if l, ok := m.live[j.ID]; ok && j.State == "RUNNING" && l.StdOut != "" {
				want, path = j.ID, l.StdOut
			}
		}
	}
	if m.stream != nil && m.stream.jobID == want {
		return nil
	}
	if m.stream != nil {
		m.stream.stop()
		m.stream = nil
	}
	m.streamLines, m.streamAt = nil, time.Time{}
	if want == "" {
		return nil
	}
	s, err := startStream(m.env.R, want, path, 30)
	if err != nil {
		return nil // the 30-second snapshot still shows the output
	}
	m.stream = s
	return s.listen()
}

// clusterNow is the cluster's current time.
func (m model) clusterNow() time.Time { return time.Now().Add(m.skew) }

// liveJob is a running job's output state brought up to this second, with
// anything the stream delivered since the last refresh.
func (m model) liveJob(id string) (slurm.Live, bool) {
	l, ok := m.live[id]
	if !ok {
		return l, false
	}
	l.Now = m.clusterNow()
	if m.stream != nil && m.stream.jobID == id && len(m.streamLines) > 0 {
		l.Tail = m.streamLines
		if p := slurm.ParseProgress(m.streamLines); p != nil {
			l.Progress = p
		}
		if at := m.streamAt.Add(m.skew); at.After(l.MTime) {
			l.MTime = at
		}
	}
	return l, true
}

// timeCell is the live TIME / LIMIT text: elapsed and left for running jobs,
// time waited for pending ones. near is true in a running job's last 10
// minutes.
func timeCell(j slurm.Job, l slurm.Live, haveLive bool, now time.Time) (text string, near bool) {
	switch {
	case j.State == "RUNNING" && haveLive && !l.Start.IsZero():
		elapsed := now.Sub(l.Start)
		if l.TimeLimit <= 0 {
			return slurm.FormatDuration(elapsed) + " / no limit", false
		}
		left := l.Start.Add(l.TimeLimit).Sub(now)
		return fmt.Sprintf("%s / %s · %s left", slurm.FormatDuration(elapsed), slurm.FormatDuration(l.TimeLimit),
			slurm.FormatDuration(left)), left < 10*time.Minute
	case j.State == "PENDING" && !j.Submit.IsZero():
		return "waiting " + slurm.FormatDuration(now.Sub(j.Submit)) + " · limit " + j.TimeLimit, false
	}
	return j.Time + " / " + j.TimeLimit, false
}

func (m model) updateKeys(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	switch key {
	case "q", "ctrl+c", "esc":
		return m, tea.Quit
	case "tab", "right":
		m.tab = (m.tab + 1) % numTabs
		return m, nil
	case "shift+tab", "left":
		m.tab = (m.tab + numTabs - 1) % numTabs
		return m, nil
	case "1", "2", "3", "4", "5":
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
	case tabCluster:
		if len(m.sums) == 0 {
			break
		}
		s := m.sums[m.cursor[tabCluster]]
		switch key {
		case "enter", "t":
			return m.inPartition(s, "Node terminal on "+s.Name, "terminal")
		case "j":
			return m.inPartition(s, "Jupyter on "+s.Name, "jupyter")
		}
	case tabJobs:
		if key == "/" {
			m.filtering = true
			return m, m.filter.Focus()
		}
		jobs := m.visibleJobs()
		if len(jobs) == 0 {
			break
		}
		j := jobs[m.cursor[tabJobs]]
		switch key {
		case "enter":
			m.menu = jobMenu(j)
			return m, nil
		case "l":
			return m, m.run("Following job "+j.ID, "logs", "-f", j.ID)
		case "x":
			return m.menuAction(jobMenu(j), "x")
		}
	case tabSessions:
		if len(m.sessions) == 0 {
			break
		}
		s := m.sessions[m.cursor[tabSessions]]
		switch key {
		case "enter":
			m.menu = sessionMenu(s)
			return m, nil
		case "a":
			return m, m.run("Attach "+s.JobID, "attach", s.JobID)
		case "x":
			return m.menuAction(sessionMenu(s), "x") // asks before stopping
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

// inPartition starts a session in the selected partition, adding the QOS
// the partition requires and a GPU when it has them.
func (m model) inPartition(s slurm.Summary, what, sub string) (tea.Model, tea.Cmd) {
	if !s.Allowed {
		m.errMsg = "You can't use " + s.Name + ": " + s.Why
		return m, nil
	}
	return m, m.run(what, m.partitionArgs(s, sub)...)
}

// partitionArgs are the command-line arguments to start sub in partition s.
func (m model) partitionArgs(s slurm.Summary, sub string) []string {
	args := []string{sub, "-p", s.Name}
	profile := ""
	if sub == "jupyter" {
		profile = m.cfg.Jupyter.Profile
	}
	// The usual time limit may be longer than this partition allows.
	if p, err := m.cfg.Profile(profile); err == nil {
		if want, limit := slurm.ParseDuration(p.Time), slurm.ParseDuration(s.MaxTime); limit > 0 && want > limit {
			args = append(args, "-t", slurm.FormatDuration(limit))
		}
	}
	if m.info != nil && len(s.AllowedVia) > 0 {
		args = append(args, "-q", m.info.QOSFor(s))
	}
	if s.GPUsTotal > 0 {
		args = append(args, "--gpus", "1")
	}
	return args
}

// updateFilter edits the Jobs tab's name filter; enter or esc finishes.
func (m model) updateFilter(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "enter", "esc":
		m.filtering = false
		m.filter.Blur()
		m.cursor[tabJobs] = 0
		return m, nil
	}
	var cmd tea.Cmd
	m.filter, cmd = m.filter.Update(k)
	m.cursor[tabJobs] = 0
	return m, cmd
}

// visibleJobs are the jobs matching the Jobs tab's filter.
func (m model) visibleJobs() []slurm.Job {
	pattern := strings.TrimSpace(m.filter.Value())
	if pattern == "" {
		return m.jobs
	}
	var out []slurm.Job
	for _, j := range m.jobs {
		if ok, err := path.Match(pattern, j.Name); err == nil && ok {
			out = append(out, j)
		}
	}
	return out
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
	if a.key == "j" && len(m.sums) > 0 {
		m.menu = m.partitionMenu(a.label, "jupyter", m.cfg.Jupyter.Profile)
		return m, nil
	}
	return m, m.run(a.label, a.args...)
}

func (m *model) clampCursors() {
	limits := [numTabs]int{len(actions), len(m.sums), len(m.visibleJobs()), len(m.sessions), len(fields)}
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
	faint    = lipgloss.AdaptiveColor{Light: "#cccccc", Dark: "#444444"}
	good     = lipgloss.AdaptiveColor{Light: "#2b8a3e", Dark: "#8ce99a"}
	bad      = lipgloss.AdaptiveColor{Light: "#c92a2a", Dark: "#ff8787"}
	warn     = lipgloss.AdaptiveColor{Light: "#e67700", Dark: "#ffd43b"}
	titleSt  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#000000")).Background(accent).Padding(0, 1)
	mutedSt  = lipgloss.NewStyle().Foreground(muted)
	accentSt = lipgloss.NewStyle().Foreground(accent).Bold(true)
	selSt    = lipgloss.NewStyle().Foreground(accent).Bold(true)
	goodSt   = lipgloss.NewStyle().Foreground(good)
	warnSt   = lipgloss.NewStyle().Foreground(warn)
	badSt    = lipgloss.NewStyle().Foreground(bad)
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
	} else if m.info != nil && m.info.User != "" && !strings.Contains(host, "@") {
		host = m.info.User + "@" + host
	}
	conn := badSt.Render("● offline")
	if m.connected {
		conn = goodSt.Render("● connected")
	}
	mnt := mutedSt.Render("not mounted")
	if m.mounted {
		mnt = goodSt.Render("mounted at " + m.cfg.MountPoint)
	}
	left := titleSt.Render("cluster") + " " + accentSt.Render(host) + "  " + conn
	right := "drive: " + mnt
	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right))
	b.WriteString(left + strings.Repeat(" ", gap) + right + "\n")

	var tabs []string
	for i, n := range tabNames {
		label := fmt.Sprintf("%d %s", i+1, n)
		switch {
		case i == tabJobs && len(m.jobs) > 0:
			label += fmt.Sprintf(" (%d)", len(m.jobs))
		case i == tabSessions && len(m.sessions) > 0:
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
	case tabCluster:
		body = m.viewCluster()
	case tabJobs:
		body = m.viewJobs(bodyHeight)
	case tabSessions:
		body = m.viewSessions()
	case tabSettings:
		body = m.viewSettings()
	}
	body = lipgloss.NewStyle().Height(bodyHeight).MaxHeight(bodyHeight).Render(body)
	if m.menu != nil {
		body = overlay(body, m.menuView(), bodyHeight, m.width)
	}
	b.WriteString(body + "\n")

	switch {
	case m.errMsg != "":
		b.WriteString(badSt.Render(truncate(m.errMsg, m.width)) + "\n")
	case m.status != "":
		b.WriteString(warnSt.Render(truncate(m.status, m.width)) + "\n")
	default:
		b.WriteString("\n")
	}
	b.WriteString(mutedSt.Render(truncate(m.help(), m.width)))
	return b.String()
}

// menuAction runs one item of a menu directly, by its key.
func (m model) menuAction(mn *menu, key string) (tea.Model, tea.Cmd) {
	for _, it := range mn.items {
		if it.key == key {
			return it.run(m)
		}
	}
	return m, nil
}

func (m model) help() string {
	if m.menu != nil {
		return "↑↓ select · enter choose · letter shortcut · esc close"
	}
	common := "tab/1-5 switch · r refresh · c connect · q quit"
	switch m.tab {
	case tabActions:
		return "↑↓ select · enter run · letter shortcut · " + common
	case tabCluster:
		return "↑↓ partition · t/enter node terminal there · j Jupyter there · " + common
	case tabJobs:
		if m.filtering {
			return "type a name glob · enter/esc done"
		}
		return "↑↓ select · enter options · l follow output · x cancel · / filter by name · " + common
	case tabSessions:
		return "↑↓ select · enter options · a connect · x stop · g clean up · " + common
	case tabSettings:
		if m.editing {
			return "type to edit · enter/↓ next · ↑ previous · ctrl+s save · esc done"
		}
		return "↑↓ select · enter edit · ctrl+s save · " + common
	}
	return common
}

// summary finds the summary for a partition name ("" = the default partition).
func (m model) summary(name string) (slurm.Summary, bool) {
	for _, s := range m.sums {
		if s.Name == name || (name == "" && s.Default) {
			return s, true
		}
	}
	return slurm.Summary{}, false
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
		fmt.Fprintf(&b, "%s%s  %s %s\n", cursor, accentSt.Render(a.key), label+strings.Repeat(" ", max(1, 37-len(a.label))), mutedSt.Render(a.desc))
	}

	// What the buttons will ask for, where those values come from, and
	// whether the request is free right now.
	source := "built-in defaults"
	if m.cfg.Source != "" {
		source = "your config, " + config.ShortPath(m.cfg.Source)
	}
	b.WriteString("\n" + accentSt.Render("What these request") + "\n")
	b.WriteString(hang(mutedSt.Render("From "+source+" · change in Settings, or per session with -c -m -t -p -q"), "  ", m.width))
	for _, prof := range []struct{ label, name string }{{"CPU", ""}, {"GPU", "gpu"}} {
		p, err := m.cfg.Profile(prof.name)
		if err != nil {
			fmt.Fprintf(&b, "  %-4s %s\n", prof.label, mutedSt.Render("no GPU profile configured"))
			continue
		}
		s, known := m.summary(p.Partition)
		cores := 0
		if known {
			cores = nodeCores(s)
		}
		b.WriteString(hang(fmt.Sprintf("%-4s ", prof.label)+requestText(p, cores, m.info == nil || m.info.MemTracked), "  ", m.width))
		const indent = "       "
		if prof.name != "" {
			if inherited := inheritedFields(m.cfg, prof.name, m.info == nil || m.info.MemTracked); inherited != "" {
				b.WriteString(hang(mutedSt.Render(inherited), indent, m.width))
			}
		}
		if known {
			b.WriteString(hang(m.availabilityHint(s, p), indent, m.width))
		}
	}
	return b.String()
}

// hang wraps text to the terminal width with every line indented by
// prefix, and a further 5 columns on continuation lines.
func hang(text, prefix string, width int) string {
	w := max(20, width-len(prefix))
	lines := strings.Split(lipgloss.NewStyle().Width(w).Render(text), "\n")
	var b strings.Builder
	for i, l := range lines {
		l = strings.TrimRight(l, " ")
		if i > 0 {
			l = "     " + strings.TrimLeft(l, " ")
		}
		b.WriteString(prefix + l + "\n")
	}
	return b.String()
}

// requestText labels every value, e.g. "partition medium · 4 of 28 cores on
// one node · 8G memory · 04:00:00 time limit · QOS normal". nodeCores is
// the partition's cores per node, or 0 when unknown or mixed. Memory is left
// out when showMem is false (the cluster doesn't track memory per job).
func requestText(p config.Slurm, nodeCores int, showMem bool) string {
	part := "default partition"
	if p.Partition != "" {
		part = "partition " + p.Partition
	}
	parts := []string{part}
	if p.CPUs > 0 {
		parts = append(parts, coresText(p.CPUs, nodeCores))
	}
	if p.GPUs > 0 {
		parts = append(parts, slurm.Plural(p.GPUs, "GPU"))
	}
	if showMem {
		mem := "default memory"
		if p.Mem != "" {
			mem = p.Mem + " memory"
		}
		parts = append(parts, mem)
	}
	tm := "the partition's time limit"
	if p.Time != "" {
		tm = p.Time + " time limit"
	}
	parts = append(parts, tm)
	if p.QOS != "" {
		parts = append(parts, "QOS "+p.QOS)
	}
	return strings.Join(parts, " · ")
}

// coresText is "4 of 28 cores on one node", or "4 cores on one node" when
// the node size is unknown.
func coresText(cores, nodeCores int) string {
	if nodeCores > 0 {
		return fmt.Sprintf("%d of %d cores on one node", cores, nodeCores)
	}
	return slurm.Plural(cores, "core") + " on one node"
}

// nodeCores is the cores per node in a partition when all its nodes match.
func nodeCores(s slurm.Summary) int {
	if len(s.Shapes) == 0 {
		return 0
	}
	n := s.Shapes[0].CPUs
	for _, sh := range s.Shapes[1:] {
		if sh.CPUs != n {
			return 0
		}
	}
	return n
}

// inheritedFields says which values a profile takes from [slurm] rather than
// setting itself, e.g. "CPUs, memory and time limit come from the CPU settings".
func inheritedFields(cfg config.Config, profile string, showMem bool) string {
	own, ok := cfg.Profiles[profile]
	if !ok {
		return ""
	}
	var names []string
	if own.CPUs == 0 && cfg.Slurm.CPUs != 0 {
		names = append(names, "cores")
	}
	if own.Mem == "" && cfg.Slurm.Mem != "" && showMem {
		names = append(names, "memory")
	}
	if own.Time == "" && cfg.Slurm.Time != "" {
		names = append(names, "time limit")
	}
	if own.Account == "" && cfg.Slurm.Account != "" {
		names = append(names, "account")
	}
	if len(names) == 0 {
		return ""
	}
	s, verb := names[0], "comes"
	if len(names) > 1 {
		s, verb = strings.Join(names[:len(names)-1], ", ")+" and "+names[len(names)-1], "come"
	} else if s == "cores" {
		verb = "come"
	}
	return strings.ToUpper(s[:1]) + s[1:] + " " + verb + " from the CPU settings"
}

// availabilityHint says whether a request would start now in partition s.
func (m model) availabilityHint(s slurm.Summary, p config.Slurm) string {
	if !s.Allowed {
		return badSt.Render("you can't use " + s.Name + ": " + s.Why)
	}
	a := m.info.AvailableTo(s)
	text := fmt.Sprintf("%s now: %s free on %s", s.Name, slurm.Plural(s.CPUsFree, "core"), slurm.Plural(s.Idle+s.Mixed, "node"))
	if s.GPUsTotal > 0 {
		text += ", " + slurm.Plural(s.GPUsFree, "GPU") + " free"
	}
	need := max(1, p.CPUs)
	switch {
	case s.BestFreeCPUs >= need && a.CPUs >= need && (p.GPUs == 0 || a.GPUs >= p.GPUs):
		return goodSt.Render("● " + text + " — should start right away")
	case a.CPUs < need && a.LimitedBy != "free resources":
		return warnSt.Render("● " + text + " — your " + a.LimitedBy + " is used up; it will queue")
	default:
		return warnSt.Render("● " + text + " — expect to wait in the queue")
	}
}

func (m model) viewCluster() string {
	if m.env == nil {
		return mutedSt.Render("No host configured.")
	}
	if m.info == nil {
		return mutedSt.Render("Loading cluster information…")
	}
	var b strings.Builder
	b.WriteString(accentSt.Render("You") + "  " + m.info.AccountText() + "\n")
	for _, line := range m.info.UsageText() {
		b.WriteString("     " + mutedSt.Render(line) + "\n")
	}
	if pol := m.cfg.EffectivePolicy(); !pol.Empty() {
		src := "your config"
		if m.cfg.Project != nil && !m.cfg.Project.Policy.Empty() {
			src = config.ShortPath(m.cfg.Project.Path)
		}
		b.WriteString(hang(warnSt.Render("Rules")+" "+mutedSt.Render("("+src+"): ")+strings.Join(pol.Lines(), " · "), "", m.width))
	}
	when := m.lastRefresh.Format("15:04:05")
	b.WriteString("\n" + accentSt.Render("Partitions") + mutedSt.Render("  updated "+when+", every 30s · nodes: idle/partly used/full/unavailable") + "\n")

	sel := m.cursor[tabCluster]
	rows := make([][]string, 0, len(m.sums))
	for _, s := range m.sums {
		name := s.Name
		if s.Default {
			name += "*"
		}
		gpus := "-"
		if s.GPUsTotal > 0 {
			gpus = fmt.Sprintf("%d/%d", s.GPUsFree, s.GPUsTotal)
		}
		mine := "-"
		if s.Allowed {
			a := m.info.AvailableTo(s)
			mine = slurm.Plural(a.CPUs, "CPU")
			if s.GPUsTotal > 0 {
				mine += ", " + slurm.Plural(a.GPUs, "GPU")
			}
		}
		rows = append(rows, []string{
			name, s.AccessText(), slurm.Unlimited(s.MaxTime), s.StateText(),
			freeBar(s.CPUsFree, s.CPUsTotal) + fmt.Sprintf(" %d/%d", s.CPUsFree, s.CPUsTotal),
			gpus, mine, compactShape(s),
		})
	}
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(mutedSt).
		Headers("PARTITION", "ACCESS", "TIME LIMIT", "NODES", "FREE CPUs", "GPUs", "FOR YOU NOW", "EACH NODE").
		Rows(rows...).
		StyleFunc(func(row, col int) lipgloss.Style {
			st := lipgloss.NewStyle().Padding(0, 1)
			switch {
			case row == table.HeaderRow:
				return st.Bold(true).Foreground(accent)
			case row == sel:
				st = st.Bold(true).Foreground(accent)
			case row >= 0 && row < len(m.sums) && !m.sums[row].Allowed:
				st = st.Foreground(muted)
			}
			if col == 1 && row >= 0 && row < len(m.sums) && row != sel {
				if m.sums[row].Allowed {
					st = st.Foreground(good)
				}
			}
			return st
		})
	b.WriteString(t.Render() + "\n")

	if sel < len(m.sums) {
		b.WriteString("\n")
		detail := m.info.Detail(m.sums[sel])
		b.WriteString(accentSt.Render(detail[0]) + "\n")
		wrap := lipgloss.NewStyle().Width(max(20, m.width-4))
		for _, line := range detail[1:] {
			b.WriteString(wrap.Render("  "+line) + "\n")
		}
	}
	return b.String()
}

// freeBar draws free capacity as a 10-cell bar: green cells are free.
func freeBar(free, total int) string {
	if total <= 0 {
		return ""
	}
	const cells = 10
	n := free * cells / total
	if free > 0 && n == 0 {
		n = 1
	}
	return goodSt.Render(strings.Repeat("█", n)) + lipgloss.NewStyle().Foreground(faint).Render(strings.Repeat("█", cells-n))
}

// compactShape is a one-line node description for the table, e.g.
// "28 cores · 2×14 · 125 GB · 1-2 tesla GPUs".
func compactShape(s slurm.Summary) string {
	if len(s.Shapes) == 0 {
		return "-"
	}
	sh := s.Shapes[0]
	text := fmt.Sprintf("%d cores · %d×%d · %s", sh.CPUs, sh.Sockets, sh.CoresPerSocket, slurm.FormatMem(sh.MemMB))
	minG, maxG := sh.GPUs, sh.GPUs
	mixed := false
	for _, o := range s.Shapes[1:] {
		if o.CPUs != sh.CPUs || o.MemMB != sh.MemMB || o.Sockets != sh.Sockets {
			mixed = true
		}
		minG, maxG = min(minG, o.GPUs), max(maxG, o.GPUs)
	}
	if maxG > 0 {
		g := strconv.Itoa(maxG)
		if minG != maxG {
			g = fmt.Sprintf("%d-%d", minG, maxG)
		}
		t := sh.GPUType
		if t != "" {
			t += " "
		}
		text += " · " + g + " " + t + "GPU"
		if maxG != 1 {
			text += "s"
		}
	}
	if mixed {
		text += " (+ other node types)"
	}
	return text
}

func (m model) viewJobs(height int) string {
	if m.env == nil {
		return mutedSt.Render("No host configured.")
	}
	var b strings.Builder
	jobs := m.visibleJobs()
	running, cpus, gpus := 0, 0, 0
	for _, j := range jobs {
		if j.State == "RUNNING" {
			running++
			n, _ := strconv.Atoi(j.CPUs)
			cpus += n
			gpus += j.GPUs()
		}
	}
	summary := fmt.Sprintf("%s, %d running using %s", slurm.Plural(len(jobs), "job"), running, slurm.Plural(cpus, "core"))
	if gpus > 0 {
		summary += " and " + slurm.Plural(gpus, "GPU")
	}
	b.WriteString(accentSt.Render("Your jobs") + "  " + mutedSt.Render(summary))
	if m.filtering || m.filter.Value() != "" {
		b.WriteString("   " + m.filter.View())
	}
	b.WriteString("\n")
	if len(jobs) == 0 {
		b.WriteString(mutedSt.Render("  no jobs") + "\n")
		return b.String()
	}

	showGPUs := false
	for _, j := range jobs {
		showGPUs = showGPUs || j.GPUs() > 0
	}
	sel := m.cursor[tabJobs]
	maxRows := max(1, height-14)
	headers := []string{"JOB", "NAME", "STATE", "CORES", "TIME / LIMIT", "NODE (REASON)", "LAST OUTPUT", "PROGRESS"}
	if showGPUs {
		headers = append(headers[:4], append([]string{"GPUs"}, headers[4:]...)...)
	}
	rows := [][]string{}
	near := map[int]bool{}
	now := m.clusterNow()
	first := max(0, min(sel-maxRows/2, len(jobs)-maxRows))
	for i := first; i < len(jobs) && i < first+maxRows; i++ {
		j := jobs[i]
		last, prog := "", ""
		l, haveLive := m.liveJob(j.ID)
		if haveLive {
			if age, ok := l.LastOutputAge(); ok {
				last = slurm.HumanDuration(age) + " ago"
			}
			prog = l.ProgressText()
		}
		tm, isNear := timeCell(j, l, haveLive, now)
		near[len(rows)] = isNear
		row := []string{j.ID, j.Name, j.State, j.CPUs, tm, j.NodeList, last, prog}
		if showGPUs {
			g := "-"
			if n := j.GPUs(); n > 0 {
				g = strconv.Itoa(n)
			}
			row = append(row[:4], append([]string{g}, row[4:]...)...)
		}
		rows = append(rows, row)
	}
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(mutedSt).
		Headers(headers...).
		Rows(rows...).
		StyleFunc(func(row, col int) lipgloss.Style {
			st := lipgloss.NewStyle().Padding(0, 1)
			if row == table.HeaderRow {
				return st.Bold(true).Foreground(accent)
			}
			if row+first == sel {
				st = st.Bold(true).Foreground(accent)
			}
			if col == 2 && row >= 0 && row < len(rows) && row+first != sel {
				switch rows[row][2] {
				case "RUNNING":
					return st.Foreground(good)
				case "PENDING":
					return st.Foreground(warn)
				}
			}
			if headers[col] == "PROGRESS" && row >= 0 && row < len(rows) && strings.Contains(rows[row][col], "past limit") {
				return st.Foreground(bad)
			}
			if headers[col] == "TIME / LIMIT" && near[row] {
				return st.Foreground(warn)
			}
			return st
		})
	b.WriteString(t.Render() + "\n")
	if len(jobs) > maxRows {
		b.WriteString(mutedSt.Render(fmt.Sprintf("  showing %d-%d of %d", first+1, min(first+maxRows, len(jobs)), len(jobs))) + "\n")
	}

	// The selected job's recent output, live while the stream runs.
	if sel < len(jobs) {
		j := jobs[sel]
		if l, ok := m.liveJob(j.ID); ok && len(l.Tail) > 0 {
			label := mutedSt.Render(" (refreshed every 30 s)")
			if m.stream != nil && m.stream.jobID == j.ID {
				label = goodSt.Render(" ● live")
			}
			b.WriteString("\n" + accentSt.Render("Output of "+j.ID) + label + "  " + mutedSt.Render(l.StdOut) + "\n")
			show := max(6, height-strings.Count(b.String(), "\n")-1)
			for _, line := range l.Tail[max(0, len(l.Tail)-show):] {
				b.WriteString(mutedSt.Render(truncate("  "+line, m.width)) + "\n")
			}
		} else if j.State == "PENDING" {
			b.WriteString("\n" + mutedSt.Render("  "+j.ID+" is waiting: "+strings.Trim(j.NodeList, "()")) + "\n")
		}
	}
	return b.String()
}

func (m model) viewSessions() string {
	if len(m.sessions) == 0 {
		return mutedSt.Render("No sessions. Start one from Actions or Cluster; it will be listed here until released.")
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
		fmt.Fprintf(&b, "%s%-10s %-15s %-8s %s  %s  %s\n", cursor, id, s.Kind, s.Profile,
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
	b.WriteString(mutedSt.Render("Saved to "+m.cfg.Path+". Blank fields use the cluster's defaults. Profiles beyond cpu/gpu: `cluster config`.") + "\n\n")
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
