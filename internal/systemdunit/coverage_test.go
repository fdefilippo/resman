package systemdunit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProcCoverageInspectorClassifiesCompleteSplitAndRuntimeOwnedWorkloads(t *testing.T) {
	tests := []struct {
		name        string
		outside     bool
		foreignNS   bool
		runtimePath bool
		wantState   ResourceCoverageState
		wantReason  ResourceCoverageReason
	}{
		{name: "complete", wantState: ResourceCoverageComplete, wantReason: ResourceCoverageVerified},
		{name: "authority_split", outside: true, wantState: ResourceCoveragePartial, wantReason: ResourceCoverageAuthoritySplit},
		{name: "runtime_owned_descendant", foreignNS: true, wantState: ResourceCoverageRefused, wantReason: ResourceCoverageRuntimeDescendant},
		{name: "rootless_runtime_with_host_pid_namespace", runtimePath: true, wantState: ResourceCoverageRefused, wantReason: ResourceCoverageRuntimeDescendant},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			hostNS := filepath.Join(root, "host-pid-ns")
			if err := os.WriteFile(hostNS, []byte("host"), 0600); err != nil {
				t.Fatal(err)
			}
			makeProcFixture(t, root, "1", "/", hostNS)
			path := "/user.slice/user-1000.slice/session-1.scope"
			if test.outside {
				path = "/system.slice/foreign.service"
			}
			if test.runtimePath {
				path = "/user.slice/user-1000.slice/user@1000.service/user.slice/libpod-0123456789abcdef.scope/container"
			}
			processNS := hostNS
			if test.foreignNS {
				processNS = filepath.Join(root, "foreign-pid-ns")
				if err := os.WriteFile(processNS, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			makeProcFixture(t, root, "42", path, processNS)

			inspector := newProcCoverageInspector(root)
			inspector.ownerUID = func(info os.FileInfo) uint32 {
				if info.Name() == "42" {
					return 1000
				}
				return 0
			}
			got, err := inspector.inspect(context.Background(), 1000, "/user.slice/user-1000.slice")
			if err != nil {
				t.Fatalf("inspect() error: %v", err)
			}
			if got.State != test.wantState || got.Reason != test.wantReason {
				t.Fatalf("authority = %+v, want state=%s reason=%s", got, test.wantState, test.wantReason)
			}
		})
	}
}

func TestProcCoverageInspectorFailsClosedWhenHostNamespaceCannotBeRead(t *testing.T) {
	root := t.TempDir()
	inspector := newProcCoverageInspector(root)
	got, err := inspector.inspect(context.Background(), uint32(os.Getuid()), "/user.slice/user-1000.slice")
	if got.State != ResourceCoverageRefused || got.Reason != ResourceCoverageInspectionFailed || err == nil {
		t.Fatalf("authority=%+v error=%v, want refused inspection failure", got, err)
	}
}

func TestCheckResourceAuthorityRejectsMissingControllerBeforeMutation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "user.slice", "user-1000.slice")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	transport := newFakeUnitTransport(1000)
	verifier := newCgroupVerifier(root)
	adapter := mustTestAdapter(t, transport, verifier)
	adapter.coverage = staticCoverageInspector{authority: ResourceAuthority{State: ResourceCoverageComplete, Reason: ResourceCoverageVerified}}
	assignment := mustAssignment(t, PropertyMemoryHigh, 64<<20)
	snapshot, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := adapter.CheckResourceAuthority(context.Background(), snapshot.Users[0].Unit.Identity, 1000, ResourceMemory, []PropertyAssignment{assignment})
	var authorityErr *ResourceAuthorityError
	if !errors.As(err, &authorityErr) || got.Reason != ResourceCoverageControllerMissing {
		t.Fatalf("authority=%+v error=%v, want controller refusal", got, err)
	}
	if len(transport.setCalls) != 0 {
		t.Fatalf("controller refusal performed %d mutations", len(transport.setCalls))
	}
}

type staticCoverageInspector struct {
	authority ResourceAuthority
	err       error
}

func (i staticCoverageInspector) inspect(context.Context, uint32, string) (ResourceAuthority, error) {
	return i.authority, i.err
}

func makeProcFixture(t *testing.T, root, pid, cgroup, namespaceTarget string) {
	t.Helper()
	processRoot := filepath.Join(root, pid)
	if err := os.MkdirAll(filepath.Join(processRoot, "ns"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processRoot, "cgroup"), []byte("0::"+cgroup+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(namespaceTarget, filepath.Join(processRoot, "ns", "pid")); err != nil {
		t.Fatal(err)
	}
}
