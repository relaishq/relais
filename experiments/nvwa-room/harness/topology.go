// Package harness owns every child process it starts. No discovery or global
// process killing is used. The room and Redis survive a worker SIGKILL.
package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Child struct {
	Name string `json:"name"`
	PID  int    `json:"pid"`
	cmd  *exec.Cmd
	done chan struct{}
	log  *os.File
}
type Config struct {
	RoomURL   string   `json:"room_url"`
	Audio     string   `json:"audio_bus"`
	SignalURL string   `json:"signal_url"`
	Cert      string   `json:"signal_cert"`
	Token     string   `json:"signal_token_file"`
	Control   string   `json:"control_url"`
	Redis     string   `json:"redis"`
	Children  []*Child `json:"children"`
}
type Topology struct {
	Config         Config
	Dir, Bin, Root string
	children       []*Child
	mu             sync.Mutex
	env            []string
	data           bool
}

func tcpPort() (int, error) {
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return 0, e
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
func udpPort() (int, error) {
	l, e := net.ListenPacket("udp", "127.0.0.1:0")
	if e != nil {
		return 0, e
	}
	defer l.Close()
	return l.LocalAddr().(*net.UDPAddr).Port, nil
}
func randomHex() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func (t *Topology) start(name, binary string, args []string, env []string) (*Child, error) {
	f, e := os.OpenFile(filepath.Join(t.Dir, name+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return nil, e
	}
	cmd := exec.Command(binary, args...)
	cmd.Env = env
	cmd.Dir = t.Root
	cmd.Stdout = f
	cmd.Stderr = f
	if e = cmd.Start(); e != nil {
		f.Close()
		return nil, e
	}
	c := &Child{Name: name, PID: cmd.Process.Pid, cmd: cmd, done: make(chan struct{}), log: f}
	t.children = append(t.children, c)
	t.Config.Children = t.children
	go func() { _ = cmd.Wait(); _ = f.Close(); close(c.done) }()
	return c, nil
}
func waitTCP(ctx context.Context, addr string) error {
	for {
		conn, e := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if e == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait %s: %w", addr, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
func Start(ctx context.Context, bin, root, dir string, data bool) (*Topology, error) {
	t := &Topology{Dir: dir, Bin: bin, Root: root, data: data}
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	fail := func(e error) (*Topology, error) { t.Close(); return nil, e }
	ports := make([]int, 7)
	usedTCP := map[int]bool{6379: true, 9101: true, 9201: true}
	for i := range ports {
		p, e := tcpPort()
		if e != nil {
			return fail(e)
		}
		for usedTCP[p] {
			p, e = tcpPort()
			if e != nil {
				return fail(e)
			}
		}
		usedTCP[p] = true
		ports[i] = p
	}
	redis, relayHTTP, control, signal, worker1, worker2, roomHTTP := ports[0], ports[1], ports[2], ports[3], ports[4], ports[5], ports[6]
	if redis == 6379 {
		return fail(fmt.Errorf("reserved Redis port"))
	}
	up := make([]int, 5)
	usedUDP := map[int]bool{6379: true, 9101: true, 9201: true}
	for i := range up {
		p, e := udpPort()
		if e != nil {
			return fail(e)
		}
		for usedUDP[p] {
			p, e = udpPort()
			if e != nil {
				return fail(e)
			}
		}
		usedUDP[p] = true
		up[i] = p
	}
	address := func(p int) string { return fmt.Sprintf("127.0.0.1:%d", p) }
	httpURL := func(p int) string { return "http://" + address(p) }
	t.Config = Config{RoomURL: httpURL(roomHTTP), Audio: "udp://" + address(up[4]), SignalURL: "https://" + address(signal), Cert: filepath.Join(dir, "signal.pem"), Token: filepath.Join(dir, "signal.token"), Control: httpURL(control), Redis: address(redis)}
	if e := os.WriteFile(t.Config.Token, []byte(randomHex()), 0600); e != nil {
		return fail(e)
	}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "RELAIS_") {
			t.env = append(t.env, entry)
		}
	}
	t.env = append(t.env, "RELAIS_REDIS_ADDR="+address(redis), "RELAIS_REDIS_PREFIX=nvwa:"+randomHex()+":", "RELAIS_SESSIONSTORE_KEY="+randomHex())
	_, e := t.start("redis", "redis-server", []string{"--bind", "127.0.0.1", "--port", fmt.Sprint(redis), "--save", "", "--appendonly", "no", "--dir", dir}, t.env)
	if e != nil {
		return fail(e)
	}
	if e = waitTCP(ctx, address(redis)); e != nil {
		return fail(e)
	}
	_, e = t.start("relay", filepath.Join(bin, "relais-relay"), []string{"-media", address(up[0]), "-leg", address(up[1]), "-http", address(relayHTTP)}, t.env)
	if e != nil {
		return fail(e)
	}
	if e = waitTCP(ctx, address(relayHTTP)); e != nil {
		return fail(e)
	}
	worker := func(n int, port, media int) error {
		args := []string{"-name", fmt.Sprintf("w%d", n), "-media", address(media), "-http", address(port), "-relay-leg", address(up[1]), "-relay-media", address(up[0]), "-control", httpURL(control), "-agent", "room-audio", "-room-audio-bus", t.Config.Audio, "-frame-cache-off"}
		if data {
			args = append(args, "-datachannel", "-datachannel-upstream", "ws://"+address(roomHTTP)+"/relais-upstream")
		}
		_, err := t.start(fmt.Sprintf("worker-%d", n), filepath.Join(bin, "relais-worker"), args, t.env)
		if err != nil {
			return err
		}
		return waitTCP(ctx, address(port))
	}
	if e = worker(1, worker1, up[2]); e != nil {
		return fail(e)
	}
	if e = worker(2, worker2, up[3]); e != nil {
		return fail(e)
	}
	_, e = t.start("control", filepath.Join(bin, "relais-control"), []string{"-http", address(control), "-relay", httpURL(relayHTTP), "-worker", "w1=" + httpURL(worker1), "-worker", "w2=" + httpURL(worker2)}, t.env)
	if e != nil {
		return fail(e)
	}
	if e = waitTCP(ctx, address(control)); e != nil {
		return fail(e)
	}
	_, e = t.start("signal", filepath.Join(bin, "relais-signal"), []string{"-listen", address(signal), "-control", httpURL(control), "-advertise-ips", "127.0.0.1", "-token-file", t.Config.Token, "-cert-out", t.Config.Cert}, t.env)
	if e != nil {
		return fail(e)
	}
	if e = waitTCP(ctx, address(signal)); e != nil {
		return fail(e)
	}
	_, e = t.start("room", filepath.Join(bin, "room"), []string{"-http", address(roomHTTP), "-audio", address(up[4]), "-test-faults", "-web", filepath.Join(root, "experiments/nvwa-room/web"), "-signal", t.Config.SignalURL, "-signal-cert", t.Config.Cert, "-signal-token", t.Config.Token}, t.env)
	if e != nil {
		return fail(e)
	}
	if e = waitTCP(ctx, address(roomHTTP)); e != nil {
		return fail(e)
	}
	b, _ := json.MarshalIndent(t.Config, "", "  ")
	if e = os.WriteFile(filepath.Join(dir, "topology.json"), b, 0600); e != nil {
		return fail(e)
	}
	return t, nil
}
func (t *Topology) KillWorker(name string) (int, time.Time, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range t.children {
		if c.Name == name {
			select {
			case <-c.done:
				return 0, time.Time{}, fmt.Errorf("owned worker already exited")
			default:
			}
			at := time.Now()
			return c.PID, at, c.cmd.Process.Signal(syscall.SIGKILL)
		}
	}
	return 0, time.Time{}, fmt.Errorf("worker not owned")
}
func (t *Topology) Close() {
	for i := len(t.children) - 1; i >= 0; i-- {
		c := t.children[i]
		select {
		case <-c.done:
			continue
		default:
		}
		_ = c.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-c.done:
		case <-time.After(700 * time.Millisecond):
			_ = c.cmd.Process.Kill()
			<-c.done
		}
	}
}
func (t *Topology) Status(ctx context.Context) (json.RawMessage, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", t.Config.Control+"/status", nil)
	res, e := http.DefaultClient.Do(req)
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	var value json.RawMessage
	e = json.NewDecoder(res.Body).Decode(&value)
	return value, e
}
