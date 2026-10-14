package systemdunit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

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
	startCapabilityProbe(context.Context, string, []PropertyAssignment) (listedUnit, bool, error)
	stopCapabilityProbe(context.Context, string) error
}

type managerDiagnosticTransport interface {
	managerProperty(context.Context, string) (string, error)
}

type dbusTransport struct {
	conn       *systemdbus.Conn
	revertConn *godbus.Conn
	revertObj  godbus.BusObject
	cancel     context.CancelFunc
}

const capabilityProbeExecutable = "/usr/bin/sleep"

// systemdPrivateSocket is the peer-to-peer D-Bus endpoint PID 1 listens on from
// the first instant of boot, before dbus-daemon exists. It serves the same
// org.freedesktop.systemd1 objects as the system bus, authorizes root by peer
// credentials without polkit, and is what systemctl uses when run as root.
// ResMan starts before sysinit.target, ahead of dbus.socket, so the system bus
// is never available when it must establish enforcement (resman-318.2).
const systemdPrivateSocket = "/run/systemd/private"

const systemdPrivateAddress = "unix:path=" + systemdPrivateSocket

func openDBusTransport(runCtx context.Context) (*dbusTransport, error) {
	// The transport owns its connection lifetime. Operation contexts may be
	// cancelled to stop the control loop, but Close is the only event that may
	// tear down the connection while shutdown restoration is still in progress.
	lifecycleCtx, cancel := newDBusTransportLifecycle(runCtx)
	conn, err := systemdbus.NewSystemdConnectionContext(lifecycleCtx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("connect to systemd at %s: %w", systemdPrivateSocket, err)
	}
	revertConn, err := dialSystemdPrivate(lifecycleCtx)
	if err != nil {
		conn.Close()
		cancel()
		return nil, fmt.Errorf("open private systemd connection for guarded unit-file cleanup: %w", err)
	}
	return &dbusTransport{
		conn:       conn,
		revertConn: revertConn,
		revertObj:  revertConn.Object("org.freedesktop.systemd1", godbus.ObjectPath("/org/freedesktop/systemd1")),
		cancel:     cancel,
	}, nil
}

