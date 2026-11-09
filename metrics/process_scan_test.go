package metrics

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fdefilippo/resman/internal/processactivity"
	"github.com/shirou/gopsutil/v3/cpu"
)

func TestProcessPoolBoundsAndStopJoinsWorkers(t *testing.T) {
	root := processFixture(t, 12)
	t.Setenv("HOST_PROC", root)
	c := fixtureCollector(t, root)
	c.processScanWorkers = 99 // even a private override cannot exceed the cap
	var running, peak atomic.Int32
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWorkers := func() { releaseOnce.Do(func() { close(release) }) }
	scanDone := make(chan struct{})
	stopDone := make(chan struct{})
	stopStarted := false
	// Registered after fixtureCollector's Stop cleanup: release and join first,
	// including any t.Fatal path before the normal release point.
	t.Cleanup(func() {
		releaseWorkers()
		<-scanDone
		if stopStarted {
			<-stopDone
		}
	})
	c.processStatReader = func(root string, pid int, ticks float64) (processactivity.ProcStat, error) {
		n := running.Add(1)
		defer running.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return processactivity.ReadStat(root, pid, ticks)
	}
	go func() { defer close(scanDone); c.GetAllUserMetricsForDecision() }()
	for range 4 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("four workers did not start")
		}
	}
	stopStarted = true
	go func() { c.Stop(); close(stopDone) }()
	select {
	case <-stopDone:
		t.Fatal("Stop returned with a process scan in flight")
	case <-time.After(20 * time.Millisecond):
	}
	releaseWorkers()
	select {
	case <-scanDone:
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not join")
	}
	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not join scan")
	}
	if peak.Load() != 4 || running.Load() != 0 {
		t.Fatalf("workers peak=%d remaining=%d", peak.Load(), running.Load())
	}
	if err := c.DecisionProcessScanError(); err != nil {
		t.Fatal(err)
	}
	if got := c.activity.Snapshot(); !got.ScanComplete || len(got.Entries) != 12 {
		t.Fatalf("published before completed bracket: %+v", got)
	}
}

func TestProcessPoolMatchesSerialWithIndependentObservationCadence(t *testing.T) {
	for _, observations := range []int{0, 1, 3} {
		t.Run(strconv.Itoa(observations), func(t *testing.T) {
			root := processFixture(t, 12)
			t.Setenv("HOST_PROC", root)
			serial := fixtureCollector(t, root)
			serial.processScanWorkers = 1
			parallel := fixtureCollector(t, root)
			parallel.processScanWorkers = 4
			at := time.Now()
			serial.now = func() time.Time { return at }
			parallel.now = func() time.Time { return at }
			for phase := 0; phase < 3; phase++ {
				at = at.Add(time.Second)
				for i := 0; i < 12; i++ {
					path := filepath.Join(root, strconv.Itoa(10000+i), "stat")
					body, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					text := string(body)
					// Move cumulative counters forward independently for each process.
					if phase > 0 {
						text = strings.Replace(text, strconv.Itoa(50000+(phase-1)*100), strconv.Itoa(50000+phase*100), 1)
					}
					if err := os.WriteFile(path, []byte(text), 0600); err != nil {
						t.Fatal(err)
					}
				}
				for range observations {
					parallel.collectAllUserMetrics(parallel.observationState)
				}
				a := serial.collectAllUserMetrics(serial.decisionState)
				b := parallel.collectAllUserMetrics(parallel.decisionState)
				if !reflect.DeepEqual(a, b) {
					t.Fatalf("phase %d: serial=%+v parallel=%+v", phase, a, b)
				}
				qa := processactivity.Merge(nil, serial.activity.Snapshot().Entries, processactivity.Query{})
				qb := processactivity.Merge(nil, parallel.activity.Snapshot().Entries, processactivity.Query{})
				if !reflect.DeepEqual(qa, qb) {
					t.Fatalf("phase %d: tracker differs", phase)
				}
			}
		})
	}
}

