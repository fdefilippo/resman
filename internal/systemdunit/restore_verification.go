package systemdunit

import (
	"context"
	"errors"
	"time"
)

// verifyRestored waits only for parsed controller values to converge after reload.
// The caller's operation deadline owns the entire wait; no new budget is started.
// Every attempt rechecks authority, identity, exact D-Bus values and the empty
// mutable footprint. A kernel retry never permits an external change to be adopted.
func (a *Adapter) verifyRestored(ctx context.Context, identity UnitIdentity, assignments []PropertyAssignment) error {
	const interval = 10 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return classifyTransportError("restore_readback", identity.Name, err)
		}
		after, err := a.readUnit(ctx, identity.Name, identity.ObjectPath)
		if err != nil {
			return err
		}
		if err := requireSameIdentity("restore_readback", identity, after.Identity); err != nil {
			return err
		}
		if err := verifyReadback("restore_readback", after, assignments); err != nil {
			return err
		}
		if err := a.requireManagedUnitFileFootprint("restore_readback", after, unitOverrideLease{}, false); err != nil {
			return err
		}
		err = a.verifier.verify(after, assignments)
		if err == nil {
			return nil
		}
		failure := &AdapterError{Reason: ReasonKernelVerification, Operation: "restore_readback", Unit: identity.Name, Err: err}
		var pending *kernelValueMismatch
		if !errors.As(err, &pending) {
			return failure
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(failure, ctx.Err())
		case <-timer.C:
		}
	}
}

func (a *Adapter) baselineAssignments(unit string) []PropertyAssignment {
	var result []PropertyAssignment
	for key, value := range a.leases {
		if key.identity.Name == unit {
			result = append(result, PropertyAssignment{name: key.property, value: clonePropertyValue(key.property, value.baseline), ioDeviceWeightTargets: cloneIODeviceWeightTargets(value.ioWeightTargets)})
		}
	}
	return result
}
