package metrics

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/internal/processactivity"
)

func TestProcessActivityRestoreRecoveryPreservesDisjointCoverage(t *testing.T) {
	c := fixtureCollector(t, processFixture(t, 1))
	db, err := database.NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	writer := NewDBWriter(db, 30)
	c.SetDBWriter(writer)
	now := time.Now().UTC()
	id := processactivity.Identity{BootID: c.processBootID, PID: 10000, StartTicks: "10000"}
	old := processactivity.Entry{Identity: id, UID: 1000, Executable: "/bin/job", ExecutableAvailable: true, FirstObservedAt: now.Add(-time.Hour), ActivitySince: now.Add(-time.Hour), LastSeenAt: now.Add(-time.Minute), CoveredCPUSeconds: 2880, CPUCoveredWallSeconds: 3600, CPUAverageAvailable: true, AverageCPUPercent: 80, MemorySource: processactivity.MemoryPSS, MemoryByteSeconds: 360000, MemoryCoveredSeconds: 3600, MemoryAverageAvailable: true, Samples: 3}
	if err := db.UpsertProcessActivity(t.Context(), []processactivity.Entry{old}); err != nil {
		t.Fatal(err)
	}
	writer.SetEnabled(false)
	for i := 0; i < 2; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		c.publishProcessActivity(c.decisionState, []processactivity.Sample{{Identity: id, UID: 1000, Executable: "/bin/job", ExecutableAvailable: true, SampledAt: at, CPUAvailable: true, CPUSeconds: 100 + float64(i), MemorySource: processactivity.MemoryPSS, MemoryBytes: 100}}, at, true, 0)
	}
	writer.SetEnabled(true)
	if writer.Healthy() {
		t.Fatal("failed restore did not immediately mark activity unhealthy")
	}
	if err := writer.WriteMetricsBatch(processActivityRawBatch(now)); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteControlHistory(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if writer.Healthy() {
		t.Fatal("unrelated raw/history success cleared activity restore failure")
	}
	if err := c.persistProcessActivity(writer); err == nil {
		t.Fatal("write before restore could destroy older durable coverage")
	}
	writer.SetEnabled(true)
	if writer.Healthy() {
		t.Fatal("restore-incomplete persistence failure did not affect writer health")
	}
	at := now.Add(2 * time.Second)
	c.publishProcessActivity(c.decisionState, []processactivity.Sample{{Identity: id, UID: 1000, Executable: "/bin/job", ExecutableAvailable: true, SampledAt: at, CPUAvailable: true, CPUSeconds: 102, MemorySource: processactivity.MemoryPSS, MemoryBytes: 100}}, at, true, 0)
	if err := c.persistProcessActivity(writer); err != nil {
		t.Fatal(err)
	}
	if !writer.Healthy() {
		t.Fatal("successful restore and persist did not recover writer health")
	}
	entries, err := db.QueryProcessActivity(t.Context(), processactivity.Query{})
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
	e := entries[0]
	if !e.ActivitySince.Equal(old.ActivitySince) || e.CoveredCPUSeconds != 2882 || e.CPUCoveredWallSeconds != 3602 || e.MemoryCoveredSeconds != 3602 || e.Samples != 6 {
		t.Fatalf("recovery lost or bridged coverage: %+v", e)
	}
}

func TestProcessActivityCleanupDoesNotResurrectExpiredCache(t *testing.T) {
	c := fixtureCollector(t, processFixture(t, 1))
	db, err := database.NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	writer := NewDBWriter(db, 30)
	c.SetDBWriter(writer)
	now := time.Now().UTC()
	old := processactivity.Entry{Identity: processactivity.Identity{BootID: c.processBootID, PID: 42, StartTicks: "123"}, LastSeenAt: now.AddDate(0, 0, -31)}
	if err := db.UpsertProcessActivity(t.Context(), []processactivity.Entry{old}); err != nil {
		t.Fatal(err)
	}
	c.restoreProcessActivity()
	if _, err := db.CleanupOldData(30); err != nil {
		t.Fatal(err)
	}
	if err := c.persistProcessActivity(writer); err != nil {
		t.Fatal(err)
	}
	if got, err := db.QueryProcessActivity(t.Context(), processactivity.Query{}); err != nil || len(got) != 0 {
		t.Fatalf("expired cache resurrected: %+v %v", got, err)
	}
}

