package systemdunit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type observedSystemdMethod struct {
	Input  string `json:"input"`
	Output string `json:"output"`
}

type observedSystemdInterface struct {
	Properties       map[string]string                `json:"properties,omitempty"`
	AbsentProperties []string                         `json:"absent_properties,omitempty"`
	Methods          map[string]observedSystemdMethod `json:"methods,omitempty"`
}

type observedSystemdContract struct {
	SchemaVersion int    `json:"schema_version"`
	Profile       string `json:"profile"`
	Source        struct {
		CaptureKind    string `json:"capture_kind"`
		Distribution   string `json:"distribution"`
		Image          string `json:"image"`
		ImageID        string `json:"image_id"`
		ImageDigest    string `json:"image_digest"`
		SourceRevision string `json:"source_revision"`
		SystemdNEVRA   string `json:"systemd_nevra"`
		Kernel         string `json:"kernel"`
		CapturedOn     string `json:"captured_on"`
		Environment    string `json:"environment"`
	} `json:"source"`
	Cgroup struct {
		Filesystem  string   `json:"filesystem"`
		Controllers []string `json:"controllers"`
	} `json:"cgroup"`
	Interfaces map[string]observedSystemdInterface `json:"interfaces"`
}

const el8Systemd239EvidenceDirectory = "../../test/functional/systemd239/evidence/rhel8-systemd239-82-el8_10_19"

var el8Systemd239EvidenceFiles = []string{
	"cgroup-controllers.txt",
	"cgroup-filesystem.txt",
	"environment.txt",
	"kernel.txt",
	"manager-interface.txt",
	"os-release.txt",
	"result.txt",
	"slice-interface.txt",
	"systemd-package.txt",
	"systemd-version.txt",
	"unit-interface.txt",
	"user-slice-identity.txt",
	"user-slice-values.txt",
}

func TestEL8Systemd239ContractPinsTheAdapterDBusSurface(t *testing.T) {
	contract := loadEL8Systemd239Contract(t)
	if contract.SchemaVersion != 1 || contract.Profile != "rhel8-systemd239-cgroup2" {
		t.Fatalf("contract identity = %d/%q", contract.SchemaVersion, contract.Profile)
	}
	if contract.Source.CaptureKind != "dbus_introspection" || !strings.Contains(contract.Source.SystemdNEVRA, "systemd-239-") {
		t.Fatalf("contract source is not a systemd 239 D-Bus capture: %+v", contract.Source)
	}
	for name, value := range map[string]string{
		"distribution":    contract.Source.Distribution,
		"image":           contract.Source.Image,
		"image_id":        contract.Source.ImageID,
		"image_digest":    contract.Source.ImageDigest,
		"source_revision": contract.Source.SourceRevision,
		"kernel":          contract.Source.Kernel,
		"captured_on":     contract.Source.CapturedOn,
		"environment":     contract.Source.Environment,
	} {
		if strings.TrimSpace(value) == "" {
			t.Errorf("contract source %s is empty", name)
		}
	}
	for name, digest := range map[string]string{"image_id": contract.Source.ImageID, "image_digest": contract.Source.ImageDigest} {
		if len(digest) != len("sha256:")+64 || !strings.HasPrefix(digest, "sha256:") {
			t.Errorf("contract source %s = %q, want a complete sha256 digest", name, digest)
		}
	}
	if contract.Cgroup.Filesystem != "cgroup2fs" {
		t.Fatalf("cgroup filesystem = %q, want cgroup2fs", contract.Cgroup.Filesystem)
	}
	for _, controller := range []string{"cpu", "io", "memory"} {
		if !slices.Contains(contract.Cgroup.Controllers, controller) {
			t.Errorf("EL8 contract does not expose required controller %q", controller)
		}
	}

	unit := contract.Interfaces["org.freedesktop.systemd1.Unit"]
	assertObservedSignatures(t, unit.Properties, map[string]string{
		"Id": "s", "ActiveState": "s", "InvocationID": "ay",
		"FragmentPath": "s", "DropInPaths": "as",
	})

	sliceInterface := contract.Interfaces["org.freedesktop.systemd1.Slice"]
	assertObservedSignatures(t, sliceInterface.Properties, map[string]string{"ControlGroup": "s"})
	if !slices.Contains(sliceInterface.AbsentProperties, "ControlGroupId") {
		t.Fatal("EL8 contract does not explicitly record absent ControlGroupId")
	}
	if _, exists := sliceInterface.Properties["ControlGroupId"]; exists {
		t.Fatal("EL8 contract claims that ControlGroupId is both present and absent")
	}
	for property := range approvedScalarProperties {
		assertObservedSignatures(t, sliceInterface.Properties, map[string]string{string(property): "t"})
	}
	for property := range approvedDeviceProperties {
		assertObservedSignatures(t, sliceInterface.Properties, map[string]string{string(property): "a(st)"})
	}

	manager := contract.Interfaces["org.freedesktop.systemd1.Manager"]
	wantMethods := map[string]observedSystemdMethod{
		"ListUnitsByPatterns": {Input: "asas", Output: "a(ssssssouso)"},
		"SetUnitProperties":   {Input: "sba(sv)"},
		"StartTransientUnit":  {Input: "ssa(sv)a(sa(sv))", Output: "o"},
		"StopUnit":            {Input: "ss", Output: "o"},
		"RevertUnitFiles":     {Input: "as", Output: "a(sss)"},
		"Reload":              {},
	}
	for name, want := range wantMethods {
		if got, ok := manager.Methods[name]; !ok || got != want {
			t.Errorf("manager method %s = %+v, present=%t, want %+v", name, got, ok, want)
		}
	}
}

