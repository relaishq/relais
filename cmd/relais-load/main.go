// relais-load runs harness callers against owned or external systems.
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
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/relais/internal/capacity"
	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/loadgen"
	"github.com/relais/internal/processrss"
	"github.com/relais/internal/processrun"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
)

type config struct {
	Soak                                          bool
	ChaosInterval                                 time.Duration
	Seed                                          int64
	WorkerFloor, RelayFloor                       int
	Callers, Processes, Workers, GOMAXPROCS       int
	Duration, SampleEvery                         time.Duration
	Bin, Output                                   string
	Video, Calibrate                              bool
	CalibrationStart, CalibrationMax              int
	Limits                                        loadgen.Limits
	Events                                        []loadgen.Event
	Capacity                                      bool
	Topology                                      string
	PlaceOnWorker                                 bool
	CapacityStart, CapacityMax, CallersPerProcess int
	Prices                                        string
	StoreOpsBudget, StoreBytesBudget              float64
}
type eventFlags []loadgen.Event

func (e *eventFlags) String() string { return fmt.Sprint([]loadgen.Event(*e)) }

// Repeatable -event=10s,move,3,1 means move three sampled calls to worker 1.
// -event=30s,kill,0,0 kills the entire owned worker 0 (count must be zero).
func (e *eventFlags) Set(value string) error {
	parts := strings.Split(value, ",")
	if len(parts) != 4 {
		return errors.New("event needs after,move|kill|drain,count,worker")
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
	if c.Topology != "" && c.Topology != "loopback" && c.Topology != "netns" {
		return errors.New("topology must be loopback or netns")
	}
	if c.Capacity && c.Limits != loadgen.DefaultLimits() {
		return errors.New("capacity requires the unchanged generator saturation limits")
	}
	if c.Capacity && (c.Calibrate || len(c.Events) > 0 || c.CapacityStart < 1 || c.CapacityMax < c.CapacityStart || c.CapacityMax > 1024 || c.CallersPerProcess < 1 || c.CallersPerProcess > 4 || c.Workers < 3 || c.Workers > 16 || c.Duration < 20*time.Second || c.Duration > 3*time.Minute || !(c.StoreOpsBudget > 0 && c.StoreOpsBudget <= 100) || !(c.StoreBytesBudget > 0 && c.StoreBytesBudget <= 128<<10)) {
		return errors.New("capacity needs 3..16 workers, 20s..3m duration, 1..4 callers/process, budgets in (0,100 commands] and (0,128 KiB], 1 <= start <= max <= 1024 and no custom events/calibration")
	}
	if c.Soak {
		if c.Capacity || c.Calibrate || c.Workers < 3 || c.Workers > 16 || c.Duration < 20*time.Second || c.Duration > 24*time.Hour || c.ChaosInterval < 20*time.Second || c.ChaosInterval+7*time.Second >= c.Duration || c.SampleEvery != time.Second || c.CallersPerProcess < 1 || c.CallersPerProcess > 4 || c.Limits != loadgen.DefaultLimits() || c.Callers > plannedLoad(c.Workers, c.WorkerFloor, c.RelayFloor) || c.Callers < 2 {
			return errors.New("soak needs 2..N+1 callers within explicit measured worker/relay floors, 3..16 workers, 20s..24h duration, >=20s interval and recovery tail, 1s samples, unchanged limits, 1..4 callers/process and no capacity/calibration")
		}
		for _, n := range counts(c.Callers, c.Processes) {
			if n > c.CallersPerProcess {
				return errors.New("soak processes exceed callers-per-process budget")
			}
		}
		return nil
	}
	var previous time.Duration
	killed := map[int]bool{}
	for _, e := range c.Events {
		if e.After < 3*time.Second || e.After <= previous || e.After+7*time.Second >= c.Duration || e.Worker < 0 || e.Worker >= c.Workers || e.Count < 0 || e.Count > c.Callers {
			return errors.New("events need ordered times, >=3s warmup, >7s recovery tail, valid worker/count")
		}
		if e.Kind != "move" && e.Kind != "kill" && e.Kind != "drain" {
			return errors.New("event kind must be move, drain or kill")
		}
		if killed[e.Worker] {
			return errors.New("cannot move to or kill a previously killed worker")
		}
		if e.Kind == "kill" || e.Kind == "drain" {
			if e.Count != 0 {
				return errors.New("kill/drain affects a whole worker; count must be zero")
			}
			if e.Kind == "kill" {
				killed[e.Worker] = true
			}
		}
		previous = e.After
	}
	if len(killed) >= c.Workers {
		return errors.New("must leave at least one live worker")
	}
	return nil
}

type responseError struct {
	code    int
	message string
}

func (e *responseError) Error() string { return e.message }

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
		statusError := &responseError{code: resp.StatusCode, message: fmt.Sprintf("%s: %s", url, resp.Status)}
		if resp.StatusCode == http.StatusMultiStatus && result != nil {
			return errors.Join(statusError, json.NewDecoder(resp.Body).Decode(result))
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		statusError.message += " " + strings.TrimSpace(string(data))
		return statusError
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
func runEvent(ctx context.Context, t *topology, generators []generator, e loadgen.Event) (result loadgen.EventResult) {
	defer func() {
		if result.FinishedAt.IsZero() {
			result.FinishedAt = time.Now()
		}
	}()
	result = loadgen.EventResult{Event: e, At: time.Now()}
	var status controlplane.Status
	t.statusRequests++
	err := request(ctx, http.MethodGet, strings.TrimSuffix(t.External.SignalingURL, "/calls")+"/status", nil, &status)
	if err == nil && e.Kind == "move" && e.Worker < 0 {
		counts := map[int]int{}
		for worker := range t.Workers {
			if !t.Killed[worker] {
				counts[worker] = 0
			}
		}
		for _, call := range status.Calls {
			worker, parseErr := strconv.Atoi(call.Owner)
			if parseErr == nil && !t.Killed[worker] {
				counts[worker]++
			}
		}
		best := -1
		for worker, n := range counts {
			if best < 0 || n < counts[best] || (n == counts[best] && worker < best) {
				best = worker
			}
		}
		e.Worker = best
		result.Event = e
	}
	if err == nil && e.Kind == "drain" {
		result.At = time.Now()
		for _, c := range status.Calls {
			if c.Owner == strconv.Itoa(e.Worker) {
				result.Sessions = append(result.Sessions, c.ID)
				result.Affected++
			}
		}
		if result.Affected == 0 {
			err = errors.New("drain selected worker with no calls")
		} else {
			var drained controlplane.DrainResult
			err = request(ctx, http.MethodPost, strings.TrimSuffix(t.External.SignalingURL, "/calls")+fmt.Sprintf("/workers/%d/drain", e.Worker), nil, &drained)
			result.FinishedAt = time.Now()
			var statusErr *responseError
			if errors.As(err, &statusErr) && statusErr.code == http.StatusMultiStatus {
				result.Failed = true
			}

			if drained.Error != "" {
				result.Failed = true
				err = errors.Join(err, errors.New(drained.Error))
			}
			if len(drained.Moves) != result.Affected {
				result.Failed = true
				err = errors.Join(err, errors.New("drain missing move results"))
			}
			workerURL := ""
			for _, target := range t.Targets {
				if target.Name == fmt.Sprintf("worker-%d", e.Worker) {
					workerURL = target.URL
				}
			}
			if workerURL == "" {
				err = errors.Join(err, errors.New("missing drained worker status endpoint"))
			} else {
				var worker mediaworker.WorkerStatus
				statusErr := request(ctx, http.MethodGet, workerURL+"/status", nil, &worker)
				err = errors.Join(err, statusErr)
				if statusErr == nil && worker.Sessions != 0 {
					result.Failed = true
					err = errors.Join(err, fmt.Errorf("drained worker still has %d sessions", worker.Sessions))
				}
			}
			for _, g := range generators {
				err = errors.Join(err, request(ctx, http.MethodPost, g.url+"/record-moves", drained.Moves, nil))
			}
		}
	} else if err == nil && e.Kind == "kill" {
		for _, c := range status.Calls {
			if c.Owner == strconv.Itoa(e.Worker) {
				result.Affected++
				result.Sessions = append(result.Sessions, c.ID)
			}
		}
		if result.Affected == 0 {
			err = errors.New("kill selected worker with no calls")
		} else {
			if t.Soak != nil {
				t.Soak.retire(fmt.Sprintf("worker-%d", e.Worker))
			}
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
			owner, ownerErr := strconv.Atoi(c.Owner)
			if ownerErr == nil && !t.Killed[owner] && c.Owner != strconv.Itoa(e.Worker) {
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
				result.Affected += len(ids)
				result.Sessions = append(result.Sessions, ids...)
				moveErr := request(ctx, http.MethodPost, g.url+"/move", loadgen.MoveRequest{Sessions: ids, To: e.Worker}, nil)
				err = errors.Join(err, moveErr)
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
	t, err := startTopology(ctx, m, c.Bin, dir, c.Workers, c.Topology)
	if err != nil {
		return report, err
	}
	if c.Soak {
		report.SentinelWorker = fmt.Sprintf("worker-%d", t.Sentinel)
		report.MemoryMetric = processrss.TrendMetric()
	}
	defer func() { report.WorkerRejoins = append([]loadgen.WorkerRejoin(nil), t.Rejoins...) }()
	defer func() { runErr = errors.Join(runErr, t.close()) }()
	defer func() {
		report.DriverStatusRequests = t.statusRequests
		if c.Duration > 0 {
			report.DriverStatusRequestsPerSecond = float64(t.statusRequests) / c.Duration.Seconds()
		}
	}()
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
				if c.Soak {
					p, err = loadgen.ReadSoakProcess(f, g.child.PID(), g.count, c.Limits)
				} else {
					p, err = loadgen.ReadProcess(f, g.child.PID(), g.count, c.Limits)
				}
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
		args := append([]string{executable}, callerNetworkArgs(t.Network)...)
		args = append(args, "-soak="+strconv.FormatBool(c.Soak), "-place-on-worker="+strconv.FormatBool(c.PlaceOnWorker), "-child", "-parent-pid", strconv.Itoa(os.Getpid()), "-callers", strconv.Itoa(n), "-duration", c.Duration.String(), "-sample-every", c.SampleEvery.String(), "-video="+strconv.FormatBool(c.Video), "-signaling", t.External.SignalingURL, "-relay", t.External.RelayAddr.String(), "-output", output)
		child, err := m.Start(dir, name, env, args...)
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
	if err := waitUntil(ctx, report.StartedAt); err != nil {
		return report, err
	}
	if c.Soak {
		t.Soak = startSoakExecution(ctx, t, c.SampleEvery)
		defer func() { report.Telemetry = t.Soak.finish(c.Callers, c.Duration) }()
	}
	var monitor *capacity.Monitor
	if c.Capacity {
		monitor = capacity.Start(ctx, t.Targets, c.SampleEvery)
		t.Monitor = monitor
		defer func() {
			if monitor != nil {
				report.Telemetry = monitor.Finish(c.Callers)
			}
		}()
	}
	for _, e := range c.Events {
		if err := waitUntil(ctx, report.StartedAt.Add(e.After)); err != nil {
			return report, err
		}
		if c.Soak {
			resolved, err := resolveSoakEvent(ctx, t, e, c.WorkerFloor)
			if err != nil {
				return report, err
			}
			result := runEvent(ctx, t, generators, resolved)
			report.Events = append(report.Events, result)
			if result.Error != "" {
				return report, errors.New(result.Error)
			}
			if resolved.Kind == "kill" || resolved.Kind == "drain" {
				if err := t.recycle(ctx, resolved.Worker); err != nil {
					return report, err
				}
			}
		} else {
			report.Events = append(report.Events, runEvent(ctx, t, generators, e))
		}
	}
	if c.Capacity {
		if err := waitUntil(ctx, report.StartedAt.Add(c.Duration)); err != nil {
			return report, err
		}
		report.Telemetry = monitor.Finish(c.Callers)
		monitor = nil
		report.WorkerPeaks = map[string]int{}
		for _, p := range report.Telemetry.Processes {
			if strings.HasPrefix(p.Name, "worker-") {
				report.WorkerPeaks[p.Name] = int(p.Peaks["relais_worker_active_sessions"])
			}
		}
	}
	for _, g := range generators {
		select {
		case <-g.child.Done():
		case <-ctx.Done():
			return report, ctx.Err()
		}
	}
	if c.Soak {
		report.Telemetry = t.Soak.finish(c.Callers, c.Duration)
		report.Leaks = t.checkSoakLeaks(generators)
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
	fmt.Println("PID CALLS CPU(core)% CPU/GOMAXPROCS% HEAP(MiB) RSS-peak(MiB) SEND-p50 SEND-p99 INTERVAL-p99-peak SEND-max SKIPPED-peak% STATUS/s SATURATED")
	for _, p := range r.Processes {
		fmt.Printf("%d %d %.2f %.2f %.1f %.1f %s %s %s %s %.2f %.2f %t\n", p.PID, len(p.Calls), p.PeakCPUPercent, p.PeakCPUPerGOMAXPROCS, float64(p.PeakHeapBytes)/(1<<20), float64(p.PeakRSSBytes)/(1<<20), p.Lateness.P50, p.Lateness.P99, p.PeakIntervalP99, p.Lateness.Max, p.PeakSkippedPercent, p.StatusRequestsPerSecond, p.Saturated)
	}
	for _, e := range r.Events {
		fmt.Printf("event=%s at=%s worker=%d affected=%d recorded=%d error=%q\n", e.Event.Kind, e.Event.After, e.Event.Worker, e.Affected, e.Recorded, e.Error)
	}
	for _, cause := range r.Causes {
		fmt.Println("cause:", cause)
	}
}
func run() error {
	return runArgs(os.Args[1:])
}

func runArgs(args []string) error {
	fs := flag.NewFlagSet("relais-load", flag.ContinueOnError)
	c := config{Limits: loadgen.DefaultLimits()}
	var events eventFlags
	child := fs.Bool("child", false, "internal caller-process mode")
	externalCaller := fs.Bool("external-caller", false, "run callers against an existing system; starts no services, waits for local /start")
	signaling := fs.String("signaling", "", "external caller HTTP(S) calls URL")
	relay := fs.String("relay", "", "external relay IPv4 media address:port")
	callerNamespace := fs.String("caller-namespace", "", "optional Linux caller network namespace (requires -caller-bind)")
	callerBind := fs.String("caller-bind", "", "literal caller IPv4 UDP address:port; port 0 allocates a port, no namespace required")
	signalingCert := fs.String("signaling-cert", "", "PEM file containing the pinned HTTPS server certificate (normal IP SAN verification)")
	distributed := fs.Bool("distributed", false, "opt-in identity feed and interval clock evidence; external caller only")
	runID := fs.String("run-id", "", "distributed run identity")
	controlBound := fs.Duration("frontend-control-bound", -1, "measured absolute control-minus-frontend clock error")
	workerBound := fs.Duration("frontend-worker-bound", -1, "measured absolute worker-minus-frontend clock error for all workers")
	driftPPM := fs.Float64("clock-drift-ppm", 100, "worst-case relative clock drift in parts per million")
	retention := fs.Duration("event-retention", 0, "caller event history; default 15s local, 180s distributed")
	bearerTokenFile := fs.String("bearer-token-file", "", "file containing the HTTPS signaling bearer token")
	fs.BoolVar(&c.PlaceOnWorker, "place-on-worker", false, "internal capacity placement on worker zero")
	parentPID := fs.Int("parent-pid", 0, "internal caller owner PID")
	fs.IntVar(&c.Callers, "callers", 10, "total simultaneous harness callers")
	fs.IntVar(&c.Processes, "processes", 2, "generator processes (balanced caller distribution)")
	fs.IntVar(&c.Workers, "workers", 3, "owned media workers")
	fs.IntVar(&c.GOMAXPROCS, "gomaxprocs", 1, "Go CPU budget for each caller process")
	fs.DurationVar(&c.Duration, "duration", 60*time.Second, "media duration after all callers connect")
	fs.DurationVar(&c.SampleEvery, "sample-every", time.Second, "child resource sample interval")
	fs.DurationVar(&c.Limits.P99, "lateness-limit", 20*time.Millisecond, "saturation limit for interval p99 packet lateness")
	fs.Float64Var(&c.Limits.SkippedPercent, "skipped-frame-limit", 1, "saturation percent of skipped frame opportunities")
	fs.Float64Var(&c.Limits.CPUPercent, "cpu-limit", 85, "saturation CPU percent per GOMAXPROCS")
	fs.StringVar(&c.Bin, "bin", "bin", "service binaries and run logs directory")
	fs.StringVar(&c.Output, "output", "", "report JSON path; defaults to run directory")
	fs.BoolVar(&c.Video, "video", true, "send harness audio plus video with bounded online decode sampling")
	fs.BoolVar(&c.Soak, "soak", false, "continuous calls with seeded moves, drains and restarted SIGKILL workers")
	fs.DurationVar(&c.ChaosInterval, "chaos-interval", 30*time.Second, "time between soak events")
	fs.Int64Var(&c.Seed, "seed", 41, "reproducible soak schedule seed")
	fs.IntVar(&c.WorkerFloor, "worker-floor", 8, "measured per-worker call floor; supply matching capacity evidence")
	fs.IntVar(&c.RelayFloor, "relay-floor", 8, "measured relay call floor; supply matching capacity evidence")
	fs.BoolVar(&c.Capacity, "capacity", false, "bounded service ramps followed by derived N+1 kill/move and drain")
	fs.StringVar(&c.Topology, "topology", "loopback", "loopback or Linux netns (unshaped LAN)")
	fs.IntVar(&c.CapacityStart, "capacity-start", 2, "first concurrent-call step")
	fs.IntVar(&c.CapacityMax, "capacity-max", 16, "bounded maximum concurrent calls per ramp")
	fs.IntVar(&c.CallersPerProcess, "callers-per-process", 2, "capacity generators: at most four callers per process, two by default")
	fs.StringVar(&c.Prices, "prices", "", "optional input machine/transfer price JSON")
	fs.Float64Var(&c.StoreOpsBudget, "store-ops-budget", 100, "maximum Redis client command attempts per call-second")
	fs.Float64Var(&c.StoreBytesBudget, "store-bytes-budget", 128<<10, "maximum Redis request+reply bytes per call-second")
	fs.BoolVar(&c.Calibrate, "calibrate", false, "double callers per process until saturation; requires -processes=1")
	fs.IntVar(&c.CalibrationStart, "calibration-start", 1, "first calibration caller count")
	fs.IntVar(&c.CalibrationMax, "calibration-max", 128, "maximum calibration count (a lower bound if unsaturated)")
	fs.Var(&events, "event", "repeat after,move|kill|drain,count,worker; move count=0 means all eligible calls")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *externalCaller && *child {
		return errors.New("external-caller and child are separate modes")
	}
	if *externalCaller {
		if err := validateExternalCaller(fs, c); err != nil {
			return err
		}
	}
	if *child || *externalCaller {
		addr, err := netip.ParseAddrPort(*relay)
		if err != nil {
			return err
		}
		if *externalCaller && (!addr.Addr().Is4() || addr.Addr().IsUnspecified() || addr.Port() == 0) {
			return errors.New("relay requires a specific IPv4 media address and nonzero port")
		}
		if *externalCaller {
			if !addr.Addr().IsLoopback() && *callerBind == "" {
				return errors.New("external caller with a non-loopback relay requires -caller-bind to select a local IPv4 media address")
			}
			if bind, err := netip.ParseAddrPort(*callerBind); err == nil && bind.Port() != 0 && c.Callers > 1 {
				return errors.New("multiple external callers require -caller-bind port 0 so each caller gets its own socket")
			}
		}
		ctx, stop := processrun.Context()
		defer stop()
		external, err := childTopology(*signaling, addr, *callerNamespace, *callerBind)
		if err != nil {
			return err
		}
		client, err := callerHTTPClient(*signaling, *signalingCert, *bearerTokenFile)
		if err != nil {
			return err
		}
		if client != nil {
			defer client.CloseIdleConnections()
		}
		external.HTTPClient = client
		if *externalCaller {
			runtime.GOMAXPROCS(c.GOMAXPROCS)
		}
		var distributedConfig *loadgen.DistributedConfig
		if *distributed {
			if !*externalCaller {
				return errors.New("distributed mode requires -external-caller")
			}
			distributedConfig = &loadgen.DistributedConfig{RunID: *runID, FrontendControlBound: *controlBound, FrontendWorkerBound: *workerBound, DriftPPM: *driftPPM}
		} else {
			for _, name := range []string{"run-id", "frontend-control-bound", "frontend-worker-bound", "clock-drift-ppm"} {
				if flagWasSet(fs, name) {
					return fmt.Errorf("-%s requires -distributed", name)
				}
			}
		}
		return loadgen.RunChild(ctx, loadgen.ChildOptions{EventRetention: *retention, Distributed: distributedConfig, PlaceOnWorker: c.PlaceOnWorker, Topology: external, Callers: c.Callers, Duration: c.Duration, SampleEvery: c.SampleEvery, Video: c.Video, Output: c.Output, ParentPID: *parentPID, Soak: c.Soak})
	}
	for _, name := range []string{"signaling", "relay", "caller-namespace", "caller-bind", "signaling-cert", "bearer-token-file", "parent-pid", "distributed", "run-id", "frontend-control-bound", "frontend-worker-bound", "clock-drift-ppm", "event-retention"} {
		if flagWasSet(fs, name) {
			return fmt.Errorf("-%s requires -external-caller or -child", name)
		}
	}
	c.Events = []loadgen.Event(events)
	if c.Soak {
		if len(c.Events) > 0 {
			return errors.New("soak generates its own seeded schedule; custom events are forbidden")
		}
		if !flagWasSet(fs, "processes") {
			c.Processes = (c.Callers + c.CallersPerProcess - 1) / c.CallersPerProcess
		}
		c.Events = soakSchedule(c.Duration, c.ChaosInterval, c.Seed, c.Workers)
	}
	if err := c.validate(); err != nil {
		return err
	}
	bin, err := filepath.Abs(c.Bin)
	if err != nil {
		return err
	}
	c.Bin = bin
	if c.Soak {
		if err := processrss.Check(); err != nil {
			return fmt.Errorf("soak memory sampling unavailable: %w", err)
		}
	}
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
	if c.Soak {
		return runSoak(ctx, m, c, root)
	}
	if c.Capacity {
		return runCapacity(ctx, m, c, root)
	}
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

func flagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
