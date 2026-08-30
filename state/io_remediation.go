/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program. If not, see <https://www.gnu.org/licenses/>.
 */
// state/io_remediation.go
package state

import (
	"fmt"
	"sync"
	"time"

	"github.com/fdefilippo/resman/cgroup"
	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/operationgate"
	"github.com/fdefilippo/resman/logging"
)

// IOBoostState tracks temporary I/O boost state for one user.
type IOBoostState struct {
	IsActive             bool
	StartTime            time.Time
	OriginalReadBPS      string
	OriginalWriteBPS     string
	OriginalReadIOPS     int
	OriginalWriteIOPS    int
	OriginalDeviceFilter string
	BoostCount           int
	LastBoostTime        time.Time
	StarvationStart      time.Time
	LastSeen             time.Time
}

// IORemediation detects and remediates I/O starvation.
type IORemediation struct {
	mu          sync.RWMutex
	opGate      operationgate.Gate
	logger      *logging.Logger
	boostStates map[int]*IOBoostState // UID -> boost state
	revision    uint64
	lastCheck   time.Time
}

// NewIORemediation creates an I/O remediation coordinator.
func NewIORemediation(logger *logging.Logger) *IORemediation {
	return &IORemediation{
		logger:      logger,
		boostStates: make(map[int]*IOBoostState),
	}
}

// IORemediationDeps contains the external remediation operations.
type IORemediationDeps interface {
	GetPSIStats(uid int) (cgroup.PSIStats, error)
	ApplyTemporaryIOLimit(uid int, readBPS, writeBPS string, readIOPS, writeIOPS int, deviceFilter string, multiplier float64) error
}

const (
	ioRemediationPSIReadFailure      = "psi_read_failure"
	ioRemediationBoostApplyFailure   = "boost_apply_failure"
	ioRemediationBoostRevertFailure  = "boost_revert_failure"
	ioRemediationCompensationFailure = "compensation_failure"
)

type ioRemediationError struct {
	uid       int
	operation string
	err       error
}

func (e *ioRemediationError) Error() string {
	return fmt.Sprintf("IO remediation %s for UID %d: %v", e.operation, e.uid, e.err)
}

func (e *ioRemediationError) Unwrap() error {
	return e.err
}

// CheckAndRemediate checks limited users and applies temporary I/O boosts.
func (r *IORemediation) CheckAndRemediate(deps IORemediationDeps, cfg *config.Config, limitedUsers []int) []error {
	if !cfg.GetIORemediationEnabled() {
		return nil
	}

	now := time.Now()
	checkInterval := time.Duration(cfg.GetIOStarvationCheckInterval()) * time.Second
	r.mu.Lock()
	if now.Sub(r.lastCheck) < checkInterval {
		r.mu.Unlock()
		return nil
	}
	r.lastCheck = now
	r.mu.Unlock()

	leaveOperation := r.opGate.Enter()
	defer leaveOperation()

	starvationThreshold := cfg.GetIOStarvationThreshold()
	psiThreshold := cfg.GetIOPSIThreshold()
	boostMultiplier := cfg.GetIOBoostMultiplier()
	boostDuration := time.Duration(cfg.GetIOBoostDuration()) * time.Second
	boostMaxPerHour := cfg.GetIOBoostMaxPerHour()
	revertOnNormal := cfg.GetIORevertOnNormal()
	deviceFilter := cfg.GetIODeviceFilter()
	readBPS := cfg.GetIOReadBPS()
	writeBPS := cfg.GetIOWriteBPS()
	readIOPS := cfg.GetIOReadIOPS()
	writeIOPS := cfg.GetIOWriteIOPS()
	var remediationErrors []error

	for _, uid := range limitedUsers {
		state, version := r.snapshotForCheck(uid, now)

		// Expiration is independent of the current PSI state.
		if state.IsActive && now.Sub(state.StartTime) >= boostDuration {
			if err := r.revertBoost(deps, uid, &state); err != nil {
				remediationErrors = append(remediationErrors, &ioRemediationError{
					uid:       uid,
					operation: ioRemediationBoostRevertFailure,
					err:       err,
				})
				continue
			}
			if !r.publishIfCurrent(uid, version, state) {
				continue
			}
			r.logger.Info("IO starvation remediation: reverted expired boost", "uid", uid)
			version++
		}

		psiStats, err := deps.GetPSIStats(uid)
		if err != nil {
			remediationErrors = append(remediationErrors, &ioRemediationError{
				uid:       uid,
				operation: ioRemediationPSIReadFailure,
				err:       err,
			})
			continue
		}

		isStarved := psiStats.SomeAvg10 >= psiThreshold

		if isStarved {
			if state.StarvationStart.IsZero() {
				state.StarvationStart = now
			}

			starvationDuration := now.Sub(state.StarvationStart)

			if starvationDuration >= time.Duration(starvationThreshold)*time.Second && !state.IsActive {
				if state.BoostCount >= boostMaxPerHour {
					r.logger.Warn("IO starvation detected but max boosts per hour reached, skipping",
						"uid", uid,
						"boosts_this_hour", state.BoostCount,
						"psi_avg10", psiStats.SomeAvg10,
					)
					r.publishIfCurrent(uid, version, state)
					continue
				}

				if err := r.applyBoost(deps, uid, &state, readBPS, writeBPS, readIOPS, writeIOPS, boostMultiplier, deviceFilter, now); err != nil {
					remediationErrors = append(remediationErrors, &ioRemediationError{
						uid:       uid,
						operation: ioRemediationBoostApplyFailure,
						err:       err,
					})
					r.publishIfCurrent(uid, version, state)
					continue
				}
				if !r.publishIfCurrent(uid, version, state) {
					// A concurrent reset or release won while the cgroup write was in flight.
					if err := deps.ApplyTemporaryIOLimit(uid, readBPS, writeBPS, readIOPS, writeIOPS, deviceFilter, 1); err != nil {
						remediationErrors = append(remediationErrors, &ioRemediationError{
							uid:       uid,
							operation: ioRemediationCompensationFailure,
							err:       err,
						})
					}
					continue
				}
				r.logger.Info("IO starvation remediation: applied temporary boost",
					"uid", uid,
					"multiplier", boostMultiplier,
					"duration", boostDuration,
					"boosts_this_hour", state.BoostCount,
				)
				continue
			}
		} else {
			state.StarvationStart = time.Time{}

			if state.IsActive && revertOnNormal {
				if err := r.revertBoost(deps, uid, &state); err != nil {
					remediationErrors = append(remediationErrors, &ioRemediationError{
						uid:       uid,
						operation: ioRemediationBoostRevertFailure,
						err:       err,
					})
					r.publishIfCurrent(uid, version, state)
					continue
				}
				if r.publishIfCurrent(uid, version, state) {
					r.logger.Info("IO starvation remediation: reverted boost", "uid", uid)
				}
				continue
			}
		}
		r.publishIfCurrent(uid, version, state)
	}

	return remediationErrors
}

