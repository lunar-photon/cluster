package lifecycle

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/lunar-photon/cluster/internal/remote"
	"github.com/lunar-photon/cluster/internal/session"
	"github.com/lunar-photon/cluster/internal/slurm"
)

type JupyterOpts struct {
	Req     slurm.Request
	Profile string
	Dir     string
	Port    int
	Browse  bool
}

// jupyterScript is the batch job that starts JupyterLab on a compute node
// and publishes its location in ~/.cluster/jupyter-<jobid>.json (home is
// shared between login and compute nodes). The token travels in the
// environment, not argv, so other users cannot read it from the process list.
func jupyterScript(setup, dir, token string) string {
	cd := `"$HOME"`
	switch {
	case dir == "" || dir == "~":
	case strings.HasPrefix(dir, "~/"):
		cd = `"$HOME"/` + remote.Quote(dir[2:])
	default:
		cd = remote.Quote(dir)
	}
	return fmt.Sprintf(`#!/bin/bash -l
# Started by cluster jupyter.
umask 077
INFO="$HOME/.cluster/jupyter-$SLURM_JOB_ID.json"
%[1]s
if ! command -v jupyter >/dev/null 2>&1; then
  echo "jupyter is not on PATH; set [jupyter] setup in your cluster config (e.g. module load / conda activate)" >&2
  exit 1
fi
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')
NODE=$(hostname)
printf '{"host":"%%s","port":%%s}\n' "$NODE" "$PORT" > "$INFO.tmp" && mv "$INFO.tmp" "$INFO"
export JUPYTER_TOKEN=%[2]s
cd %[3]s || exit 1
exec jupyter lab --no-browser --ip=0.0.0.0 --port="$PORT" --ServerApp.port_retries=0
`, setup, token, cd)
}