func TestProcessActivityMissingBootIdentityIsExplicit(t *testing.T) {
	c := fixtureCollector(t, processFixture(t, 1))
	c.processBootID = ""
	at := time.Now()
	c.publishProcessActivity(c.decisionState, []processactivity.Sample{{Identity: processactivity.Identity{PID: 42, StartTicks: "123"}, SampledAt: at}}, at, true, 0)
	got := c.activity.Snapshot()
	if got.ScanComplete || got.UnavailableIdentities != 1 || len(got.Entries) != 0 {
		t.Fatalf("unknown boot represented as measured absence: %+v", got)
	}
	if c.DecisionProcessScanError() != nil {
		t.Fatal("optional boot identity blocked valid authoritative CPU source")
	}
}

func TestProcessActivityUsesEffectiveCadenceAcrossReload(t *testing.T) {
	c := fixtureCollector(t, processFixture(t, 1))
	c.SetFallbackCPUSamplingInterval(300 * time.Second)
	now := time.Now()
	sample := processactivity.Sample{Identity: processactivity.Identity{BootID: c.processBootID, PID: 42, StartTicks: "123"}, UID: 1000, SampledAt: now, CPUAvailable: true, CPUSeconds: 100}
	c.publishProcessActivity(c.decisionState, []processactivity.Sample{sample}, now, true, 0)
	sample.SampledAt = now.Add(300 * time.Second)
	sample.CPUSeconds = 340
	c.publishProcessActivity(c.decisionState, []processactivity.Sample{sample}, sample.SampledAt, true, 0)
	if e := c.activity.Snapshot().Entries[0]; e.CPUCoveredWallSeconds != 300 {
		t.Fatalf("effective PSI fallback interval discarded: %+v", e)
	}
	cfg := *c.getConfig()
	cfg.PollingInterval = 1
	c.UpdateConfig(&cfg)
	c.SetFallbackCPUSamplingInterval(15 * time.Second)
	sample.SampledAt = sample.SampledAt.Add(30 * time.Second)
	sample.CPUSeconds = 355
	c.publishProcessActivity(c.decisionState, []processactivity.Sample{sample}, sample.SampledAt, true, 0)
	sample.SampledAt = sample.SampledAt.Add(31 * time.Second)
	sample.CPUSeconds = 370
	c.publishProcessActivity(c.decisionState, []processactivity.Sample{sample}, sample.SampledAt, true, 0)
	if e := c.activity.Snapshot().Entries[0]; e.CPUCoveredWallSeconds != 330 || e.CoveredCPUSeconds != 255 {
		t.Fatalf("reloaded effective cadence not respected: %+v", e)
	}
}

func TestProcessActivityUnknownRuntimeDoesNotInventLifetime(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		for _, body := range []string{"missing", "bad 0\n", "NaN 0\n", "+Inf 0\n", "-1 0\n", "50 0\n"} {
			t.Run(fmt.Sprintf("fallback=%v/%s", fallback, strings.TrimSpace(body)), func(t *testing.T) {
				root := processFixture(t, 1)
				t.Setenv("HOST_PROC", root)
				path := filepath.Join(root, "uptime")
				var err error
				if body == "missing" {
					err = os.Remove(path)
				} else {
					err = os.WriteFile(path, []byte(body), 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
				c := fixtureCollector(t, root)
				at := time.Now()
				c.now = func() time.Time { return at }
				collect := c.collectAllUserMetrics
				if fallback {
					collect = c.getAllUserMetricsFallback
				}
				collect(c.decisionState)
				path = filepath.Join(root, "10000", "stat")
				stat, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.Replace(string(stat), "50000", "50100", 1)), 0600); err != nil {
					t.Fatal(err)
				}
				at = at.Add(time.Second)
				got := collect(c.decisionState)
				if c.DecisionProcessScanError() != nil || got[1000] == nil || got[1000].CPUUsage <= 0 || got[1000].CPUUsageAverage != 0 {
					t.Fatalf("unknown optional lifetime damaged raw CPU: %+v %v", got, c.DecisionProcessScanError())
				}
				result, err := c.QueryProcessActivity(t.Context(), processactivity.Query{})
				if err != nil || len(result.Entries) != 1 {
					t.Fatalf("query=%+v err=%v", result, err)
				}
				e := result.Entries[0]
				if e.RuntimeAvailable || e.RuntimeSeconds != 0 || !e.CPUAverageAvailable || e.CoveredCPUSeconds <= 0 {
					t.Fatalf("unknown runtime/valid CPU conflated: %+v", e)
				}
				if _, err := json.Marshal(result); err != nil {
					t.Fatal(err)
				}
				filtered, err := c.QueryProcessActivity(t.Context(), processactivity.Query{MinRuntimeSeconds: 1})
				if err != nil || len(filtered.Entries) != 0 {
					t.Fatalf("unknown lifetime matched positive filter: %+v %v", filtered, err)
				}
			})
		}
	}
}

