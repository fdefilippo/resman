// Package readiness defines the bounded, read-only daemon readiness contract.
package readiness

import "time"

// Status is a bounded lifecycle or subsystem state without internal error text.
type Status string

const (
	Ready       Status = "ready"
	Degraded    Status = "degraded"
	NotReady    Status = "not_ready"
	Disabled    Status = "disabled"
	Active      Status = "active"
	Unavailable Status = "unavailable"
)

// Snapshot contains only in-memory observations; it does not authorize mutations.
type Snapshot struct {
	Status             Status    `json:"status"`
	ConfigWatcher      Status    `json:"config_watcher"`
	Database           Status    `json:"database"`
	Systemd            Status    `json:"systemd"`
	PSI                Status    `json:"psi"`
	DecisionAt         time.Time `json:"decision_at"`
	DecisionsSuspended bool      `json:"decisions_suspended"`
}

// IsReady includes supported degraded operation, such as polling without PSI.
func (s Snapshot) IsReady() bool { return s.Status == Ready || s.Status == Degraded }
