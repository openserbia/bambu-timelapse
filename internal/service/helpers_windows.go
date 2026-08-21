//go:build windows

package service

import "golang.org/x/sys/windows"

// freeBytes reports free space on the volume holding path.
//
// GetDiskFreeSpaceEx rather than statfs: its first out-parameter is the space
// available to the calling user, which is what a quota-bearing volume makes
// different from the volume's own free space, and it is the number a capture
// about to write frames actually has.
func freeBytes(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var availableToCaller uint64
	if err := windows.GetDiskFreeSpaceEx(p, &availableToCaller, nil, nil); err != nil {
		return 0, err
	}
	return availableToCaller, nil
}