// dialSystemdPrivate opens an authenticated peer-to-peer connection to PID 1.
// There is no bus daemon on this socket, so no Hello is sent: the peer is
// systemd itself, and EXTERNAL authentication by numeric uid needs no NSS.
func dialSystemdPrivate(ctx context.Context) (*godbus.Conn, error) {
	conn, err := godbus.Dial(systemdPrivateAddress, godbus.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	if err := conn.Auth([]godbus.Auth{godbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("authenticate to systemd at %s: %w", systemdPrivateSocket, err)
	}
	return conn, nil
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

func (t *dbusTransport) managerProperty(ctx context.Context, property string) (string, error) {
	var value godbus.Variant
	if err := t.revertObj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0,
		"org.freedesktop.systemd1.Manager", property).Store(&value); err != nil {
		return "", err
	}
	var result string
	if err := value.Store(&result); err != nil {
		return "", fmt.Errorf("decode Manager.%s: %w", property, err)
	}
	return result, nil
}

func (t *dbusTransport) setUnitProperties(ctx context.Context, unit string, runtime bool, assignments []PropertyAssignment) error {
	properties := make([]systemdbus.Property, len(assignments))
	// The adapter has already canonicalized ordinary assignments. Preserve its
	// order because a repeated per-device property may encode reset followed by
	// replacement within this single method call.
	for index, assignment := range assignments {
		properties[index] = systemdbus.Property{
			Name:  string(assignment.name),
			Value: godbus.MakeVariant(dbusPropertyValue(assignment)),
		}
	}
	return t.conn.SetUnitPropertiesContext(ctx, unit, runtime, properties...)
}

func (t *dbusTransport) startCapabilityProbe(ctx context.Context, unit string, assignments []PropertyAssignment) (listedUnit, bool, error) {
	service := capabilityProbeServiceUnit(unit)
	serviceProperties, properties := capabilityProbeProperties(unit, assignments)
	result := make(chan string, 1)
	if _, err := t.conn.StartTransientUnitAux(ctx, service, "fail", serviceProperties, []systemdbus.PropertyCollection{{
		Name: unit, Properties: properties,
	}}, result); err != nil {
		return listedUnit{}, false, classifyTransportError("start_capability_probe", unit, err)
	}
	select {
	case outcome := <-result:
		if outcome != "done" {
			return listedUnit{}, true, fmt.Errorf("transient capability probe start completed with %s", outcome)
		}
	case <-ctx.Done():
		return listedUnit{}, true, ctx.Err()
	}
	statuses, err := t.conn.ListUnitsByPatternsContext(ctx, []string{"active"}, []string{unit})
	if err != nil {
		return listedUnit{}, true, classifyTransportError("list_capability_probe", unit, err)
	}
	if len(statuses) != 1 || statuses[0].Name != unit || statuses[0].LoadState != "loaded" || statuses[0].ActiveState != "active" {
		return listedUnit{}, true, fmt.Errorf("transient capability probe was not returned as one active loaded unit")
	}
	return listedUnit{
		name: unit, objectPath: string(statuses[0].Path),
		loadState: statuses[0].LoadState, activeState: statuses[0].ActiveState,
	}, true, nil
}

// capabilityProbeProperties builds the transient probe service and its slice.
// Only the weighted-I/O probe uses them, after readiness: a slice below
// user.slice inherits After=systemd-user-sessions.service from systemd's
// user-.slice.d drop-in, so it can never run before sysinit.target.
func capabilityProbeProperties(unit string, assignments []PropertyAssignment) ([]systemdbus.Property, []systemdbus.Property) {
	sliceProperties := []systemdbus.Property{
		systemdbus.PropDescription("ResMan cgroup interface capability probe"),
	}
	for _, assignment := range assignments {
		sliceProperties = append(sliceProperties, systemdbus.Property{
			Name:  string(assignment.name),
			Value: godbus.MakeVariant(dbusPropertyValue(assignment)),
		})
	}
	serviceProperties := []systemdbus.Property{
		systemdbus.PropDescription("ResMan bounded cgroup interface capability probe"),
		systemdbus.PropSlice(unit),
		systemdbus.PropExecStart([]string{capabilityProbeExecutable, "30"}, false),
	}
	return serviceProperties, sliceProperties
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
			return classified
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
	return errors.Join(stop(capabilityProbeServiceUnit(unit)), stop(unit))
}

func (t *dbusTransport) loadedServices(ctx context.Context, names []string) ([]string, error) {
	statuses, err := t.conn.ListUnitsByNamesContext(ctx, names)
	if err != nil {
		return nil, err
	}
	loaded := make([]string, 0, len(statuses))
	for _, status := range statuses {
		if status.LoadState == "loaded" {
			loaded = append(loaded, status.Name)
		}
	}
	return loaded, nil
}

// Linux scheduling policies systemd reports in CPUSchedulingPolicy.
const (
	schedulingPolicyFIFO = 1
	schedulingPolicyRR   = 2
)

func (t *dbusTransport) realtimeScheduledServices(ctx context.Context) ([]string, error) {
	statuses, err := t.conn.ListUnitsByPatternsContext(ctx, nil, []string{"*.service"})
	if err != nil {
		return nil, err
	}
	var scheduled []string
	for _, status := range statuses {
		if status.LoadState != "loaded" {
			continue
		}
		property, err := t.conn.GetServicePropertyContext(ctx, status.Name, "CPUSchedulingPolicy")
		if err != nil {
			return nil, err
		}
		policy, ok := property.Value.Value().(int32)
		if !ok {
			return nil, fmt.Errorf("decode %s CPUSchedulingPolicy: unexpected %s", status.Name, property.Value.Signature())
		}
		if policy == schedulingPolicyFIFO || policy == schedulingPolicyRR {
			scheduled = append(scheduled, status.Name)
		}
	}
	return scheduled, nil
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
