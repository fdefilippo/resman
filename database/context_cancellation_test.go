package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fdefilippo/resman/internal/metricrollup"
	"github.com/fdefilippo/resman/internal/processactivity"
)

func TestObservationDatabaseDeadlineIncludesGateWait(t *testing.T) {
	db, err := NewDatabaseManager(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	now := time.Now().UTC()
	for _, operation := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"history write", func(ctx context.Context) error { return db.WriteControlHistory(ctx, nil) }},
		{"history read", func(ctx context.Context) error {
			_, err := db.ReadControlHistory(ctx, 1)
			return err
		}},
		{"activity write", func(ctx context.Context) error { return db.UpsertProcessActivity(ctx, nil) }},
		{"activity query", func(ctx context.Context) error {
			_, err := db.QueryProcessActivity(ctx, processactivity.Query{})
			return err
		}},
		{"activity restore", func(ctx context.Context) error {
			_, err := db.RestoreProcessActivity(ctx, "boot")
			return err
		}},
		{"rollup query", func(ctx context.Context) error {
			_, err := db.GetMetricRollups(ctx, metricrollup.Query{Scope: metricrollup.User, Start: now.Add(-time.Hour), End: now})
			return err
		}},
	} {
		t.Run(operation.name, func(t *testing.T) {
			leaveOwner := db.dbGate.Enter()
			defer leaveOwner()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- operation.run(ctx) }()
			select {
			case err := <-result:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("operation error=%v, want deadline exceeded", err)
				}
			case <-time.After(time.Second):
				t.Fatal("operation deadline did not cancel the gate wait")
			}
		})
		// A failed waiter must leave the gate usable by the next operation.
		if err := operation.run(t.Context()); err != nil {
			t.Fatalf("%s after cancellation: %v", operation.name, err)
		}
	}
}
