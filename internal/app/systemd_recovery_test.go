package app

import (
	"reflect"
	"testing"

	"github.com/fdefilippo/resman/internal/systemdunit"
)

type recoveryCaptureLogger struct {
	messages []string
	fields   []interface{}
}

func (*recoveryCaptureLogger) Debug(string, ...interface{}) {}
func (l *recoveryCaptureLogger) Info(message string, fields ...interface{}) {
	l.messages = append(l.messages, message)
	l.fields = append([]interface{}(nil), fields...)
}
func (*recoveryCaptureLogger) Warn(string, ...interface{})              {}
func (*recoveryCaptureLogger) Error(string, ...interface{})             {}
func (*recoveryCaptureLogger) InfoChecked(string, ...interface{}) error { return nil }

func TestSystemdLeaseRecoveryIsLoggedOnceAsBoundedCounts(t *testing.T) {
	logger := &recoveryCaptureLogger{}
	report := []systemdunit.LeaseRecoveryOutcome{
		{Unit: "user.slice", State: systemdunit.LeaseRecoveryReclaimed},
		{Unit: "user-1000.slice", State: systemdunit.LeaseRecoveryReclaimed},
		{Unit: "user-1001.slice", State: systemdunit.LeaseRecoveryConflict},
	}
	logSystemdLeaseRecovery(logger, report)
	if !reflect.DeepEqual(logger.messages, []string{"Systemd property lease recovery completed"}) {
		t.Fatalf("recovery messages = %v", logger.messages)
	}
	want := []interface{}{
		"total_units", 3,
		"reclaimed_units", 2,
		"orphaned_units", 0,
		"inactive_cleaned_units", 0,
		"pending_units", 0,
		"conflicted_units", 1,
	}
	if !reflect.DeepEqual(logger.fields, want) {
		t.Fatalf("recovery fields = %#v, want %#v", logger.fields, want)
	}
	for _, field := range logger.fields {
		if field == "user.slice" || field == "user-1000.slice" || field == "user-1001.slice" {
			t.Fatalf("recovery log leaked an unbounded unit identity: %#v", logger.fields)
		}
	}
}

func TestEmptySystemdLeaseRecoveryDoesNotEmitNoise(t *testing.T) {
	logger := &recoveryCaptureLogger{}
	logSystemdLeaseRecovery(logger, nil)
	if len(logger.messages) != 0 {
		t.Fatalf("empty recovery emitted messages: %v", logger.messages)
	}
}