type jupyterInfo struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Jupyter starts JupyterLab as a Slurm job on a compute node (never on the
// login node), tunnels it, opens the browser, and keeps the tunnel alive
// until Ctrl+C or the terminal closes.
func (e *Env) Jupyter(o JupyterOpts) error {
	if err := e.R.EnsureMaster(); err != nil {
		return err
	}
	ctx, which, stop := interrupts()
	defer stop()

	token := randHex(24)
	sess := session.Session{
		Kind:    session.Jupyter,
		Host:    e.R.Host,
		Profile: o.Profile,
		Created: time.Now(),
		Token:   token,
	}
	if o.Port != 0 {
		sess.LocalPort = o.Port
	}

	if _, err := e.R.Output(ctx, "mkdir -p ~/.cluster && chmod 700 ~/.cluster", nil); err != nil {
		return err
	}

	o.Req.Name = e.Cfg.JobName("jupyter")
	if err := e.CheckRequest(ctx, o.Req); err != nil {
		return err
	}
	id, err := e.Slurm.Submit(ctx, o.Req, jupyterScript(e.Cfg.Jupyter.Setup, o.Dir, token),
		"--output=.cluster/jupyter-%j.log")
	if err != nil {
		return err
	}
	sess.JobID = id
	sess.RemoteFiles = []string{".cluster/jupyter-" + id + ".log", ".cluster/jupyter-" + id + ".json"}
	e.printf("Submitted Jupyter job %s: %s\n", id, o.Req)
	if err := e.Store.Add(sess); err != nil {
		e.printf("Warning: could not record session: %v\n", err)
	}
	if e.R.DryRun {
		return nil
	}

	info, err := e.waitJupyterInfo(ctx, sess)
	if err != nil {
		if ctx.Err() != nil {
			e.printf("\nInterrupted (%v); shutting down.\n", which())
		}
		_ = e.Release(sess)
		return err
	}
	sess.Node, sess.RemotePort = info.Host, info.Port
	_ = e.Store.Add(sess)
	stop()
	return e.serveJupyter(sess, o.Browse)
}

// waitJupyterInfo waits for the server to publish its host and port,
// reporting queue state and surfacing the job log if it dies first.
func (e *Env) waitJupyterInfo(ctx context.Context, sess session.Session) (jupyterInfo, error) {
	infoPath := ".cluster/jupyter-" + sess.JobID + ".json"
	logPath := ".cluster/jupyter-" + sess.JobID + ".log"
	last := ""
	deadline := time.Now().Add(2 * time.Minute) // only applies once running
	for {
		if ctx.Err() != nil {
			return jupyterInfo{}, ctx.Err()
		}
		out, err := e.R.Output(ctx, "cat "+infoPath+" 2>/dev/null; true", nil)
		if errors.Is(err, remote.ErrConnection) {
			e.printf("\nLost connection while starting.\n")
			if rerr := e.Reconnect(ctx); rerr != nil {
				return jupyterInfo{}, rerr
			}
			continue
		}
		var info jupyterInfo
		if strings.TrimSpace(out) != "" && json.Unmarshal([]byte(out), &info) == nil && info.Port > 0 {
			e.printf("\r\033[KJupyterLab is starting on %s:%d.\n", info.Host, info.Port)
			if job, alive, err := e.Slurm.Alive(ctx, sess.JobID); err == nil && alive {
				e.describeAllocation(job)
			}
			return info, nil
		}

		job, alive, err := e.Slurm.Alive(ctx, sess.JobID)
		if err == nil && !alive {
			tail, _ := e.R.Output(ctx, "tail -n 20 "+logPath+" 2>/dev/null; true", nil)
			return jupyterInfo{}, fmt.Errorf("Jupyter job %s ended before starting; last log lines:\n%s", sess.JobID, tail)
		}
		running := job.State == "RUNNING"
		if status := job.State + " " + job.NodeList; !running && status != last {
			e.printf("\r\033[KWaiting in queue: %s (Ctrl+C to cancel)", status)
			last = status
			deadline = time.Now().Add(2 * time.Minute)
		}
		if running && time.Now().After(deadline) {
			tail, _ := e.R.Output(ctx, "tail -n 20 "+logPath+" 2>/dev/null; true", nil)
			return jupyterInfo{}, fmt.Errorf("Jupyter did not start within 2 minutes; last log lines:\n%s", tail)
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// freePort returns preferred if it can be bound on loopback, else the next free one.
func freePort(preferred int) int {
	if preferred <= 0 {
		preferred = 8888
	}
	for p := preferred; p < preferred+100; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			l.Close()
			return p
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return preferred
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitHTTP(ctx context.Context, url, token string) error {
	client := http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, "GET", url+"/api/status", nil)
		req.Header.Set("Authorization", "token "+token)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("Jupyter did not answer through the tunnel within 3 minutes")
}

func openBrowser(url string) {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	cmd := exec.Command(opener, url)
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

// kernelProbe counts running kernels via the Jupyter API, from the login
// node. The token arrives on stdin so it never appears in argv, and proxy
// settings are ignored because the server is on the cluster network.
const kernelProbe = `import json, sys, urllib.request
host, port = sys.argv[1], sys.argv[2]
token = sys.stdin.readline().strip()
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
req = urllib.request.Request("http://%s:%s/api/kernels" % (host, port), headers={"Authorization": "token " + token})
print(len(json.load(opener.open(req, timeout=10))))
`

func (e *Env) kernelCount(sess session.Session) (int, error) {
	ctx, cancel := timeout(30 * time.Second)
	defer cancel()
	out, err := e.R.Output(ctx, remote.Join("python3", "-c", kernelProbe, sess.Node, strconv.Itoa(sess.RemotePort)),
		strings.NewReader(sess.Token+"\n"))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// keepJupyter decides whether a signal should leave Jupyter running.
// Ctrl+C stops it, after asking if kernels (with in-memory state) are open.
// A closed terminal only stops it when no kernels are open.
func (e *Env) keepJupyter(sess session.Session, sig os.Signal) bool {
	n, err := e.kernelCount(sess)
	if sig == os.Interrupt {
		if err == nil && n == 0 {
			return false
		}
		what := fmt.Sprintf("%d kernel(s) are running", n)
		if err != nil {
			what = "Couldn't check for running kernels"
		}
		if f, ok := e.In.(*os.File); ok && !term.IsTerminal(int(f.Fd())) {
			return false // no one to ask: an interrupt from a script means stop
		}
		e.printf("\n%s. Stopping Jupyter loses their variables (saved notebooks are kept). Stop anyway? [y/N] ", what)
		line, _ := bufio.NewReader(e.In).ReadString('\n')
		return !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y")
	}
	switch {
	case err != nil:
		e.notify(fmt.Sprintf("Terminal closed; couldn't check Jupyter %s (%v), so it was kept. `cluster attach %s` reopens it, `cluster stop %s` ends it.",
			sess.JobID, err, sess.JobID, sess.JobID))
		return true
	case n > 0:
		e.notify(fmt.Sprintf("Terminal closed with %d Jupyter kernel(s) running, so Jupyter %s was kept. `cluster attach %s` reopens it, `cluster stop %s` ends it.",
			n, sess.JobID, sess.JobID, sess.JobID))
		return true
	}
	return false
}

// serveJupyter runs the tunnel, restarting it after network loss, until the
// user stops Jupyter, the terminal closes, or the server goes away.
func (e *Env) serveJupyter(sess session.Session, browse bool) error {
	base := e.Cfg.Jupyter.Port
	if sess.LocalPort != 0 {
		base = sess.LocalPort
	}
	first := true
	for {
		lp := freePort(base)
		if lp != base && !first {
			e.printf("Port %d is taken now; using %d.\n", base, lp)
		}
		base = lp
		url := fmt.Sprintf("http://127.0.0.1:%d", lp)

		ctx, which, stop := interrupts()
		tunnel := e.R.Tunnel(lp, sess.Node, sess.RemotePort)
		tunnel.Stdin, tunnel.Stdout, tunnel.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := tunnel.Start(); err != nil {
			stop()
			return err
		}
		done := make(chan error, 1)
		go func() { done <- tunnel.Wait() }()
		killTunnel := func() {
			_ = tunnel.Process.Kill()
			<-done
		}

		sess.LocalPort, sess.TunnelPID = lp, tunnel.Process.Pid
		sess.URL = url + "/lab?token=" + sess.Token
		_ = e.Store.Add(sess)

		if err := waitHTTP(ctx, url, sess.Token); err != nil && ctx.Err() == nil {
			e.printf("Warning: %v\n", err)
		}
		if ctx.Err() == nil {
			if first {
				e.printf("\nJupyterLab: %s\n", sess.URL)
				e.printf("Ctrl+C stops Jupyter. Closing this terminal keeps it running while kernels are open.\n")
				if browse {
					openBrowser(sess.URL)
				}
			} else {
				e.printf("Tunnel restored: %s\n", sess.URL)
			}
		}
		first = false

		// Wait for a signal, the tunnel dying, or the job ending.
		const (
			gotSignal = iota
			tunnelDied
			jobEnded
		)
		outcome := -1
		alive := time.NewTicker(time.Minute)
		for outcome < 0 {
			select {
			case <-ctx.Done():
				outcome = gotSignal
			case <-done:
				done <- nil // let killTunnel's receive succeed later
				outcome = tunnelDied
				if ctx.Err() != nil {
					outcome = gotSignal // Ctrl+C reaches ssh too
				}
			case <-alive.C:
				c, cancel := context.WithTimeout(ctx, 20*time.Second)
				_, ok, err := e.Slurm.Alive(c, sess.JobID)
				cancel()
				if err == nil && !ok {
					outcome = jobEnded
				}
			}
		}
		alive.Stop()
		killTunnel()
		sess.TunnelPID = 0
		_ = e.Store.Add(sess)

		switch outcome {
		case jobEnded:
			stop()
			e.printf("Jupyter job %s has ended (time limit?).\n", sess.JobID)
			return e.Release(sess)
		case gotSignal:
			sig := which()
			stop()
			if !e.keepJupyter(sess, sig) {
				e.printf("Stopping Jupyter.\n")
				return e.Release(sess)
			}
			if sig != os.Interrupt {
				return nil // terminal closed; Jupyter stays up
			}
			e.printf("Jupyter keeps running.\n")
			continue
		}
		stop()

		// The tunnel died on its own: network loss.
		e.printf("\nTunnel lost; reconnecting (Jupyter keeps running on the cluster)...\n")
		rctx, rwhich, rstop := interrupts()
		err := e.Reconnect(rctx)
		var sig os.Signal
		if err != nil {
			sig = rwhich()
		}
		rstop()
		if err != nil {
			e.printf("Stopped reconnecting (%v). Jupyter %s keeps running on the cluster:\n"+
				"  cluster attach %s   # reopen it\n  cluster stop %s     # end it\n", sig, sess.JobID, sess.JobID, sess.JobID)
			return nil
		}
		c, cancel := timeout(20 * time.Second)
		_, ok, err := e.Slurm.Alive(c, sess.JobID)
		cancel()
		if err == nil && !ok {
			e.printf("Jupyter job %s ended while disconnected.\n", sess.JobID)
			return e.Release(sess)
		}
	}
}