func TestProcessActivityBeforeFirstScanDoesNotBlockRawWriteOrClearFailure(t *testing.T) {
	c, err := NewCollector(config.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	db, err := database.NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	writer := NewDBWriter(db, 30)
	c.SetDBWriter(writer)
	at := time.Now().UTC()
	batch := processActivityRawBatch(at)
	if err := c.WriteMetricsToDatabase(batch); err != nil {
		t.Fatalf("raw write before process scan: %v", err)
	}
	if c.activity.HasObservations() || c.activityRestored {
		t.Fatal("raw persistence collected or restored process activity")
	}
	if writer.ShouldWrite() || !writer.Healthy() {
		t.Fatal("successful raw write did not preserve writer cadence/health")
	}
	writer.setProcessActivityWriteFailed(true)
	if err := c.persistProcessActivity(writer); err != nil {
		t.Fatalf("unused tracker persistence should be a no-op: %v", err)
	}
	if writer.Healthy() {
		t.Fatal("no-op cleared a prior activity persistence failure")
	}
	// An actually completed empty decision scan owes activity state too. It must
	// retain the restore guard even though no process entries were produced.
	c.activity.Update(nil, at, time.Minute, true, 0)
	if err := c.persistProcessActivity(writer); err == nil {
		t.Fatal("observed empty scan bypassed restore-before-write contract")
	}
	if writer.Healthy() {
		t.Fatal("incomplete restore reported healthy")
	}
}

func processActivityRawBatch(at time.Time) PersistenceBatch {
	return PersistenceBatch{System: SystemPersistenceMetrics{SampleEpochID: at.UnixNano(), IntervalEnd: at, IODeviceWeightState: "disabled", IODeviceWeightMechanism: "none", IODeviceWeightProgrammedState: "not_attempted", IODeviceWeightReadBackState: "not_attempted", IODeviceWeightAuthorityCoverage: "unavailable", IODeviceWeightEffectQualificationProvenance: "none", IODeviceWeightValuesJSON: "[]", IODeviceWeightObservedDelivery: "not_measured"}}
}

func TestProcessActivityRawCommitIndependentOfActivityFailure(t *testing.T) {
	c := fixtureCollector(t, processFixture(t, 1))
	db, err := database.NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	writer := NewDBWriter(db, 3600)
	c.SetDBWriter(writer)
	at := time.Now().UTC()
	c.activity.Update(nil, at, time.Minute, true, 0)
	err = c.WriteMetricsToDatabase(processActivityRawBatch(at))
	var partial *PartialPersistenceError
	if !errors.As(err, &partial) || !partial.RawMetricsCommitted() || partial.Unwrap() == nil {
		t.Fatalf("successful raw commit not identified: %v", err)
	}
	if writer.ShouldWrite() || writer.Healthy() {
		t.Fatal("activity failure changed raw cadence or disappeared from health")
	}
	rows, queryErr := db.GetSystemHistory(at.Add(-time.Minute), at.Add(time.Minute), 10)
	if queryErr != nil || len(rows) != 1 {
		t.Fatalf("raw transaction lost: rows=%d error=%v", len(rows), queryErr)
	}
	// A failed raw transaction must remain retryable and must not receive the
	// partial-success acknowledgment, even if activity fails independently too.
	writer = NewDBWriter(db, 3600)
	c.SetDBWriter(writer)
	invalid := processActivityRawBatch(at.Add(time.Second))
	invalid.Users = map[int]UserPersistenceMetrics{1000: {}}
	err = c.WriteMetricsToDatabase(invalid)
	partial = nil
	if err == nil || errors.As(err, &partial) || !writer.ShouldWrite() {
		t.Fatalf("raw failure was acknowledged: %v", err)
	}
	rows, queryErr = db.GetSystemHistory(at.Add(-time.Minute), at.Add(time.Minute), 10)
	if queryErr != nil || len(rows) != 1 {
		t.Fatalf("failed raw transaction appended rows: %d %v", len(rows), queryErr)
	}
}
