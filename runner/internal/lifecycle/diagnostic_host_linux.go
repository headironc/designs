package lifecycle

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func (h hostFiles) diagnose(ctx context.Context, r receipt) hostObservation {
	one := h.hostSnapshot(ctx, r)
	two := h.hostSnapshot(ctx, r)
	if one != two {
		return hostObservation{unknown("host_changed"), unknown("host_changed"), unknown("host_changed")}
	}
	return one
}

func (h hostFiles) hostSnapshot(ctx context.Context, r receipt) hostObservation {
	out := hostObservation{unknown("boot_unavailable"), unknown("original_process_unavailable"), unknown("original_cgroup_unavailable")}
	if ctx.Err() != nil {
		return out
	}
	root, err := openPath(h.proc, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return out
	}
	defer root.Close()
	if h.realFS {
		var fs unix.Statfs_t
		if unix.Fstatfs(int(root.Fd()), &fs) != nil || fs.Type != unix.PROC_SUPER_MAGIC {
			return out
		}
	}
	f, err := openPath(filepath.Join(h.proc, "sys/kernel/random/boot_id"), unix.O_RDONLY, 0)
	if err != nil {
		return out
	}
	boot, err := io.ReadAll(io.LimitReader(f, 129))
	f.Close()
	id := strings.TrimSpace(string(boot))
	if err != nil || len(boot) > 128 || !uuidPattern.MatchString(id) {
		return out
	}
	if id != r.Observed.Host.BootID {
		out.Boot = resource("mismatch", 1)
		out.Process = unknown("original_boot_changed")
		out.Cgroup = unknown("original_boot_changed")
		return out
	}
	out.Boot = resource("matched", 1)
	original := r.Observed.Host
	dir, err := openPath(filepath.Join(h.proc, strconv.FormatInt(original.PID, 10)), unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if os.IsNotExist(err) {
		out.Process = resource("absent_verified", 0)
	} else if err == nil {
		raw, readErr := readAt(dir, "stat", 8192)
		dir.Close()
		if readErr == nil {
			start, startErr := startTime(raw, original.PID)
			if startErr == nil && start != original.StartTime {
				out.Process = resourceObservation{State: "mismatch", Code: "pid_reused", Count: 1}
			} else if startErr == nil {
				p, observeErr := h.observe(original.PID, r.Observed.Guard.UUID)
				if observeErr == nil {
					if p == original {
						out.Process = resource("present", 1)
					} else {
						out.Process = resource("mismatch", 1)
					}
				}
			}
		}
	}
	group, err := openPath(filepath.Join(h.cgroup, original.Cgroup), unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if os.IsNotExist(err) {
		out.Cgroup = resource("absent_verified", 0)
	} else if err == nil {
		defer group.Close()
		var st unix.Stat_t
		var fs unix.Statfs_t
		if unix.Fstat(int(group.Fd()), &st) == nil && unix.Fstatfs(int(group.Fd()), &fs) == nil && (!h.realFS || fs.Type == unix.CGROUP2_SUPER_MAGIC) {
			if st.Dev != original.CgroupDevice || st.Ino != original.CgroupInode {
				out.Cgroup = resourceObservation{State: "mismatch", Code: "cgroup_reused", Count: 1}
			} else {
				raw, err := readAt(group, "cgroup.events", 4096)
				if err == nil {
					seen := false
					valid := true
					populated := ""
					for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
						fields := strings.Fields(line)
						if len(fields) != 2 {
							valid = false
							break
						}
						if fields[0] == "populated" {
							if seen || fields[1] != "0" && fields[1] != "1" {
								valid = false
							}
							seen = true
							populated = fields[1]
						}
					}
					if valid && seen && samePath(group, filepath.Join(h.cgroup, original.Cgroup)) {
						if populated == "0" {
							out.Cgroup = resource("empty_observed", 0)
						} else {
							out.Cgroup = resource("residual", 1)
						}
					}
				}
			}
		}
	}
	return out
}
