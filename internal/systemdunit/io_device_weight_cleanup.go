package systemdunit

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// ioDeviceWeightResetConflict marks a kernel tuple that no longer equals the
// exact ResMan-owned value. The adapter preserves the lease and requires
// operator intervention instead of overwriting the divergent value.
type ioDeviceWeightResetConflict struct{ err error }

func (e *ioDeviceWeightResetConflict) Error() string { return e.err.Error() }
func (e *ioDeviceWeightResetConflict) Unwrap() error { return e.err }

func (v cgroupVerifier) prepareIODeviceWeightResets(snapshot UnitSnapshot, previous, desired PropertyAssignment) ([]ioDeviceWeightReset, error) {
	if previous.name != PropertyIODeviceWeight || desired.name != PropertyIODeviceWeight {
		return nil, fmt.Errorf("keyed reset preparation is restricted to %s", PropertyIODeviceWeight)
	}
	if snapshot.Identity.ControlGroupID == 0 {
		return nil, fmt.Errorf("keyed reset preparation requires an authoritative cgroup identity")
	}
	return prepareIODeviceWeightResets(previous, desired, v.deviceNumber)
}

func prepareIODeviceWeightResets(previous, desired PropertyAssignment, deviceNumber func(string) (string, error)) ([]ioDeviceWeightReset, error) {
	desiredPaths := make(map[string]bool, len(desired.value.devices))
	for _, value := range desired.value.devices {
		desiredPaths[value.Path] = true
	}
	resets := make([]ioDeviceWeightReset, 0)
	seenDevices := make(map[string]string)
	for _, value := range previous.value.devices {
		if desiredPaths[value.Path] {
			continue
		}
		target, ok := ioDeviceWeightTargetByPath(previous.ioDeviceWeightTargets, value.Path)
		if !ok || !validIODeviceWeightMechanism(target.mechanism) {
			return nil, fmt.Errorf("removed device %s has no exact owned mechanism context", value.Path)
		}
		device, err := deviceNumber(value.Path)
		if err != nil {
			return nil, fmt.Errorf("resolve removed block device %s: %w", value.Path, err)
		}
		if earlier, exists := seenDevices[device]; exists {
			return nil, fmt.Errorf("removed paths %s and %s resolve to the same device %s", earlier, value.Path, device)
		}
		seenDevices[device] = value.Path
		resets = append(resets, ioDeviceWeightReset{
			path: value.Path, device: device, mechanism: target.mechanism,
			expected: kernelIODeviceWeight(value.Value, target.mechanism),
		})
	}
	sort.Slice(resets, func(left, right int) bool { return resets[left].path < resets[right].path })
	return resets, nil
}

func (v cgroupVerifier) resetIODeviceWeightOverrides(snapshot UnitSnapshot, resets []ioDeviceWeightReset) error {
	if len(resets) == 0 {
		return nil
	}
	path, err := v.controlGroupPath(snapshot.ControlGroup)
	if err != nil {
		return err
	}
	if current, err := v.identity(snapshot.ControlGroup); err != nil || current != snapshot.Identity.ControlGroupID {
		if err == nil {
			err = fmt.Errorf("cgroup inode is %d, expected %d", current, snapshot.Identity.ControlGroupID)
		}
		return &ioDeviceWeightResetConflict{fmt.Errorf("confirm cgroup identity before keyed reset: %w", err)}
	}
	seen := make(map[string]bool, len(resets))
	present := make(map[string]bool, len(resets))
	for _, reset := range resets {
		if !validAbsolutePath(reset.path) || !validIODeviceWeightMechanism(reset.mechanism) || reset.expected == 0 {
			return fmt.Errorf("invalid durable %s keyed-reset context", PropertyIODeviceWeight)
		}
		key := string(reset.mechanism) + "\x00" + reset.device
		if seen[key] {
			return fmt.Errorf("duplicate durable %s keyed-reset tuple %s", PropertyIODeviceWeight, reset.device)
		}
		seen[key] = true
		device, deviceErr := v.deviceNumber(reset.path)
		if deviceErr != nil || device != reset.device {
			if deviceErr == nil {
				deviceErr = fmt.Errorf("device path now resolves to %s, expected %s", device, reset.device)
			}
			return &ioDeviceWeightResetConflict{fmt.Errorf("confirm removed device identity for %s: %w", reset.path, deviceErr)}
		}
		actual, exists, readErr := v.readIODeviceWeightResetValue(path, snapshot.Identity.Name, reset)
		if readErr != nil {
			return readErr
		}
		if exists && actual != reset.expected {
			return &ioDeviceWeightResetConflict{fmt.Errorf("effective %s for %s device %s changed from owned value %d to %d", PropertyIODeviceWeight, snapshot.Identity.Name, reset.device, reset.expected, actual)}
		}
		present[key] = exists
	}
	writer := v.resetIODeviceWeight
	if writer == nil {
		writer = writeIODeviceWeightReset
	}
	for _, reset := range resets {
		key := string(reset.mechanism) + "\x00" + reset.device
		if !present[key] {
			continue
		}
		device, deviceErr := v.deviceNumber(reset.path)
		if deviceErr != nil || device != reset.device {
			if deviceErr == nil {
				deviceErr = fmt.Errorf("device path now resolves to %s, expected %s", device, reset.device)
			}
			return &ioDeviceWeightResetConflict{fmt.Errorf("reconfirm removed device identity for %s: %w", reset.path, deviceErr)}
		}
		if err := writer(v.root, snapshot.ControlGroup, snapshot.Identity.ControlGroupID, reset); err != nil {
			return err
		}
	}
	if current, err := v.identity(snapshot.ControlGroup); err != nil || current != snapshot.Identity.ControlGroupID {
		if err == nil {
			err = fmt.Errorf("cgroup inode is %d, expected %d", current, snapshot.Identity.ControlGroupID)
		}
		return &ioDeviceWeightResetConflict{fmt.Errorf("confirm cgroup identity after keyed reset: %w", err)}
	}
	for _, reset := range resets {
		actual, exists, err := v.readIODeviceWeightResetValue(path, snapshot.Identity.Name, reset)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("effective %s for %s device %s remains at %d after keyed reset", PropertyIODeviceWeight, snapshot.Identity.Name, reset.device, actual)
		}
	}
	return nil
}

