package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

const fcCmdline = "init=/sbin/init root=/dev/vda ro rootfstype=erofs console=ttyS0 reboot=k panic=-1 pci=off sandbox.backend=firecracker sandbox.disk.offset=1"

func TestParseBootCmdline(t *testing.T) {
	for _, tt := range []struct {
		name, cmdline string
		want          bootConfig
	}{
		{"CH default", "", bootConfig{}},
		{"CH pmem", "root=/dev/pmem0 ro rootfstype=erofs rootflags=dax=always console=hvc0", bootConfig{}},
		{"CH explicit", "sandbox.backend=cloud-hypervisor sandbox.disk.offset=0 sandbox.root.layout=overlay", bootConfig{}},
		{"CH single", "sandbox.root.layout=single", bootConfig{singleRoot: true}},
		{"FC overlay", fcCmdline, bootConfig{backend: firecrackerBackend, diskOffset: 1}},
		{"FC single", fcCmdline + " sandbox.root.layout=single keep_bootcon", bootConfig{backend: firecrackerBackend, diskOffset: 1, singleRoot: true}},
		{"init arguments ignored", "-- sandbox.backend=invalid", bootConfig{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBootCmdline(tt.cmdline)
			if err != nil || got != tt.want {
				t.Fatalf("parseBootCmdline() = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestParseBootCmdlineRejectsInvalid(t *testing.T) {
	for _, cmdline := range []string{
		"sandbox.backend", "sandbox.backend=", "sandbox.backend=qemu",
		"sandbox.disk.offset=", "sandbox.disk.offset=-1", "sandbox.disk.offset=1", "sandbox.disk.offset=01",
		"sandbox.root.layout=unknown", "sandbox.root.layout=", "sandbox.root.layout",
		"sandbox.backend=cloud-hypervisor sandbox.backend=cloud-hypervisor",
		"sandbox.root.layout=single sandbox.root.layout=overlay",
		fcCmdline + " sandbox.backend=cloud-hypervisor",
		fcCmdline + " sandbox.disk.offset=0",
		strings.Replace(fcCmdline, "sandbox.disk.offset=1", "", 1),
		strings.Replace(fcCmdline, "sandbox.disk.offset=1", "sandbox.disk.offset=0", 1),
		strings.Replace(fcCmdline, "sandbox.disk.offset=1", "sandbox.disk.offset=2", 1),
		strings.Replace(fcCmdline, "root=/dev/vda", "root=/dev/pmem0", 1),
		strings.Replace(fcCmdline, "rootfstype=erofs", "rootfstype=ext4", 1),
		strings.Replace(fcCmdline, " ro ", " rw ", 1),
		fcCmdline + " rw", fcCmdline + " rootflags=dax=always",
	} {
		t.Run(cmdline, func(t *testing.T) {
			if _, err := parseBootCmdline(cmdline); err == nil {
				t.Fatal("accepted invalid boot configuration")
			}
		})
	}
}

func TestBootDeviceOrderAndReadyPhase(t *testing.T) {
	for _, tt := range []struct {
		cfg                bootConfig
		base, upper, phase string
	}{
		{bootConfig{}, "/dev/vda", "/dev/vdb", "runtime_ready"},
		{bootConfig{singleRoot: true}, "/dev/vda", "/dev/vdb", "runtime_ready"},
		{bootConfig{backend: firecrackerBackend, diskOffset: 1}, "/dev/vdb", "/dev/vdc", "runtime_ready_firecracker_v1"},
		{bootConfig{backend: firecrackerBackend, diskOffset: 1, singleRoot: true}, "/dev/vdb", "/dev/vdc", "runtime_ready_firecracker_v1"},
	} {
		base, upper := tt.cfg.rootDevices()
		if base != tt.base || upper != tt.upper || tt.cfg.readyPhase() != tt.phase {
			t.Fatalf("unexpected boot mapping: %s/%s, %s", base, upper, tt.cfg.readyPhase())
		}
	}
}

func TestFirecrackerLaunchDiskPaths(t *testing.T) {
	for _, single := range []bool{false, true} {
		cfg := bootConfig{backend: firecrackerBackend, diskOffset: 1, singleRoot: single}
		first := "/dev/vdd"
		if single {
			first = "/dev/vdc"
		}
		m := proto.MountSpec{Type: "disk", DiskDevs: []string{first}, Target: "/data"}
		spec := &proto.LaunchSpec{Mounts: []proto.MountSpec{m, m}}
		if err := cfg.validateLaunchDisks(spec); err != nil {
			t.Fatal(err)
		}
		if spec.Mounts[0].DiskDevs[0] != first {
			t.Fatal("host-resolved data path was shifted again")
		}
		for _, dev := range []string{"/dev/vda", "/dev/vdb", "/dev/vdz1", "/dev/vdaa", "/dev/../vdd", "vdd", "/dev/sda"} {
			bad := m
			bad.DiskDevs = []string{dev}
			if err := cfg.validateLaunchDisks(&proto.LaunchSpec{Mounts: []proto.MountSpec{bad}}); err == nil {
				t.Fatalf("accepted reserved/invalid device %s", dev)
			}
		}
		if !single {
			bad := m
			bad.DiskDevs = []string{"/dev/vdc"}
			if err := cfg.validateLaunchDisks(&proto.LaunchSpec{Mounts: []proto.MountSpec{bad}}); err == nil {
				t.Fatal("accepted overlay root upper as data disk")
			}
		}
		for _, bad := range []proto.MountSpec{
			{Type: "disk", DiskSlot: true, DiskDevs: []string{first}},
			{Type: "disk", DiskSlotDeclared: true, DiskDevs: []string{first}},
			{Type: "disk", DiskOverlay: true, DiskDevs: []string{first}},
			{Type: "disk", DiskOverlay: true, DiskDevs: []string{first, first}},
			{Type: "disk", DiskIndex: 8, DiskDevs: []string{first}},
		} {
			if err := cfg.validateLaunchDisks(&proto.LaunchSpec{Mounts: []proto.MountSpec{bad}}); err == nil {
				t.Fatalf("accepted invalid topology: %+v", bad)
			}
		}
		other := m
		other.DiskIndex = 1
		if err := cfg.validateLaunchDisks(&proto.LaunchSpec{Mounts: []proto.MountSpec{m, other}}); err == nil {
			t.Fatal("accepted a device shared by distinct disks")
		}
		overlay := m
		overlay.DiskOverlay = true
		overlay.DiskDevs = []string{first, "/dev/vde"}
		if err := cfg.validateLaunchDisks(&proto.LaunchSpec{Mounts: []proto.MountSpec{overlay}}); err != nil {
			t.Fatal(err)
		}
	}
	// CH's existing topology/projection validator remains authoritative.
	if err := (bootConfig{}).validateLaunchDisks(&proto.LaunchSpec{Mounts: []proto.MountSpec{{Type: "disk", DiskSlot: true}}}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkloadMountBoundary(t *testing.T) {
	sentinel := errors.New("injected failure")
	for _, backend := range []string{"", firecrackerBackend} {
		for _, fail := range []string{"", "wait", "flush", "mount"} {
			t.Run(backend+"/"+fail, func(t *testing.T) {
				var events []string
				step := func(name, dev string) error {
					if dev != "/dev/vdb" {
						t.Fatalf("wrong device %s", dev)
					}
					events = append(events, name)
					if name == fail {
						return sentinel
					}
					return nil
				}
				cfg := bootConfig{backend: backend, diskOffset: 1}
				err := cfg.mountBlock("/dev/vdb", "/overlay/lower", "erofs", unix.MS_RDONLY, "",
					func(dev string) error { return step("wait", dev) },
					func(dev string) error { return step("flush", dev) },
					func(dev, target, fs string, flags uintptr, opts string) error {
						if target != "/overlay/lower" || fs != "erofs" || flags != unix.MS_RDONLY || opts != "" {
							t.Fatal("mount arguments changed")
						}
						return step("mount", dev)
					})
				want := []string{"wait"}
				if fail != "wait" {
					if backend == firecrackerBackend {
						want = append(want, "flush")
					}
					if backend != firecrackerBackend || fail != "flush" {
						want = append(want, "mount")
					}
				}
				wantError := fail != "" && (backend == firecrackerBackend || fail != "flush")
				if !slices.Equal(events, want) || errors.Is(err, sentinel) != wantError {
					t.Fatalf("events=%v error=%v; want %v error=%v", events, err, want, wantError)
				}
			})
		}
	}
}

func TestFirecrackerMountNeverTouchesRuntimeDisk(t *testing.T) {
	cfg := bootConfig{backend: firecrackerBackend, diskOffset: 1}
	untouched := func(string) error { t.Fatal("touched reserved device"); return nil }
	err := cfg.mountBlock("/dev/vda", "/sysroot", "ext4", 0, "", untouched, untouched,
		func(string, string, string, uintptr, string) error { t.Fatal("mounted runtime disk"); return nil })
	if err == nil {
		t.Fatal("accepted runtime disk")
	}
}

func TestFirecrackerFlushesEachWorkloadMount(t *testing.T) {
	for _, single := range []bool{false, true} {
		cfg := bootConfig{backend: firecrackerBackend, diskOffset: 1, singleRoot: single}
		base, upper := cfg.rootDevices()
		devices := []string{base}
		if !single {
			devices = append(devices, upper)
		}
		// These data paths are host-resolved, not logical disk ordinals.
		devices = append(devices, "/dev/vdd", "/dev/vde", "/dev/vdf")
		var events, want []string
		for _, dev := range devices {
			fs, flags, opts := "ext4", uintptr(0), "nodiscard"
			if !single && dev == base || dev == "/dev/vdd" {
				fs, flags, opts = "erofs", unix.MS_RDONLY, ""
			}
			err := cfg.mountBlock(dev, "/staging", fs, flags, opts,
				func(path string) error { events = append(events, "wait "+path); return nil },
				func(path string) error { events = append(events, "flush "+path); return nil },
				func(path, target, fstype string, mountFlags uintptr, options string) error {
					if fstype != fs || mountFlags != flags || options != opts {
						t.Fatal("filesystem/discard policy changed")
					}
					events = append(events, "mount "+path)
					return nil
				})
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, "wait "+dev, "flush "+dev, "mount "+dev)
		}
		if !slices.Equal(events, want) {
			t.Fatalf("mount boundary events=%v; want %v", events, want)
		}
	}
}

func TestInvalidateBlockCacheRejectsRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-block-device")
	if err := os.WriteFile(path, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := invalidateBlockCache(path); err == nil || !strings.Contains(err.Error(), "not a block device") {
		t.Fatalf("unexpected invalidation error: %v", err)
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "unchanged" {
		t.Fatalf("regular file changed: %q, %v", content, err)
	}
}

func TestFirecrackerKernelFragmentRequirements(t *testing.T) {
	for file, required := range map[string][]string{
		"sandbox-common.config": {"CONFIG_VIRTIO_MMIO=y", "CONFIG_VIRTIO_BLK=y", "CONFIG_VIRTIO_NET=y", "CONFIG_VIRTIO_VSOCKETS=y", "CONFIG_EROFS_FS=y", "CONFIG_SERIAL_8250=y", "CONFIG_SERIAL_8250_CONSOLE=y"},
		"sandbox-arm64.config":  {"CONFIG_OF=y", "CONFIG_SERIAL_OF_PLATFORM=y", "CONFIG_ARM_GIC_V3=y", "CONFIG_ARM_ARCH_TIMER=y", "CONFIG_SERIAL_AMBA_PL011=y", "CONFIG_SERIAL_AMBA_PL011_CONSOLE=y"},
		"sandbox-x86_64.config": {"CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES=y", "CONFIG_PVH=y"},
	} {
		content, err := os.ReadFile(filepath.Join("../../native-deps/deps/vmlinux", file))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(content), "\n")
		for _, option := range required {
			if !slices.Contains(lines, option) {
				t.Errorf("%s is missing %s", file, option)
			}
		}
	}
}