func TestEL8Systemd239FixtureMatchesArchivedRawCapture(t *testing.T) {
	contract := loadEL8Systemd239Contract(t)
	evidence := loadEL8Systemd239Evidence(t)
	environment := parseKeyValueEvidence(t, evidence["environment.txt"])

	assertEvidenceValue(t, environment, "capture_kind", contract.Source.CaptureKind)
	assertEvidenceValue(t, environment, "source_revision", contract.Source.SourceRevision)
	assertEvidenceValue(t, environment, "captured_on", contract.Source.CapturedOn)
	assertEvidenceValue(t, environment, "environment", contract.Source.Environment)
	assertEvidenceValue(t, environment, "image", contract.Source.Image)
	assertEvidenceValue(t, environment, "image_id", contract.Source.ImageID)
	assertEvidenceValue(t, environment, "image_digest", contract.Source.ImageDigest)
	if environment["capture_schema"] != strconv.Itoa(contract.SchemaVersion) {
		t.Errorf("capture schema = %q, want %d", environment["capture_schema"], contract.SchemaVersion)
	}

	osRelease := parseKeyValueEvidence(t, evidence["os-release.txt"])
	distribution, err := strconv.Unquote(osRelease["PRETTY_NAME"])
	if err != nil {
		t.Fatalf("decode PRETTY_NAME: %v", err)
	}
	if distribution != contract.Source.Distribution {
		t.Errorf("captured distribution = %q, fixture = %q", distribution, contract.Source.Distribution)
	}
	assertTrimmedEvidence(t, evidence["systemd-package.txt"], contract.Source.SystemdNEVRA)
	assertTrimmedEvidence(t, evidence["kernel.txt"], contract.Source.Kernel)
	assertTrimmedEvidence(t, evidence["result.txt"], "PASS")
	assertTrimmedEvidence(t, evidence["cgroup-filesystem.txt"], "type="+contract.Cgroup.Filesystem)
	if controllers := strings.Fields(string(evidence["cgroup-controllers.txt"])); !slices.Equal(controllers, contract.Cgroup.Controllers) {
		t.Errorf("captured controllers = %v, fixture = %v", controllers, contract.Cgroup.Controllers)
	}

	rawInterfaces := map[string]observedSystemdInterface{}
	for interfaceName, evidenceName := range map[string]string{
		"org.freedesktop.systemd1.Unit":    "unit-interface.txt",
		"org.freedesktop.systemd1.Slice":   "slice-interface.txt",
		"org.freedesktop.systemd1.Manager": "manager-interface.txt",
	} {
		rawInterfaces[interfaceName] = parseBusctlIntrospection(t, evidence[evidenceName])
	}
	for interfaceName, fixture := range contract.Interfaces {
		raw, present := rawInterfaces[interfaceName]
		if !present {
			t.Errorf("fixture interface %s has no archived introspection", interfaceName)
			continue
		}
		assertObservedSignatures(t, raw.Properties, fixture.Properties)
		for _, absent := range fixture.AbsentProperties {
			if signature, present := raw.Properties[absent]; present {
				t.Errorf("archived interface %s exposes fixture-absent property %s with signature %q", interfaceName, absent, signature)
			}
		}
		for name, want := range fixture.Methods {
			if got, present := raw.Methods[name]; !present || got != want {
				t.Errorf("archived interface %s method %s = %+v, present=%t, fixture=%+v", interfaceName, name, got, present, want)
			}
		}
	}
}

func TestEL8Systemd239FixtureReproducesTheControlGroupIDFailure(t *testing.T) {
	modern := newFakeUnitTransport(1000)
	if _, present := modern.units[parentUserSlice].slice["ControlGroupId"]; !present {
		t.Fatal("existing fake transport no longer demonstrates its modern-systemd assumption")
	}
	if _, err := mustTestAdapter(t, modern, &fakeKernelVerifier{}).Discover(context.Background()); err != nil {
		t.Fatalf("modern fake transport unexpectedly failed: %v", err)
	}

	el8 := newFakeUnitTransport(1000)
	for _, unit := range el8.units {
		delete(unit.slice, "ControlGroupId")
	}
	_, err := mustTestAdapter(t, el8, &fakeKernelVerifier{}).Discover(context.Background())
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Reason != ReasonMalformedReply || !strings.Contains(err.Error(), "ControlGroupId is absent") {
		t.Fatalf("EL8 fixture error = %v, want the field-reported missing-ControlGroupId failure", err)
	}
}

