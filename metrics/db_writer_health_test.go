package metrics

import (
	"testing"
	"time"

	"github.com/fdefilippo/resman/database"
)

func TestDBWriterHealthTracksActualAttemptAndRecovery(t *testing.T) {
	db, err := database.NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	writer := NewDBWriter(db, 0)
	if !writer.Healthy() {
		t.Fatal("new writer unhealthy")
	}
	if err := writer.WriteMetricsBatch(PersistenceBatch{Users: map[int]UserPersistenceMetrics{1000: {}}}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	if writer.Healthy() {
		t.Fatal("failed write reported healthy")
	}
	now := time.Now().UTC()
	batch := PersistenceBatch{System: SystemPersistenceMetrics{SampleEpochID: now.UnixNano(), IntervalEnd: now, IODeviceWeightState: "disabled", IODeviceWeightMechanism: "none", IODeviceWeightProgrammedState: "not_attempted", IODeviceWeightReadBackState: "not_attempted", IODeviceWeightAuthorityCoverage: "unavailable", IODeviceWeightEffectQualificationProvenance: "none", IODeviceWeightValuesJSON: "[]", IODeviceWeightObservedDelivery: "not_measured"}}
	if err := writer.WriteMetricsBatch(batch); err != nil {
		t.Fatal(err)
	}
	if !writer.Healthy() {
		t.Fatal("successful retry did not clear failure")
	}
	writer.SetEnabled(false)
	if writer.Healthy() {
		t.Fatal("disabled writer reported healthy")
	}
}
