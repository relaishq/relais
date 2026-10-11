// relais-load runs harness callers across owned generator processes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/loadgen"
	"github.com/relais/internal/processrun"
	"github.com/relais/pkg/controlplane"
)

type config struct {
	Callers, Processes, Workers, GOMAXPROCS int
	Duration, SampleEvery                   time.Duration
	Bin, Output                             string
	Video, Calibrate                        bool
	CalibrationStart, CalibrationMax        int
	Limits                                  loadgen.Limits
	Events                                  []loadgen.Event
}
type eventFlags []loadgen.Event

func (e *eventFlags) String() string { return fmt.Sprint([]loadgen.Event(*e)) }

// Repeatable -event=10s,move,3,1 means move three sampled calls to worker 1.
// -event=30s,kill,0,0 kills the entire owned worker 0 (count must be zero).
func (e *eventFlags) Set(value string) error {
	parts := strings.Split(value, ",")
	if len(parts) != 4 {
		return errors.New("event needs after,move|kill,count,worker")
	}
	after, err := time.ParseDuration(parts[0])
	if err != nil {
		return err
	}
	count, err := strconv.Atoi(parts[2])
	if err != nil {
		return err
	}
	worker, err := strconv.Atoi(parts[3])
	if err != nil {
		return err
	}
	*e = append(*e, loadgen.Event{After: after, Kind: parts[1], Count: count, Worker: worker})
	return nil
}
func (c config) validate() error {
	if c.Callers < 1 || c.Processes < 1 || c.Processes > c.Callers || c.Workers < 2 || c.GOMAXPROCS < 1 {
		return errors.New("need callers >= processes >= 1, workers >= 2 and gomaxprocs >= 1")
	}
	if c.Duration < time.Second || c.SampleEvery < 100*time.Millisecond || c.SampleEvery > c.Duration {
		return errors.New("duration >= 1s and sample interval in [100ms,duration] required")
	}
	if err := c.Limits.Validate(); err != nil {
		return err
	}
	if c.Calibrate && (c.Processes != 1 || len(c.Events) > 0 || c.CalibrationStart < 1 || c.CalibrationMax < c.CalibrationStart || c.CalibrationMax > 4096) {
		return errors.New("calibration needs one process, no events and 1 <= start <= max <= 4096")
	}
	var previous time.Duration
	killed := map[int]bool{}
	for _, e := range c.Events {
		if e.After < 3*time.Second || e.After <= previous || e.After+7*time.Second >= c.Duration || e.Worker < 0 || e.Worker >= c.Workers || e.Count < 0 || e.Count > c.Callers {
			return errors.New("events need ordered times, >=3s warmup, >7s recovery tail, valid worker/count")
		}
		if e.Kind != "move" && e.Kind != "kill" {
			return errors.New("event kind must be move or kill")
		}
		if killed[e.Worker] {
			return errors.New("cannot move to or kill a previously killed worker")
		}
		if e.Kind == "kill" {
			if e.Count != 0 {
				return errors.New("kill affects a whole worker; count must be zero")
			}
			killed[e.Worker] = true
		}
		previous = e.After
	}
	if len(killed) >= c.Workers {
		return errors.New("must leave at least one live worker")
	}
	return nil
}
func request(ctx context.Context, method, url string, body, result any) error {
	var b bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&b).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, url, &b)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s %s", url, resp.Status, strings.TrimSpace(string(data)))
	}
	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

type generator struct {
	child       *clusterprocess.Child
	url, output string
	count       int
	sessions    []string
}