func TestProcessPoolMetadataSharesLifetimeBracket(t *testing.T) {
	for _, beforeFirst := range []bool{true, false} {
		t.Run(strconv.FormatBool(beforeFirst), func(t *testing.T) {
			root := processFixture(t, 1)
			t.Setenv("HOST_PROC", root)
			c := fixtureCollector(t, root)
			replace := func() {
				status := filepath.Join(root, "10000", "status")
				body, err := os.ReadFile(status)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(status, []byte(strings.ReplaceAll(string(body), "1000", "1001")), 0600); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, "10000", "exe")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/bin/reused", path); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(root, "10000", "stat")
				body, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.Replace(string(body), " 10000 4096000 ", " 10001 4096000 ", 1)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			reads := 0
			c.processStatReader = func(root string, pid int, ticks float64) (processactivity.ProcStat, error) {
				reads++
				if reads == 1 && beforeFirst {
					replace()
				}
				stat, err := processactivity.ReadStat(root, pid, ticks)
				if reads == 1 && !beforeFirst {
					replace()
				}
				return stat, err
			}
			got := c.collectAllUserMetrics(c.decisionState)
			if beforeFirst {
				if got[1000] != nil || got[1001] == nil {
					t.Fatalf("metadata from old lifetime: %+v", got)
				}
				entries := c.activity.Snapshot().Entries
				if len(entries) != 1 || entries[0].UID != 1001 || entries[0].Executable != "/bin/reused" || entries[0].StartTicks != "10001" {
					t.Fatalf("mixed process identity: %+v", entries)
				}
			} else if c.DecisionProcessScanError() == nil || len(got) != 0 || len(c.activity.Snapshot().Entries) != 0 {
				t.Fatal("PID replacement inside metadata bracket escaped guard")
			}
			if reads != 2 {
				t.Fatalf("stat reads=%d", reads)
			}
		})
	}
}

func TestProcessPoolIgnoresStatFailureForExcludedUIDRange(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(strconv.FormatBool(fallback), func(t *testing.T) {
			root := processFixture(t, 1)
			t.Setenv("HOST_PROC", root)
			path := filepath.Join(root, "10000", "status")
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(body), "1000", "0")), 0600); err != nil {
				t.Fatal(err)
			}
			c := fixtureCollector(t, root)
			reads := 0
			c.processStatReader = func(string, int, float64) (processactivity.ProcStat, error) {
				reads++
				return processactivity.ProcStat{}, os.ErrPermission
			}
			if fallback {
				c.getAllUserMetricsFallback(c.decisionState)
			} else {
				c.collectAllUserMetrics(c.decisionState)
			}
			if c.DecisionProcessScanError() != nil || reads != 1 {
				t.Fatalf("unrelated PID blocked decision: reads=%d error=%v", reads, c.DecisionProcessScanError())
			}
		})
	}
}

func TestProcessZeroStartTicksPreserveDeltasAndDetectReuse(t *testing.T) {
	state := newUserMetricsSamplingState()
	at := time.Now()
	first := cpu.TimesStat{User: 1}
	updateProcessCPUSample(state, 42, 0, first, at)
	updateProcessIOSample(state, 42, 0, processIOCounters{writeBytes: 100}, at)
	updateProcessCPUSample(state, 43, 7, first, at)
	if got := updateProcessCPUSample(state, 42, 0, cpu.TimesStat{User: 2}, at.Add(time.Second)); got != 100 {
		t.Fatalf("zero start CPU delta=%v", got)
	}
	if got := updateProcessIOSample(state, 42, 0, processIOCounters{writeBytes: 150}, at.Add(time.Second)); got.WriteBytes != 50 {
		t.Fatalf("zero start IO delta=%+v", got)
	}
	if got := updateProcessCPUSample(state, 42, 1, cpu.TimesStat{User: 20}, at.Add(2*time.Second)); got != 0 {
		t.Fatalf("reuse inherited zero-start CPU: %v", got)
	}
	if got := updateProcessIOSample(state, 42, 1, processIOCounters{writeBytes: 900}, at.Add(2*time.Second)); got.WriteBytes != 0 {
		t.Fatalf("reuse inherited zero-start IO: %+v", got)
	}
	if got := updateProcessCPUSample(state, 43, 7, cpu.TimesStat{User: 3}, at.Add(2*time.Second)); got != 100 {
		t.Fatalf("unrelated PID baseline changed: %v", got)
	}
}
