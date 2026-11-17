package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"golang.org/x/sys/unix"
)

const firecrackerBackend = "firecracker"

// Fixed before hello; the zero value preserves CH's pmem root and disk order.
type bootConfig struct {
	backend    string
	diskOffset int
	singleRoot bool
}

var guestBoot bootConfig

func parseBootCmdline(cmdline string) (bootConfig, error) {
	var cfg bootConfig
	values := make(map[string]string)
	readOnly, readWrite := false, false
	for _, word := range strings.Fields(cmdline) {
		if word == "--" {
			break // Remaining words are init arguments, not kernel parameters.
		}
		readOnly = readOnly || word == "ro"
		readWrite = readWrite || word == "rw"
		key, value, assigned := strings.Cut(word, "=")
		switch key {
		case "sandbox.backend", "sandbox.disk.offset", "sandbox.root.layout":
			if !assigned || value == "" {
				return cfg, fmt.Errorf("missing value for %s", key)
			}
			if _, exists := values[key]; exists {
				return cfg, fmt.Errorf("duplicate kernel parameter %s", key)
			}
		case "root", "rootfstype", "rootflags":
			// Last assignment follows Linux boot-root parameter semantics.
		default:
			continue
		}
		values[key] = value
	}
	switch values["sandbox.backend"] {
	case "", "cloud-hypervisor":
	case firecrackerBackend:
		cfg.backend = firecrackerBackend
	default:
		return cfg, fmt.Errorf("unsupported sandbox.backend %q", values["sandbox.backend"])
	}
	switch values["sandbox.root.layout"] {
	case "", "overlay":
	case "single":
		cfg.singleRoot = true
	default:
		return cfg, fmt.Errorf("unsupported sandbox.root.layout %q", values["sandbox.root.layout"])
	}
	if cfg.backend == firecrackerBackend {
		if values["sandbox.disk.offset"] != "1" {
			return cfg, errors.New("firecracker requires sandbox.disk.offset=1")
		}
		if values["root"] != "/dev/vda" || values["rootfstype"] != "erofs" || !readOnly || readWrite || values["rootflags"] != "" {
			return cfg, errors.New("firecracker requires root=/dev/vda ro rootfstype=erofs without rootflags")
		}
		cfg.diskOffset = 1
	} else if offset := values["sandbox.disk.offset"]; offset != "" && offset != "0" {
		return cfg, errors.New("cloud-hypervisor requires sandbox.disk.offset=0 (or absent)")
	}
	return cfg, nil
}

func (cfg bootConfig) readyPhase() string {
	if cfg.backend == firecrackerBackend {
		return "runtime_ready_firecracker_v1"
	}
	return "runtime_ready"
}

func (cfg bootConfig) rootDevices() (string, string) {
	return fmt.Sprintf("/dev/vd%c", 'a'+cfg.diskOffset), fmt.Sprintf("/dev/vd%c", 'b'+cfg.diskOffset)
}

// DiskDevs are already shifted by sandboxer. Never apply the offset twice.
// Reject runtime/root aliases before touching any workload filesystem.
func (cfg bootConfig) validateLaunchDisks(spec *proto.LaunchSpec) error {
	if cfg.backend != firecrackerBackend {
		return nil
	}
	firstData := byte('d') // runtime=vda, overlay root=vdb/vdc
	if cfg.singleRoot {
		firstData = 'c'
	}
	owners := make(map[string]int)
	for _, m := range spec.Mounts {
		if m.Type != "disk" {
			continue
		}
		want := 1
		if m.DiskOverlay {
			want = 2
		}
		if m.DiskSlot || m.DiskSlotDeclared || m.DiskIndex < 0 || m.DiskIndex >= 8 || len(m.DiskDevs) != want {
			return fmt.Errorf("firecracker: invalid disk %d topology or unsupported slot", m.DiskIndex)
		}
		for _, dev := range m.DiskDevs {
			if len(dev) != len("/dev/vda") || !strings.HasPrefix(dev, "/dev/vd") || dev[len(dev)-1] < firstData || dev[len(dev)-1] > 'z' {
				return fmt.Errorf("firecracker: invalid data disk device %q", dev)
			}
			if owner, exists := owners[dev]; exists && owner != m.DiskIndex {
				return fmt.Errorf("firecracker: device %s shared by disks %d and %d", dev, owner, m.DiskIndex)
			}
			owners[dev] = m.DiskIndex
		}
		if want == 2 && m.DiskDevs[0] == m.DiskDevs[1] {
			return fmt.Errorf("firecracker: disk %d lower and upper alias", m.DiskIndex)
		}
	}
	return nil
}

// BLKFLSBUF drops cached zero-filled partition/superblock probes from VM boot.
// Open read-only so EROFS base devices work too. No sync-only fallback is safe.
func invalidateBlockCache(dev string) error {
	fd, err := unix.Open(dev, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	var st unix.Stat_t
	err = unix.Fstat(fd, &st)
	if err == nil && st.Mode&unix.S_IFMT != unix.S_IFBLK {
		err = fmt.Errorf("%s is not a block device", dev)
	}
	if err == nil {
		err = unix.IoctlSetInt(fd, unix.BLKFLSBUF, 0)
	}
	return errors.Join(err, unix.Close(fd))
}

func mountWorkloadBlock(dev, target, fstype string, flags uintptr, options string) error {
	return guestBoot.mountBlock(dev, target, fstype, flags, options,
		func(dev string) error { return waitForDevice(dev, devicePollTimeout) }, invalidateBlockCache, unix.Mount)
}

// Dependencies are explicit so unprivileged tests can prove wait/flush/mount
// ordering and fail-closed behavior without opening or mounting real disks.
func (cfg bootConfig) mountBlock(dev, target, fstype string, flags uintptr, options string,
	wait, invalidate func(string) error, mount func(string, string, string, uintptr, string) error) error {
	if cfg.backend == firecrackerBackend && (dev == "/dev/vda" || len(dev) != len("/dev/vda") || !strings.HasPrefix(dev, "/dev/vd") || dev[len(dev)-1] < 'b' || dev[len(dev)-1] > 'z') {
		return fmt.Errorf("firecracker: invalid workload device %q", dev)
	}
	if err := wait(dev); err != nil {
		return fmt.Errorf("wait %s: %w", dev, err)
	}
	if cfg.backend == firecrackerBackend {
		if err := invalidate(dev); err != nil {
			return fmt.Errorf("BLKFLSBUF %s: %w", dev, err)
		}
	}
	return mount(dev, target, fstype, flags, options)
}
