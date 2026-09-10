package systemdunit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
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
		CaptureKind  string `json:"capture_kind"`
		Distribution string `json:"distribution"`
		Image        string `json:"image"`
		ImageID      string `json:"image_id"`
		ImageDigest  string `json:"image_digest"`
		SystemdNEVRA string `json:"systemd_nevra"`
		Kernel       string `json:"kernel"`
		CapturedOn   string `json:"captured_on"`
		Environment  string `json:"environment"`
	} `json:"source"`
	Cgroup struct {
		Filesystem  string   `json:"filesystem"`
		Controllers []string `json:"controllers"`
	} `json:"cgroup"`
	Interfaces map[string]observedSystemdInterface `json:"interfaces"`
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
		"distribution": contract.Source.Distribution,
		"image":        contract.Source.Image,
		"image_id":     contract.Source.ImageID,
		"image_digest": contract.Source.ImageDigest,
		"kernel":       contract.Source.Kernel,
		"captured_on":  contract.Source.CapturedOn,
		"environment":  contract.Source.Environment,
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

func assertObservedSignatures(t *testing.T, got, want map[string]string) {
	t.Helper()
	for name, signature := range want {
		if observed, ok := got[name]; !ok || observed != signature {
			t.Errorf("D-Bus member %s signature = %q, present=%t, want %q", name, observed, ok, signature)
		}
	}
}
