package systemdunit

import (
	"context"
	"errors"
	"fmt"
	"sort"

	systemdbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

type listedUnit struct {
	name        string
	objectPath  string
	loadState   string
	activeState string
}

type unitTransport interface {
	listUserSlices(context.Context) ([]listedUnit, error)
	unitProperties(context.Context, string) (map[string]any, error)
	sliceProperties(context.Context, string) (map[string]any, error)
	setUnitProperties(context.Context, string, bool, []PropertyAssignment) error
	revertUnitFiles(context.Context, string) error
	reload(context.Context) error
	close()
}

type startupCapabilityTransport interface {
	startCapabilityProbe(context.Context, string, []PropertyAssignment) (string, bool, error)
	stopCapabilityProbe(context.Context, string) error
}

type dbusTransport struct {
	conn       *systemdbus.Conn
	revertConn *godbus.Conn
	revertObj  godbus.BusObject
	cancel     context.CancelFunc
}

func openDBusTransport(runCtx context.Context) (*dbusTransport, error) {
	// The transport owns its connection lifetime. Operation contexts may be
	// cancelled to stop the control loop, but Close is the only event that may
	// tear down the bus while shutdown restoration is still in progress.
	lifecycleCtx, cancel := newDBusTransportLifecycle(runCtx)
	conn, err := systemdbus.NewSystemConnectionContext(lifecycleCtx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("connect to the system bus: %w", err)
	}
	revertConn, err := godbus.SystemBusPrivate()
	if err != nil {
		conn.Close()
		cancel()
		return nil, fmt.Errorf("open private system bus connection for guarded unit-file cleanup: %w", err)
	}
	if err := revertConn.Auth(nil); err != nil {
		_ = revertConn.Close()
		conn.Close()
		cancel()
		return nil, fmt.Errorf("authenticate private system bus connection for guarded unit-file cleanup: %w", err)
	}
	if err := revertConn.Hello(); err != nil {
		_ = revertConn.Close()
		conn.Close()
		cancel()
		return nil, fmt.Errorf("initialize private system bus connection for guarded unit-file cleanup: %w", err)
	}
	return &dbusTransport{
		conn:       conn,
		revertConn: revertConn,
		revertObj:  revertConn.Object("org.freedesktop.systemd1", godbus.ObjectPath("/org/freedesktop/systemd1")),
		cancel:     cancel,
	}, nil
}

func newDBusTransportLifecycle(_ context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func (t *dbusTransport) listUserSlices(ctx context.Context) ([]listedUnit, error) {
	statuses, err := t.conn.ListUnitsByPatternsContext(ctx, []string{"active"}, []string{"user.slice", "user-*.slice"})
	if err != nil {
		return nil, err
	}
	result := make([]listedUnit, 0, len(statuses))
	for _, status := range statuses {
		result = append(result, listedUnit{
			name:        status.Name,
			objectPath:  string(status.Path),
			loadState:   status.LoadState,
			activeState: status.ActiveState,
		})
	}
	return result, nil
}

func (t *dbusTransport) unitProperties(ctx context.Context, unit string) (map[string]any, error) {
	return t.conn.GetUnitPropertiesContext(ctx, unit)
}

func (t *dbusTransport) sliceProperties(ctx context.Context, unit string) (map[string]any, error) {
	return t.conn.GetUnitTypePropertiesContext(ctx, unit, "Slice")
}

func (t *dbusTransport) setUnitProperties(ctx context.Context, unit string, runtime bool, assignments []PropertyAssignment) error {
	properties := make([]systemdbus.Property, len(assignments))
	ordered := append([]PropertyAssignment(nil), assignments...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].name < ordered[j].name })
	for index, assignment := range ordered {
		properties[index] = systemdbus.Property{
			Name:  string(assignment.name),
			Value: godbus.MakeVariant(dbusPropertyValue(assignment)),
		}
	}
	return t.conn.SetUnitPropertiesContext(ctx, unit, runtime, properties...)
}

func (t *dbusTransport) startCapabilityProbe(ctx context.Context, unit string, assignments []PropertyAssignment) (string, bool, error) {
	start := func(target string) error {
		properties := []systemdbus.Property{systemdbus.PropDescription("ResMan cgroup interface capability probe")}
		for _, assignment := range assignments {
			properties = append(properties, systemdbus.Property{
				Name:  string(assignment.name),
				Value: godbus.MakeVariant(dbusPropertyValue(assignment)),
			})
		}
		result := make(chan string, 1)
		if _, err := t.conn.StartTransientUnitContext(ctx, target, "fail", properties, result); err != nil {
			return err
		}
		select {
		case outcome := <-result:
			if outcome != "done" {
				return fmt.Errorf("transient capability probe start completed with %s", outcome)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
	if err := start(unit); err != nil {
		return "", false, err
	}
	if err := start(capabilityProbeLeafUnit(unit)); err != nil {
		return "", true, err
	}
	propertiesByName, err := t.conn.GetUnitTypePropertiesContext(ctx, unit, "Slice")
	if err != nil {
		return "", true, err
	}
	controlGroup, ok := propertiesByName["ControlGroup"].(string)
	if !ok || !validControlGroup(controlGroup) {
		return "", true, fmt.Errorf("transient capability probe returned an invalid ControlGroup")
	}
	return controlGroup, true, nil
}

func (t *dbusTransport) stopCapabilityProbe(ctx context.Context, unit string) error {
	stop := func(target string) error {
		result := make(chan string, 1)
		if _, err := t.conn.StopUnitContext(ctx, target, "replace", result); err != nil {
			classified := classifyTransportError("stop_capability_probe", target, err)
			var adapterErr *AdapterError
			if errors.As(classified, &adapterErr) && adapterErr.Reason == ReasonUnitMissing {
				return nil
			}
			return err
		}
		select {
		case outcome := <-result:
			if outcome != "done" {
				return fmt.Errorf("transient capability probe stop completed with %s", outcome)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
	return errors.Join(stop(capabilityProbeLeafUnit(unit)), stop(unit))
}

type dbusDeviceLimit struct {
	Path  string
	Value uint64
}

func dbusPropertyValue(assignment PropertyAssignment) any {
	if _, deviceProperty := approvedDeviceProperties[assignment.name]; !deviceProperty {
		return assignment.value.scalar
	}
	values := make([]dbusDeviceLimit, len(assignment.value.devices))
	for index, value := range assignment.value.devices {
		values[index] = dbusDeviceLimit(value)
	}
	return values
}

func (t *dbusTransport) revertUnitFiles(ctx context.Context, unit string) error {
	return t.revertObj.CallWithContext(ctx, "org.freedesktop.systemd1.Manager.RevertUnitFiles", 0, []string{unit}).Err
}

func (t *dbusTransport) reload(ctx context.Context) error { return t.conn.ReloadContext(ctx) }

func (t *dbusTransport) close() {
	t.cancel()
	_ = t.revertConn.Close()
	t.conn.Close()
}
