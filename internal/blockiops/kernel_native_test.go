//go:build linux && resman_native

package blockiops

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/fdefilippo/resman/config"
)

// TestNativeKernelSource requires the explicitly provisioned disposable guest.
// It never runs in the ordinary unit suite or attaches to the developer host.
func TestNativeKernelSource(t *testing.T) {
	if os.Getenv("RESMAN_NATIVE_DISPOSABLE_GUEST") != "resman-82p" {
		t.Skip("disposable guest required")
	}
	command := func(t *testing.T) func(...string) string {
		return func(args ...string) string {
			t.Helper()
			out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %v: %s", args, err, out)
			}
			return string(out)
		}
	}
	run := command(t)
	slice := "resman82p-native.slice"
	run("systemd-run", "--quiet", "--unit=resman82p-native-keeper", "--slice="+slice, "--property=IOAccounting=yes", "/usr/bin/sleep", "300")
	defer exec.Command("systemctl", "stop", "resman82p-native-keeper.service", slice).Run()
	controlGroup := run("systemctl", "show", "-p", "ControlGroup", "--value", slice)
	controlGroup = strings.TrimSpace(controlGroup)
	raw := run("stat", "-c", "%i", "/sys/fs/cgroup"+controlGroup)
	var cgroup uint64
	if _, err := fmt.Sscan(raw, &cgroup); err != nil {
		t.Fatal(err)
	}
	var devices []Device
	for _, name := range []string{"vda", "dm-0"} {
		var device Device
		if _, err := fmt.Sscanf(run("cat", "/sys/block/"+name+"/dev"), "%d:%d", &device.Major, &device.Minor); err != nil {
			t.Fatal(err)
		}
		devices = append(devices, device)
	}
	for _, excluded := range []bool{false, true} {
		t.Run(strconv.FormatBool(excluded), func(t *testing.T) {
			run := command(t)
			cfg := config.DefaultConfig()
			cfg.ProcessExcludeList = []string{"^fio$"}
			if !excluded {
				cfg.ProcessExcludeList = nil
			}
			source, err := NewKernelSource(1, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			id, err := source.Register(1000, "native-test", cgroup)
			if err != nil {
				t.Fatal(err)
			}
			before, err := source.Read(id, devices)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
			run("systemd-run", "--quiet", "--wait", "--collect", "--unit=resman82p-native-fio", "--slice="+slice, "--property=IOAccounting=yes", "/usr/bin/fio", "--name=direct", "--filename=/var/tmp/resman82p-native-data", "--rw=write", "--bs=4k", "--size=256k", "--direct=1", "--ioengine=sync", "--output=/var/tmp/resman82p-fio-result")
			time.Sleep(100 * time.Millisecond)
			after, err := source.Read(id, devices)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("cgroup=%d io.stat=%s", cgroup, run("cat", "/sys/fs/cgroup"+controlGroup+"/io.stat"))
			for _, d := range devices {
				interval, err := Between(before, after, d)
				if err != nil {
					t.Fatal(err)
				}
				counters := after.Devices[d].Write
				t.Logf("device=%v selected=%d excluded=%d unresolved=%d bounds=%+v", d, counters.Selected, counters.Excluded, counters.Unresolved, interval.Write())
				if excluded {
					if counters.Excluded != 64 || counters.Selected != 0 {
						t.Fatalf("excluded direct operations: %+v", counters)
					}
				} else if counters.Selected < 64 {
					t.Fatalf("selected direct operations: %+v", counters)
				}
			}

			if excluded {
				before, err = source.Read(id, devices)
				if err != nil {
					t.Fatal(err)
				}
				script := `import ctypes,os,mmap;ctypes.CDLL(None).prctl(15,b"fio",0,0,0);b=mmap.mmap(-1,4096);f=os.open("/var/tmp/resman82p-spoof-data",os.O_CREAT|os.O_WRONLY|os.O_DIRECT,0o600);os.write(f,b);os.close(f)`
				run("systemd-run", "--quiet", "--wait", "--collect", "--unit=resman82p-spoof", "--slice="+slice, "/usr/bin/python3", "-c", script)
				after, err = source.Read(id, devices)
				if err != nil {
					t.Fatal(err)
				}
				for _, d := range devices {
					if after.Devices[d].Write.Selected <= before.Devices[d].Write.Selected {
						t.Fatal("comm spoof changed trusted executable policy")
					}
				}
				before, err = source.Read(id, devices)
				if err != nil {
					t.Fatal(err)
				}
				run("systemd-run", "--quiet", "--wait", "--collect", "--unit=resman82p-buffered", "--slice="+slice, "/usr/bin/python3", "-c", `import os;p="/var/tmp/resman82p-shared";os.unlink(p) if os.path.exists(p) else None;open(p,"wb").write(b"x"*4096)`)
				run("systemd-run", "--quiet", "--wait", "--collect", "--unit=resman82p-sync", "/usr/bin/sync", "-f", "/var/tmp/resman82p-shared")
				time.Sleep(100 * time.Millisecond)
				after, err = source.Read(id, devices)
				if err != nil {
					t.Fatal(err)
				}
				unknown := uint64(0)
				for _, d := range devices {
					unknown += after.Devices[d].Write.Unresolved - before.Devices[d].Write.Unresolved
				}
				if unknown == 0 {
					t.Fatal("delayed buffered writeback was falsely attributed")
				}
				t.Logf("buffered unresolved operations across layers=%d", unknown)
			}
			if excluded {
				// Both selected and excluded writers dirty the same filesystem
				// block. Writeback has no certified direct-submit provenance.
				run("systemd-run", "--quiet", "--wait", "--collect", "--unit=resman82p-mixed-prepare", "--slice="+slice, "/usr/bin/python3", "-c", `import os;p="/var/tmp/resman82p-mixed";os.unlink(p) if os.path.exists(p) else None;open(p,"wb").write(b"x"*4096)`)
				run("systemd-run", "--quiet", "--wait", "--collect", "--unit=resman82p-mixed-sync", "/usr/bin/sync", "-f", "/var/tmp/resman82p-mixed")
				before, err = source.Read(id, devices)
				if err != nil {
					t.Fatal(err)
				}
				run("systemd-run", "--quiet", "--wait", "--collect", "--unit=resman82p-mixed-selected", "--slice="+slice, "/usr/bin/python3", "-c", `f=open("/var/tmp/resman82p-mixed", "r+b");f.write(b"s"*512);f.close()`)
				run("systemd-run", "--quiet", "--wait", "--collect", "--unit=resman82p-mixed-excluded", "--slice="+slice, "/usr/bin/fio", "--name=mixed", "--filename=/var/tmp/resman82p-mixed", "--rw=write", "--offset=512", "--bs=512", "--size=512", "--direct=0", "--ioengine=sync", "--invalidate=0", "--end_fsync=0", "--fsync_on_close=0", "--output=/var/tmp/resman82p-mixed-result")
				run("systemd-run", "--quiet", "--wait", "--collect", "--unit=resman82p-mixed-sync", "/usr/bin/sync", "-f", "/var/tmp/resman82p-mixed")
				after, err = source.Read(id, devices)
				if err != nil {
					t.Fatal(err)
				}
				for _, d := range devices {
					interval, err := Between(before, after, d)
					if err != nil || interval.Write().ActivationMinimum != 0 || interval.Write().ReleaseMaximum <= 0 || !interval.HasUnresolvedOperations() {
						t.Fatalf("mixed buffered writers lost conservative coverage: device=%v interval=%+v error=%v", d, interval, err)
					}
					t.Logf("mixed buffered writers device=%v write=%+v", d, interval.Write())
				}
			}

			if err := source.Retain(nil); err != nil {
				t.Fatal(err)
			}
			renewed, err := source.Register(1000, "native-test", cgroup)
			if err != nil || renewed == id {
				t.Fatalf("watch renewal retained the retired identity: %v %v", renewed, err)
			}
			if _, err := source.Read(id, devices); err == nil {
				t.Fatal("retired watch identity remained readable")
			}

		})
	}
}

func TestNativeIntegrityFailuresAreSticky(t *testing.T) {
	if os.Getenv("RESMAN_NATIVE_DISPOSABLE_GUEST") != "resman-82p" {
		t.Skip("disposable guest required")
	}
	for _, kind := range []string{"map_loss", "read_failure", "detached"} {
		t.Run(kind, func(t *testing.T) {
			source, err := NewKernelSource(1, config.DefaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			if kind == "detached" {
				if err := source.links[len(source.links)-1].Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				key := uint32(3)
				if kind == "read_failure" {
					key = 1
				}
				if err := source.collection.Maps["health"].Update(key, uint64(1), ebpf.UpdateAny); err != nil {
					t.Fatal(err)
				}
			}
			if err := source.Health(); err == nil {
				t.Fatal("source loss was accepted as complete coverage")
			} else {
				t.Log(err)
			}
			if kind != "detached" {
				for key := uint32(1); key < 5; key++ {
					if err := source.collection.Maps["health"].Update(key, uint64(0), ebpf.UpdateAny); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := source.Health(); err == nil {
				t.Fatal("damaged source silently became healthy")
			}
		})
	}
}