func counts(total, processes int) []int {
	values := make([]int, processes)
	for i := range values {
		values[i] = total / processes
		if i < total%processes {
			values[i]++
		}
	}
	return values
}
func waitUntil(ctx context.Context, at time.Time) error {
	timer := time.NewTimer(max(time.Until(at), 0))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func runEvent(ctx context.Context, t *topology, generators []generator, e loadgen.Event) loadgen.EventResult {
	result := loadgen.EventResult{Event: e, At: time.Now()}
	var status controlplane.Status
	err := request(ctx, http.MethodGet, strings.TrimSuffix(t.External.SignalingURL, "/calls")+"/status", nil, &status)
	if err == nil && e.Kind == "kill" {
		for _, c := range status.Calls {
			if c.Owner == strconv.Itoa(e.Worker) {
				result.Affected++
			}
		}
		if result.Affected == 0 {
			err = errors.New("kill selected worker with no calls")
		} else {
			result.At = time.Now()
			err = t.kill(e.Worker)
			if err == nil {
				for _, g := range generators {
					err = errors.Join(err, request(ctx, http.MethodPost, g.url+"/kill", loadgen.KillRequest{Worker: strconv.Itoa(e.Worker), At: result.At}, nil))
				}
			}
		}
	} else if err == nil {
		eligible := map[string]bool{}
		for _, c := range status.Calls {
			if c.Owner != strconv.Itoa(e.Worker) {
				eligible[c.ID] = true
			}
		}
		remaining := e.Count
		if remaining == 0 {
			remaining = len(eligible)
		}
		for _, g := range generators {
			ids := []string{}
			for _, id := range g.sessions {
				if eligible[id] && remaining > 0 {
					ids = append(ids, id)
					remaining--
				}
			}
			if len(ids) > 0 {
				moveErr := request(ctx, http.MethodPost, g.url+"/move", loadgen.MoveRequest{Sessions: ids, To: e.Worker}, nil)
				err = errors.Join(err, moveErr)
				if moveErr == nil {
					result.Affected += len(ids)
				}
			}
		}
		if result.Affected == 0 || remaining > 0 {
			err = errors.Join(err, fmt.Errorf("move affected %d calls; requested %d", result.Affected, e.Count))
		}
	}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}
func execute(ctx context.Context, m *clusterprocess.Manager, c config, dir string) (report loadgen.RunReport, runErr error) {
	report = loadgen.RunReport{Duration: c.Duration, Callers: c.Callers, Limits: c.Limits}
	defer func() {
		report.Judge()
		if runErr != nil {
			report.Outcome = "inconclusive"
			report.Causes = append(report.Causes, runErr.Error())
		}
	}()
	t, err := startTopology(ctx, m, c.Bin, dir, c.Workers)
	if err != nil {
		return report, err
	}
	defer t.close()
	executable, err := os.Executable()
	if err != nil {
		return report, err
	}
	var generators []generator
	defer func() {
		for _, g := range generators {
			g.child.Stop()
		}
		// Read only after children have been reaped. Preserve partial evidence
		// on setup failure, timeout or cancellation as well as normal exit.
		for _, g := range generators {
			f, err := os.Open(g.output)
			p := loadgen.ProcessReport{PID: g.child.PID(), ExpectedCalls: g.count}
			if err == nil {
				p, err = loadgen.ReadProcess(f, g.child.PID(), g.count, c.Limits)
				_ = f.Close()
			}
			if err = errors.Join(err, g.child.Err()); err != nil {
				p.Complete = false
				p.Causes = append(p.Causes, err.Error())
			}
			report.Processes = append(report.Processes, p)
		}
	}()
	for i, n := range counts(c.Callers, c.Processes) {
		output := filepath.Join(dir, fmt.Sprintf("caller-%d.jsonl", i))
		name := fmt.Sprintf("caller-%d", i)
		env := serviceEnv("", "", "")
		env = append(withoutEnv(env, "GOMAXPROCS"), "GOMAXPROCS="+strconv.Itoa(c.GOMAXPROCS))
		child, err := m.Start(dir, name, env, executable, "-child", "-callers", strconv.Itoa(n), "-duration", c.Duration.String(), "-sample-every", c.SampleEvery.String(), "-video="+strconv.FormatBool(c.Video), "-signaling", t.External.SignalingURL, "-relay", t.External.RelayAddr.String(), "-output", output)
		if err != nil {
			return report, err
		}
		generators = append(generators, generator{child: child, output: output, count: n})
	}
	for i := range generators {
		startup, stop := context.WithTimeout(ctx, 3*time.Minute)
		ready, err := generators[i].child.Ready(startup)
		stop()
		if err != nil {
			return report, err
		}
		generators[i].url = ready.HTTP
		if err := request(ctx, http.MethodGet, ready.HTTP+"/sessions", nil, &generators[i].sessions); err != nil {
			return report, err
		}
	}
	report.StartedAt = time.Now().Add(time.Second)
	for _, g := range generators {
		if err := request(ctx, http.MethodPost, g.url+"/start", loadgen.StartRequest{At: report.StartedAt}, nil); err != nil {
			return report, err
		}
	}
	for _, e := range c.Events {
		if err := waitUntil(ctx, report.StartedAt.Add(e.After)); err != nil {
			return report, err
		}
		report.Events = append(report.Events, runEvent(ctx, t, generators, e))
	}
	for _, g := range generators {
		select {
		case <-g.child.Done():
		case <-ctx.Done():
			return report, ctx.Err()
		}
	}
	return report, nil
}

type calibration struct {
	Runner         string              `json:"runner"`
	GOMAXPROCS     int                 `json:"gomaxprocs"`
	Steps          []loadgen.RunReport `json:"steps"`
	LargestValid   int                 `json:"largest_valid_callers_per_process"`
	FirstSaturated int                 `json:"first_saturated_callers_per_process"`
	LimitFound     bool                `json:"limit_found"`
	Note           string              `json:"note"`
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}
func printReport(r loadgen.RunReport) {
	fmt.Printf("callers=%d duration=%s outcome=%s\n", r.Callers, r.Duration, r.Outcome)
	fmt.Println("PID CALLS CPU(core)% CPU/GOMAXPROCS% HEAP(MiB) RSS-peak(MiB) SEND-p50 SEND-p99 INTERVAL-p99-peak SEND-max SATURATED")
	for _, p := range r.Processes {
		fmt.Printf("%d %d %.2f %.2f %.1f %.1f %s %s %s %s %t\n", p.PID, len(p.Calls), p.PeakCPUPercent, p.PeakCPUPerGOMAXPROCS, float64(p.PeakHeapBytes)/(1<<20), float64(p.PeakRSSBytes)/(1<<20), p.Lateness.P50, p.Lateness.P99, p.PeakIntervalP99, p.Lateness.Max, p.Saturated)
	}
	for _, e := range r.Events {
		fmt.Printf("event=%s at=%s worker=%d affected=%d error=%q\n", e.Event.Kind, e.Event.After, e.Event.Worker, e.Affected, e.Error)
	}
	for _, cause := range r.Causes {
		fmt.Println("cause:", cause)
	}
}
func run() error {
	fs := flag.NewFlagSet("relais-load", flag.ContinueOnError)
	c := config{Limits: loadgen.DefaultLimits()}
	var events eventFlags
	child := fs.Bool("child", false, "internal caller-process mode")
	signaling := fs.String("signaling", "", "child control-plane calls URL")
	relay := fs.String("relay", "", "child relay media address")
	fs.IntVar(&c.Callers, "callers", 10, "total simultaneous harness callers")
	fs.IntVar(&c.Processes, "processes", 2, "generator processes (balanced caller distribution)")
	fs.IntVar(&c.Workers, "workers", 3, "owned media workers")
	fs.IntVar(&c.GOMAXPROCS, "gomaxprocs", 1, "Go CPU budget for each caller process")
	fs.DurationVar(&c.Duration, "duration", 60*time.Second, "media duration after all callers connect")
	fs.DurationVar(&c.SampleEvery, "sample-every", time.Second, "child resource sample interval")
	fs.DurationVar(&c.Limits.P99, "lateness-limit", 20*time.Millisecond, "saturation limit for interval p99 packet lateness")
	fs.Float64Var(&c.Limits.CPUPercent, "cpu-limit", 85, "saturation CPU percent per GOMAXPROCS")
	fs.StringVar(&c.Bin, "bin", "bin", "service binaries and run logs directory")
	fs.StringVar(&c.Output, "output", "", "report JSON path; defaults to run directory")
	fs.BoolVar(&c.Video, "video", true, "send harness audio plus video with bounded online decode sampling")
	fs.BoolVar(&c.Calibrate, "calibrate", false, "double callers per process until saturation; requires -processes=1")
	fs.IntVar(&c.CalibrationStart, "calibration-start", 1, "first calibration caller count")
	fs.IntVar(&c.CalibrationMax, "calibration-max", 128, "maximum calibration count (a lower bound if unsaturated)")
	fs.Var(&events, "event", "repeat after,move|kill,count,worker; move count=0 means all eligible calls")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *child {
		addr, err := netip.ParseAddrPort(*relay)
		if err != nil {
			return err
		}
		ctx, stop := processrun.Context()
		defer stop()
		return loadgen.RunChild(ctx, loadgen.ChildOptions{Topology: externalTopology(*signaling, addr), Callers: c.Callers, Duration: c.Duration, SampleEvery: c.SampleEvery, Video: c.Video, Output: c.Output})
	}
	c.Events = []loadgen.Event(events)
	if err := c.validate(); err != nil {
		return err
	}
	bin, err := filepath.Abs(c.Bin)
	if err != nil {
		return err
	}
	c.Bin = bin
	for _, name := range []string{"relais-relay", "relais-worker", "relais-control"} {
		s, err := os.Stat(filepath.Join(bin, name))
		if err != nil || s.IsDir() || s.Mode()&0111 == 0 {
			return fmt.Errorf("make build first: missing executable %s", name)
		}
	}
	if c.Video {
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			return errors.New("ffmpeg is required for online video decode sampling")
		}
	}
	root, err := os.MkdirTemp(bin, "load-run-")
	if err != nil {
		return err
	}
	fmt.Println("process logs:", root)
	if c.Output == "" {
		c.Output = filepath.Join(root, "report.json")
	}
	m := &clusterprocess.Manager{}
	ctx, stop := m.Context()
	defer stop()
	if !c.Calibrate {
		runCtx, cancel := context.WithTimeout(ctx, 4*time.Minute+c.Duration)
		defer cancel()
		report, runErr := execute(runCtx, m, c, root)
		printReport(report)
		err = writeJSON(c.Output, report)
		fmt.Println("report:", c.Output)
		if report.Outcome == "inconclusive" {
			runErr = errors.Join(runErr, errors.New("load run inconclusive; see report causes"))
		}
		return errors.Join(runErr, err)
	}
	host, _ := os.Hostname()
	cal := calibration{Runner: host, GOMAXPROCS: c.GOMAXPROCS}
	for n := c.CalibrationStart; n <= c.CalibrationMax; {
		stepDir := filepath.Join(root, fmt.Sprintf("step-%d", n))
		if err := os.Mkdir(stepDir, 0700); err != nil {
			return err
		}
		c.Callers = n
		// One owner handles signals for every child; each step closes its topology.
		stepCtx, cancel := context.WithTimeout(ctx, 4*time.Minute+c.Duration)
		report, stepErr := execute(stepCtx, m, c, stepDir)
		cancel()
		cal.Steps = append(cal.Steps, report)
		printReport(report)
		_ = writeJSON(filepath.Join(stepDir, "report.json"), report)
		saturated := false
		for _, p := range report.Processes {
			if p.Saturated {
				saturated = true
			}
		}
		if saturated {
			cal.FirstSaturated = n
			cal.LimitFound = true
			cal.Note = "Observed saturation bracket; sampled steps are not an exact capacity threshold."
			break
		}
		if stepErr != nil || report.Outcome != "valid" {
			cal.Note = "Calibration stopped on an incomplete or invalid measurement; capacity limit unknown."
			break
		}
		cal.LargestValid = n
		if n == c.CalibrationMax {
			break
		}
		n = min(n*2, c.CalibrationMax)
	}
	if cal.Note == "" {
		cal.Note = "No saturation observed within the configured cap; largest valid count is a lower bound, not the generator limit."
	}
	fmt.Printf("calibration: largest-valid=%d first-saturated=%d limit-found=%t; %s\n", cal.LargestValid, cal.FirstSaturated, cal.LimitFound, cal.Note)
	err = writeJSON(c.Output, cal)
	fmt.Println("report:", c.Output)
	if !cal.LimitFound && (cal.LargestValid == 0 || cal.Steps[len(cal.Steps)-1].Outcome != "valid") {
		return errors.Join(err, errors.New("calibration stopped without a trustworthy saturation bracket"))
	}
	return err
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func withoutEnv(env []string, name string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != name {
			out = append(out, entry)
		}
	}
	return out
}
