package watchdog

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func processStart(pid uint32) (string, error) {
	if pid == 0 {
		return "", nil
	}
	data, err := os.ReadFile("/proc/" + strconv.FormatUint(uint64(pid), 10) + "/stat")
	if absent(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// comm may contain spaces and parentheses. Fields after its last closing
	// parenthesis start at field 3; starttime is field 22.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", errRefused
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return "", errRefused
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", errRefused
	}
	return fields[19], nil
}

func emptyGroup(group string) error {
	if !validGroup(group) {
		return errRefused
	}
	if group == "" {
		return nil
	}
	var fs unix.Statfs_t
	if err := unix.Statfs("/sys/fs/cgroup", &fs); err != nil || fs.Type != unix.CGROUP2_SUPER_MAGIC {
		return errRefused
	}
	f, err := os.Open(filepath.Join("/sys/fs/cgroup", group, "cgroup.events"))
	if absent(err) {
		// In cgroup v2 a populated group cannot be removed.
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) > 4096 {
		return errRefused
	}
	return unpopulated(data)
}

func unpopulated(data []byte) error {
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return errRefused
		}
		if fields[0] == "populated" {
			if found || fields[1] != "0" {
				return errRefused
			}
			found = true
		}
	}
	if !found {
		return errRefused
	}
	return nil
}
