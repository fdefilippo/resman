package systemdunit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// IODeviceWeightDeviceNumber is one canonical Linux block-device identity.
type IODeviceWeightDeviceNumber struct {
	Major uint32
	Minor uint32
}

// String returns the canonical major:minor identity.
func (n IODeviceWeightDeviceNumber) String() string {
	return fmt.Sprintf("%d:%d", n.Major, n.Minor)
}

// ParseIODeviceWeightDevices parses the enabled weighted-I/O selector. The
// disabled empty value is handled by the caller and is not an enabled plan.
func ParseIODeviceWeightDevices(selector string) ([]IODeviceWeightDeviceNumber, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, newIODeviceWeightCapabilityError(IODeviceWeightReasonEmptySelector, "", fmt.Errorf("enabled weighted I/O requires at least one device"))
	}
	parts := strings.Split(selector, ",")
	result := make([]IODeviceWeightDeviceNumber, 0, len(parts))
	seen := make(map[IODeviceWeightDeviceNumber]bool, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		fields := strings.Split(value, ":")
		if len(fields) != 2 || fields[0] == "" || fields[1] == "" {
			return nil, newIODeviceWeightCapabilityError(IODeviceWeightReasonInvalidSelector, value, fmt.Errorf("device must use canonical major:minor syntax"))
		}
		major, err := parseCanonicalDeviceComponent(fields[0])
		if err != nil {
			return nil, newIODeviceWeightCapabilityError(IODeviceWeightReasonInvalidSelector, value, fmt.Errorf("invalid major number: %w", err))
		}
		minor, err := parseCanonicalDeviceComponent(fields[1])
		if err != nil {
			return nil, newIODeviceWeightCapabilityError(IODeviceWeightReasonInvalidSelector, value, fmt.Errorf("invalid minor number: %w", err))
		}
		number := IODeviceWeightDeviceNumber{Major: major, Minor: minor}
		if number.Major == 0 && number.Minor == 0 {
			return nil, newIODeviceWeightCapabilityError(IODeviceWeightReasonInvalidSelector, value, fmt.Errorf("device 0:0 is not a usable block target"))
		}
		encoded := unix.Mkdev(number.Major, number.Minor)
		if unix.Major(encoded) != number.Major || unix.Minor(encoded) != number.Minor {
			return nil, newIODeviceWeightCapabilityError(IODeviceWeightReasonInvalidSelector, value, fmt.Errorf("device number exceeds the Linux dev_t domain"))
		}
		if seen[number] {
			return nil, newIODeviceWeightCapabilityError(IODeviceWeightReasonDuplicateDevice, number.String(), fmt.Errorf("device appears more than once"))
		}
		seen[number] = true
		result = append(result, number)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Major != result[right].Major {
			return result[left].Major < result[right].Major
		}
		return result[left].Minor < result[right].Minor
	})
	return result, nil
}

func parseCanonicalDeviceComponent(value string) (uint32, error) {
	if value == "" || value != "0" && value[0] == '0' {
		return 0, fmt.Errorf("non-canonical decimal value %q", value)
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, err
	}
	if strconv.FormatUint(parsed, 10) != value {
		return 0, fmt.Errorf("non-canonical decimal value %q", value)
	}
	return uint32(parsed), nil
}

type ioDeviceWeightResolvedDevice struct {
	identity IODeviceWeightDeviceIdentity
	sysfs    string
}