func loadEL8Systemd239Contract(t *testing.T) observedSystemdContract {
	t.Helper()
	data, err := os.ReadFile("testdata/el8-systemd239-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract observedSystemdContract
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&contract); err != nil {
		t.Fatalf("decode EL8 contract: %v", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("EL8 contract has trailing JSON: %v", err)
	}
	return contract
}

func loadEL8Systemd239Evidence(t *testing.T) map[string][]byte {
	t.Helper()
	manifestData, err := os.ReadFile(filepath.Join(el8Systemd239EvidenceDirectory, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	wantDigests := make(map[string]string, len(el8Systemd239EvidenceFiles))
	for lineNumber, line := range strings.Split(strings.TrimSpace(string(manifestData)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 || fields[1] != "./"+filepath.Base(fields[1]) {
			t.Fatalf("invalid SHA256SUMS line %d: %q", lineNumber+1, line)
		}
		name := strings.TrimPrefix(fields[1], "./")
		if _, duplicate := wantDigests[name]; duplicate {
			t.Fatalf("duplicate SHA256SUMS entry %q", name)
		}
		wantDigests[name] = fields[0]
	}

	entries, err := os.ReadDir(el8Systemd239EvidenceDirectory)
	if err != nil {
		t.Fatal(err)
	}
	actualNames := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("unexpected evidence directory %q", entry.Name())
		}
		actualNames = append(actualNames, entry.Name())
	}
	wantNames := append(slices.Clone(el8Systemd239EvidenceFiles), "SHA256SUMS")
	slices.Sort(actualNames)
	slices.Sort(wantNames)
	if !slices.Equal(actualNames, wantNames) {
		t.Fatalf("evidence inventory = %v, want %v", actualNames, wantNames)
	}

	evidence := make(map[string][]byte, len(el8Systemd239EvidenceFiles))
	for _, name := range el8Systemd239EvidenceFiles {
		data, err := os.ReadFile(filepath.Join(el8Systemd239EvidenceDirectory, name))
		if err != nil {
			t.Fatal(err)
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(data))
		if want, present := wantDigests[name]; !present || digest != want {
			t.Fatalf("evidence digest %s = %s, present=%t, want %s", name, digest, present, want)
		}
		evidence[name] = data
	}
	if len(wantDigests) != len(evidence) {
		t.Fatalf("manifest entries = %d, evidence files = %d", len(wantDigests), len(evidence))
	}
	return evidence
}

func parseBusctlIntrospection(t *testing.T, data []byte) observedSystemdInterface {
	t.Helper()
	observed := observedSystemdInterface{
		Properties: make(map[string]string),
		Methods:    make(map[string]observedSystemdMethod),
	}
	for lineNumber, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !strings.HasPrefix(fields[0], ".") {
			t.Fatalf("invalid busctl introspection line %d: %q", lineNumber+1, line)
		}
		name := strings.TrimPrefix(fields[0], ".")
		switch fields[1] {
		case "property":
			observed.Properties[name] = fields[2]
		case "method":
			if len(fields) < 4 {
				t.Fatalf("method line %d has no output signature: %q", lineNumber+1, line)
			}
			observed.Methods[name] = observedSystemdMethod{Input: signatureOrEmpty(fields[2]), Output: signatureOrEmpty(fields[3])}
		case "signal":
		default:
			t.Fatalf("unknown busctl member kind %q on line %d", fields[1], lineNumber+1)
		}
	}
	return observed
}

func parseKeyValueEvidence(t *testing.T, data []byte) map[string]string {
	t.Helper()
	values := make(map[string]string)
	for lineNumber, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, present := strings.Cut(line, "=")
		if !present || key == "" {
			t.Fatalf("invalid key-value evidence line %d: %q", lineNumber+1, line)
		}
		if _, duplicate := values[key]; duplicate {
			t.Fatalf("duplicate key-value evidence key %q", key)
		}
		values[key] = value
	}
	return values
}

func signatureOrEmpty(signature string) string {
	if signature == "-" {
		return ""
	}
	return signature
}

func assertEvidenceValue(t *testing.T, evidence map[string]string, name, want string) {
	t.Helper()
	if got, present := evidence[name]; !present || got != want {
		t.Errorf("evidence %s = %q, present=%t, want %q", name, got, present, want)
	}
}

func assertTrimmedEvidence(t *testing.T, data []byte, want string) {
	t.Helper()
	if got := strings.TrimSpace(string(data)); got != want {
		t.Errorf("evidence value = %q, want %q", got, want)
	}
}

func assertObservedSignatures(t *testing.T, got, want map[string]string) {
	t.Helper()
	for name, signature := range want {
		if observed, ok := got[name]; !ok || observed != signature {
			t.Errorf("D-Bus member %s signature = %q, present=%t, want %q", name, observed, ok, signature)
		}
	}
}
