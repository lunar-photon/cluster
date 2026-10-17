package lifecycle

import (
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
	"strings"
	"time"

	"github.com/lunar-photon/cluster/internal/remote"
	"github.com/lunar-photon/cluster/internal/session"
	"github.com/lunar-photon/cluster/internal/slurm"
)

type JupyterOpts struct {
	// Head runs Jupyter on the login node instead of a Slurm allocation.
	Head    bool
	Req     slurm.Request
	Profile string
	Dir     string
	Port    int
	Browse  bool
}

// jupyterScript starts JupyterLab and publishes its location in
// ~/.cluster/jupyter-<key>.json (home is shared between login and compute
// nodes). The token travels in the environment, not argv, so other users
// cannot read it from the process list.
func jupyterScript(setup, dir, token, key string) string {
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
KEY="${SLURM_JOB_ID:-%[1]s}"
INFO="$HOME/.cluster/jupyter-$KEY.json"
%[2]s
if ! command -v jupyter >/dev/null 2>&1; then
  echo "jupyter is not on PATH; set [jupyter] setup in your cluster config (e.g. module load / conda activate)" >&2
  exit 1
fi
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')
NODE=$(hostname)
printf '{"host":"%%s","port":%%s,"pid":%%s}\n' "$NODE" "$PORT" "$$" > "$INFO.tmp" && mv "$INFO.tmp" "$INFO"
export JUPYTER_TOKEN=%[3]s
cd %[4]s || exit 1
exec jupyter lab --no-browser --ip=0.0.0.0 --port="$PORT" --ServerApp.port_retries=0
`, key, setup, token, cd)
}

type jupyterInfo struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	PID  int    `json:"pid"`
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Jupyter starts JupyterLab on the cluster, tunnels it, opens the browser,
// and keeps the tunnel alive until Ctrl+C or the terminal closes, at which
// point the server and its allocation are released.
func (e *Env) Jupyter(o JupyterOpts) error {
	if err := e.R.EnsureMaster(); err != nil {
		return err
	}
	ctx, which, stop := interrupts()
	defer stop()

	token := randHex(24)
	tag := randHex(4)
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

	var key string
	if o.Head {
		key = tag
		sess.JobID = "head-" + tag
		script := ".cluster/jupyter-" + key + ".sh"
		logf := ".cluster/jupyter-" + key + ".log"
		sess.RemoteFiles = []string{script, logf, ".cluster/jupyter-" + key + ".json"}
		if _, err := e.R.Output(ctx, "umask 077 && cat > "+script,
			strings.NewReader(jupyterScript(e.Cfg.Jupyter.Setup, o.Dir, token, key))); err != nil {
			return err
		}
		// Detached from the ssh session so a network drop doesn't kill it.
		launch := fmt.Sprintf("cd && setsid nohup bash -l %s > %s 2>&1 < /dev/null &", script, logf)
		if _, err := e.R.Output(ctx, launch, nil); err != nil {
			return err
		}
		e.printf("Starting JupyterLab on the login node %s...\n", e.R.Host)
	} else {
		o.Req.Name = slurm.JobPrefix + "jupyter"
		id, err := e.Slurm.Submit(ctx, o.Req,
			jupyterScript(e.Cfg.Jupyter.Setup, o.Dir, token, tag),
			"--output=.cluster/jupyter-%j.log")
		if err != nil {
			return err
		}
		key = id
		sess.JobID = id
		sess.RemoteFiles = []string{".cluster/jupyter-" + id + ".log", ".cluster/jupyter-" + id + ".json"}
		e.printf("Submitted Jupyter job %s: %s\n", id, o.Req)
	}
	if err := e.Store.Add(sess); err != nil {
		e.printf("Warning: could not record session: %v\n", err)
	}
	if e.R.DryRun {
		return nil
	}

	info, err := e.waitJupyterInfo(ctx, sess, key)
	if err != nil {
		if ctx.Err() != nil {
			e.printf("\nInterrupted (%v); shutting down.\n", which())
		}
		_ = e.Release(sess)
		return err
	}
	sess.Node, sess.RemotePort = info.Host, info.Port
	if o.Head {
		sess.HeadPID = info.PID
	}
	_ = e.Store.Add(sess)
	return e.serveJupyter(ctx, which, sess, o.Browse)
}

// waitJupyterInfo waits for the server to publish its host and port,
// reporting queue state and surfacing the job log if it dies first.
func (e *Env) waitJupyterInfo(ctx context.Context, sess session.Session, key string) (jupyterInfo, error) {
	infoPath := ".cluster/jupyter-" + key + ".json"
	logPath := ".cluster/jupyter-" + key + ".log"
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
			return info, nil
		}

		running := true
		if sess.IsSlurm() {
			job, alive, err := e.Slurm.Alive(ctx, sess.JobID)
			if err == nil && !alive {
				tail, _ := e.R.Output(ctx, "tail -n 20 "+logPath+" 2>/dev/null; true", nil)
				return jupyterInfo{}, fmt.Errorf("Jupyter job %s ended before starting; last log lines:\n%s", sess.JobID, tail)
			}
			running = job.State == "RUNNING"
			if status := job.State + " " + job.NodeList; !running && status != last {
				e.printf("\r\033[KWaiting in queue: %s (Ctrl+C to cancel)", status)
				last = status
				deadline = time.Now().Add(2 * time.Minute)
			}
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

// serveJupyter runs the tunnel, restarting it after network loss, until
// ctx is cancelled (Ctrl+C / terminal closed) or the server goes away.
func (e *Env) serveJupyter(ctx context.Context, which func() os.Signal, sess session.Session, browse bool) error {
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

		tunnel := e.R.Tunnel(lp, sess.Node, sess.RemotePort)
		tunnel.Stdin, tunnel.Stdout, tunnel.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := tunnel.Start(); err != nil {
			return err
		}
		done := make(chan error, 1)
		go func() { done <- tunnel.Wait() }()

		sess.LocalPort, sess.TunnelPID = lp, tunnel.Process.Pid
		sess.URL = url + "/lab?token=" + sess.Token
		_ = e.Store.Add(sess)

		if err := waitHTTP(ctx, url, sess.Token); err != nil && ctx.Err() == nil {
			e.printf("Warning: %v\n", err)
		}
		if ctx.Err() == nil {
			if first {
				e.printf("\nJupyterLab: %s\n", sess.URL)
				e.printf("Ctrl+C or closing this terminal stops Jupyter and releases the node.\n")
				if browse {
					openBrowser(sess.URL)
				}
			} else {
				e.printf("Tunnel restored: %s\n", sess.URL)
			}
		}
		first = false

		alive := time.NewTicker(time.Minute)
	wait:
		for {
			select {
			case <-ctx.Done():
				alive.Stop()
				_ = tunnel.Process.Kill()
				<-done
				e.printf("\nStopping Jupyter (%v).\n", which())
				sess.TunnelPID = 0
				return e.Release(sess)
			case <-done:
				alive.Stop()
				break wait
			case <-alive.C:
				if !sess.IsSlurm() {
					continue
				}
				c, cancel := context.WithTimeout(ctx, 20*time.Second)
				_, ok, err := e.Slurm.Alive(c, sess.JobID)
				cancel()
				if err == nil && !ok {
					alive.Stop()
					_ = tunnel.Process.Kill()
					<-done
					e.printf("Jupyter job %s has ended (time limit?).\n", sess.JobID)
					sess.TunnelPID = 0
					return e.Release(sess)
				}
			}
		}

		// Tunnel exited on its own: network loss or the server went away.
		e.printf("\nTunnel lost; reconnecting (Jupyter keeps running on the cluster)...\n")
		if err := e.Reconnect(ctx); err != nil {
			e.printf("Stopping Jupyter (%v).\n", which())
			sess.TunnelPID = 0
			return e.Release(sess)
		}
		if sess.IsSlurm() {
			c, cancel := context.WithTimeout(ctx, 20*time.Second)
			_, ok, err := e.Slurm.Alive(c, sess.JobID)
			cancel()
			if err == nil && !ok {
				e.printf("Jupyter job %s ended while disconnected.\n", sess.JobID)
				sess.TunnelPID = 0
				return e.Release(sess)
			}
		}
	}
}