func (v cgroupVerifier) readIODeviceWeightResetValue(path, unit string, reset ioDeviceWeightReset) (uint64, bool, error) {
	filename := ioDeviceWeightKernelFile(reset.mechanism)
	data, err := v.readFile(filepath.Join(path, filename))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read effective %s for %s keyed reset: %w", filename, unit, err)
	}
	values, err := parseKernelIODeviceWeights(string(data))
	if err != nil {
		return 0, false, fmt.Errorf("parse effective %s for %s keyed reset: %w", filename, unit, err)
	}
	value, present := values[reset.device]
	return value, present, nil
}

// writeIODeviceWeightReset is the only raw cgroup mutation admitted by the
// adapter. It writes exactly "MAJ:MIN default" to the active typed weight file
// after rechecking the unit inode and the exact owned kernel value on one fd.
func writeIODeviceWeightReset(root, controlGroup string, identity uint64, reset ioDeviceWeightReset) error {
	dirFD, err := openIODeviceWeightCgroup(root, controlGroup)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(dirFD) }()
	var stat unix.Stat_t
	if err := unix.Fstat(dirFD, &stat); err != nil {
		return fmt.Errorf("stat cgroup before %s keyed reset: %w", PropertyIODeviceWeight, err)
	}
	if stat.Ino != identity {
		return &ioDeviceWeightResetConflict{fmt.Errorf("cgroup inode is %d, expected %d", stat.Ino, identity)}
	}
	filename := ioDeviceWeightKernelFile(reset.mechanism)
	fd, err := unix.Openat(dirFD, filename, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open %s for exact keyed reset: %w", filename, err)
	}
	file := os.NewFile(uintptr(fd), filename)
	if file == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("adopt %s descriptor for exact keyed reset", filename)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("read %s immediately before exact keyed reset: %w", filename, err)
	}
	values, err := parseKernelIODeviceWeights(string(data))
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("parse %s immediately before exact keyed reset: %w", filename, err)
	}
	current, present := values[reset.device]
	if !present {
		return file.Close()
	}
	if current != reset.expected {
		_ = file.Close()
		return &ioDeviceWeightResetConflict{fmt.Errorf("device %s changed from owned value %d to %d", reset.device, reset.expected, current)}
	}
	if _, err := file.Seek(0, 0); err != nil {
		_ = file.Close()
		return fmt.Errorf("rewind %s before exact keyed reset: %w", filename, err)
	}
	written, err := file.Write([]byte(reset.device + " default\n"))
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("write exact keyed reset to %s: %w", filename, err)
	}
	if written != len(reset.device)+len(" default\n") {
		_ = file.Close()
		return fmt.Errorf("write exact keyed reset to %s: wrote %d of %d bytes", filename, written, len(reset.device)+len(" default\n"))
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s after exact keyed reset: %w", filename, err)
	}
	return nil
}

func openIODeviceWeightCgroup(root, controlGroup string) (int, error) {
	if !validControlGroup(controlGroup) {
		return -1, fmt.Errorf("invalid authoritative control-group path %q", controlGroup)
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, fmt.Errorf("open cgroup root %s without following symlinks: %w", root, err)
	}
	currentFD := rootFD
	for _, component := range strings.Split(strings.TrimPrefix(controlGroup, "/"), "/") {
		if component == "" {
			continue
		}
		nextFD, openErr := unix.Openat(currentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if currentFD != rootFD {
			_ = unix.Close(currentFD)
		}
		if openErr != nil {
			_ = unix.Close(rootFD)
			return -1, fmt.Errorf("open cgroup component %q beneath %s without following symlinks: %w", component, root, openErr)
		}
		currentFD = nextFD
	}
	if currentFD != rootFD {
		_ = unix.Close(rootFD)
	}
	return currentFD, nil
}
