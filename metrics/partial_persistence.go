package metrics

import "fmt"

// PartialPersistenceError reports that raw metrics committed successfully while
// optional process activity failed. Raw cadence and lifecycle acknowledgments
// must be retained; the activity failure remains observable and unhealthy.
type PartialPersistenceError struct {
	activityErr error
}

func (e *PartialPersistenceError) Error() string {
	return fmt.Sprintf("write metrics to database: %v", e.activityErr)
}

func (e *PartialPersistenceError) Unwrap() error { return e.activityErr }

// RawMetricsCommitted acknowledges only the successful raw SQL transaction.
func (e *PartialPersistenceError) RawMetricsCommitted() bool { return true }
