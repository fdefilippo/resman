package systemdunit

import (
	"context"
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
	close()
}

type dbusTransport struct {
	conn   *systemdbus.Conn
	cancel context.CancelFunc
}

func openDBusTransport(ctx context.Context) (*dbusTransport, error) {
	lifecycleCtx, cancel := context.WithCancel(ctx)
	conn, err := systemdbus.NewSystemConnectionContext(lifecycleCtx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("connect to the system bus: %w", err)
	}
	return &dbusTransport{conn: conn, cancel: cancel}, nil
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
			Value: godbus.MakeVariant(assignment.value),
		}
	}
	return t.conn.SetUnitPropertiesContext(ctx, unit, runtime, properties...)
}

func (t *dbusTransport) close() {
	t.cancel()
	t.conn.Close()
}