func (c *IODeviceWeightCapabilityClassifier) resolveDevice(number IODeviceWeightDeviceNumber) (ioDeviceWeightResolvedDevice, error) {
	requested := number.String()
	link := filepath.Join(c.io.sysDevBlockRoot, requested)
	resolved, err := c.io.evalSymlinks(link)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonDeviceMissing, requested, err)
		}
		return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonEvidenceUnavailable, requested, fmt.Errorf("resolve sysfs device: %w", err))
	}
	resolved = filepath.Clean(resolved)
	if !pathWithin(c.io.sysRoot, resolved) {
		return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonAmbiguousTopology, requested, fmt.Errorf("resolved sysfs path %s escapes %s", resolved, c.io.sysRoot))
	}
	data, err := c.io.readFile(filepath.Join(resolved, "dev"))
	if err != nil {
		return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonEvidenceUnavailable, requested, fmt.Errorf("read sysfs device identity: %w", err))
	}
	if strings.TrimSpace(string(data)) != requested {
		return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonAmbiguousTopology, requested, fmt.Errorf("sysfs identity is %q", strings.TrimSpace(string(data))))
	}
	if _, err := c.io.readFile(filepath.Join(resolved, "partition")); err == nil {
		return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonAmbiguousTopology, requested, fmt.Errorf("partitions are not supported weighted-I/O targets"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonEvidenceUnavailable, requested, fmt.Errorf("inspect partition marker: %w", err))
	}
	for _, marker := range []string{"dm", "md"} {
		if _, err := c.io.stat(filepath.Join(resolved, marker)); err == nil {
			return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonAmbiguousTopology, requested, fmt.Errorf("%s block topology is not supported", marker))
		} else if !errors.Is(err, os.ErrNotExist) {
			return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonEvidenceUnavailable, requested, fmt.Errorf("inspect %s topology marker: %w", marker, err))
		}
	}
	slaves, err := c.io.readDir(filepath.Join(resolved, "slaves"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonEvidenceUnavailable, requested, fmt.Errorf("inspect request-queue parents: %w", err))
	}
	if len(slaves) != 0 {
		return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonAmbiguousTopology, requested, fmt.Errorf("stacked or fan-out block topology has %d request-queue parents", len(slaves)))
	}
	queue, err := c.io.stat(filepath.Join(resolved, "queue"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonAmbiguousTopology, requested, fmt.Errorf("device has no originating request queue"))
		}
		return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonEvidenceUnavailable, requested, fmt.Errorf("inspect request queue: %w", err))
	}
	if !queue.IsDir() {
		return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonAmbiguousTopology, requested, fmt.Errorf("request queue is not a directory"))
	}
	uevent, err := c.io.readFile(filepath.Join(resolved, "uevent"))
	if err != nil {
		return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonEvidenceUnavailable, requested, fmt.Errorf("read device uevent: %w", err))
	}
	name := ueventValue(string(uevent), "DEVNAME")
	if name == "" || filepath.IsAbs(name) || filepath.Clean(name) != name || strings.HasPrefix(name, "..") {
		return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonAmbiguousTopology, requested, fmt.Errorf("invalid DEVNAME %q", name))
	}
	if !approvedDirectIODevice(resolved, name) {
		return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonAmbiguousTopology, requested, fmt.Errorf("device %s is not an approved direct virtio, SCSI or NVMe request queue", name))
	}
	deviceNode := filepath.Join(c.io.devRoot, name)
	actual, err := c.deviceNumber(deviceNode)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonDeviceMissing, requested, fmt.Errorf("inspect %s: %w", deviceNode, err))
		}
		return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonEvidenceUnavailable, requested, fmt.Errorf("inspect %s: %w", deviceNode, err))
	}
	if actual != requested {
		return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonDeviceIdentityChanged, requested, fmt.Errorf("device node %s resolves to %s", deviceNode, actual))
	}
	info, err := c.io.stat(resolved)
	if err != nil {
		return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonEvidenceUnavailable, requested, fmt.Errorf("stat sysfs identity: %w", err))
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino == 0 {
		return ioDeviceWeightResolvedDevice{}, newIODeviceWeightCapabilityError(IODeviceWeightReasonEvidenceUnavailable, requested, fmt.Errorf("sysfs identity has no stable inode"))
	}
	return ioDeviceWeightResolvedDevice{
		identity: IODeviceWeightDeviceIdentity{
			Number: number, DeviceNode: deviceNode, SysfsPath: resolved, SysfsInode: stat.Ino,
		},
		sysfs: resolved,
	}, nil
}

func approvedDirectIODevice(sysfsPath, name string) bool {
	if filepath.Base(sysfsPath) != name || strings.Contains(name, "/") {
		return false
	}
	components := strings.Split(filepath.Clean(sysfsPath), string(filepath.Separator))
	switch {
	case strings.HasPrefix(name, "vd"):
		return hasPathComponentPrefix(components, "virtio")
	case strings.HasPrefix(name, "sd"):
		return hasPathComponentPrefix(components, "host") && hasPathComponentPrefix(components, "target")
	default:
		controller, ok := directNVMeNamespace(name)
		return ok && !hasPathComponent(components, "virtual") && !hasPathComponentPrefix(components, "nvme-subsys") && hasPathComponent(components, controller)
	}
}

func directNVMeNamespace(name string) (string, bool) {
	if !strings.HasPrefix(name, "nvme") {
		return "", false
	}
	remainder := strings.TrimPrefix(name, "nvme")
	controllerEnd := 0
	for controllerEnd < len(remainder) && remainder[controllerEnd] >= '0' && remainder[controllerEnd] <= '9' {
		controllerEnd++
	}
	if controllerEnd == 0 || controllerEnd >= len(remainder) || remainder[controllerEnd] != 'n' {
		return "", false
	}
	namespace := remainder[controllerEnd+1:]
	if namespace == "" {
		return "", false
	}
	for _, character := range namespace {
		if character < '0' || character > '9' {
			return "", false
		}
	}
	return "nvme" + remainder[:controllerEnd], true
}

func hasPathComponent(components []string, wanted string) bool {
	for _, component := range components {
		if component == wanted {
			return true
		}
	}
	return false
}

func hasPathComponentPrefix(components []string, prefix string) bool {
	for _, component := range components {
		if strings.HasPrefix(component, prefix) {
			return true
		}
	}
	return false
}

func (c *IODeviceWeightCapabilityClassifier) deviceNumber(path string) (string, error) {
	return (cgroupVerifier{stat: c.io.stat}).deviceNumber(path)
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
