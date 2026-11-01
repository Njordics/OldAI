//go:build windows

package inventory

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows support exists only so the dev cluster on a Windows workstation
// reports real memory and disk numbers; Slaves run Linux.

func diskUsage(dir string) (total, free uint64, ok bool) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, false
	}
	var avail, tot, totFree uint64
	if windows.GetDiskFreeSpaceEx(p, &avail, &tot, &totFree) != nil {
		return 0, 0, false
	}
	return tot, avail, true
}

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

var procGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

func platformMemory() (total, avail uint64) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0, 0
	}
	return m.TotalPhys, m.AvailPhys
}