func (r *IORemediation) snapshotForCheck(uid int, now time.Time) (IOBoostState, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, exists := r.boostStates[uid]
	if !exists {
		state = &IOBoostState{}
		r.boostStates[uid] = state
	}
	state.LastSeen = now
	r.revision++
	return *state, r.revision
}

func (r *IORemediation) publishIfCurrent(uid int, version uint64, state IOBoostState) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.revision != version {
		return false
	}
	current, exists := r.boostStates[uid]
	if !exists {
		return false
	}
	*current = state
	r.revision++
	return true
}

// applyBoost applies a temporary I/O limit multiplier.
func (r *IORemediation) applyBoost(
	deps IORemediationDeps,
	uid int,
	state *IOBoostState,
	readBPS, writeBPS string,
	readIOPS, writeIOPS int,
	multiplier float64,
	deviceFilter string,
	now time.Time,
) error {
	if err := deps.ApplyTemporaryIOLimit(uid, readBPS, writeBPS, readIOPS, writeIOPS, deviceFilter, multiplier); err != nil {
		return err
	}

	state.OriginalReadBPS = readBPS
	state.OriginalWriteBPS = writeBPS
	state.OriginalReadIOPS = readIOPS
	state.OriginalWriteIOPS = writeIOPS
	state.OriginalDeviceFilter = deviceFilter
	state.IsActive = true
	state.StartTime = now
	state.BoostCount++
	state.LastBoostTime = now

	return nil
}

// revertBoost restores the original I/O limits after a boost.
func (r *IORemediation) revertBoost(deps IORemediationDeps, uid int, state *IOBoostState) error {
	if err := deps.ApplyTemporaryIOLimit(
		uid,
		state.OriginalReadBPS,
		state.OriginalWriteBPS,
		state.OriginalReadIOPS,
		state.OriginalWriteIOPS,
		state.OriginalDeviceFilter,
		1,
	); err != nil {
		return err
	}
	state.IsActive = false
	state.StartTime = time.Time{}
	state.StarvationStart = time.Time{}

	return nil
}

// Cleanup removes inactive remediation state and resets hourly counters.
func (r *IORemediation) Cleanup(maxAge time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	for uid, state := range r.boostStates {
		if !state.IsActive && !state.LastSeen.IsZero() && now.Sub(state.LastSeen) > maxAge {
			delete(r.boostStates, uid)
			r.revision++
			continue
		}
		if !state.LastBoostTime.IsZero() && now.Sub(state.LastBoostTime) > time.Hour {
			state.BoostCount = 0
			r.revision++
		}
	}
}

// ResetActiveBoosts clears temporary IO boost state when resource limiting is suspended.
func (r *IORemediation) ResetActiveBoosts() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	reset := 0
	for _, state := range r.boostStates {
		if !state.IsActive {
			continue
		}
		state.IsActive = false
		state.StartTime = time.Time{}
		state.StarvationStart = time.Time{}
		reset++
	}
	if reset > 0 {
		r.revision++
	}
	return reset
}

// ForgetUsers removes remediation state for cgroups that have been released.
func (r *IORemediation) ForgetUsers(uids []int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, uid := range uids {
		delete(r.boostStates, uid)
	}
	if len(uids) > 0 {
		r.revision++
	}
}
