//go:build !windows

package service

import "syscall"

// freeBytes reports free space on the filesystem holding path.
//
// Split by platform because statfs is a Unix call and the service is also
// built for a workstation, where `record` and `debug` run against the printer
// without a container.
func freeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	// Bsize is signed on some platforms and has no business being negative;
	// treating that as "unknown" beats multiplying it into nonsense.
	if st.Bsize < 0 {
		return 0, nil
	}
	return st.Bavail * uint64(st.Bsize), nil
}
