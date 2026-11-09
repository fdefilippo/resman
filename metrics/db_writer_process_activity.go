package metrics

import (
	"context"
	"fmt"

	"github.com/fdefilippo/resman/internal/processactivity"
)

// UpsertProcessActivity writes a bounded summary snapshot after releasing writer state.
func (w *DBWriter) UpsertProcessActivity(ctx context.Context, entries []processactivity.Entry) (writeErr error) {
	w.mu.RLock()
	enabled, db := w.enabled, w.dbManager
	w.mu.RUnlock()
	if !enabled || db == nil {
		return fmt.Errorf("process activity database unavailable")
	}
	defer func() { w.setProcessActivityWriteFailed(writeErr != nil) }()
	return db.UpsertProcessActivity(ctx, entries)
}

// QueryProcessActivity reads durable summaries without changing periodic sampling state.
func (w *DBWriter) QueryProcessActivity(ctx context.Context, q processactivity.Query) ([]processactivity.Entry, error) {
	w.mu.RLock()
	enabled, db := w.enabled, w.dbManager
	w.mu.RUnlock()
	if !enabled || db == nil {
		return nil, fmt.Errorf("process activity database unavailable")
	}
	return db.QueryProcessActivity(ctx, q)
}

// RestoreProcessActivity restores totals without restoring a CPU interval baseline.
func (w *DBWriter) RestoreProcessActivity(ctx context.Context, boot string) ([]processactivity.Entry, error) {
	w.mu.RLock()
	enabled, db := w.enabled, w.dbManager
	w.mu.RUnlock()
	if !enabled || db == nil {
		return nil, fmt.Errorf("process activity database unavailable")
	}
	return db.RestoreProcessActivity(ctx, boot)
}

func (w *DBWriter) setProcessActivityWriteFailed(failed bool) {
	w.mu.Lock()
	w.processActivityWriteFailed = failed
	w.mu.Unlock()
}
