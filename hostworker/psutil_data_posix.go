//go:build linux || freebsd || openbsd || darwin || solaris
// +build linux freebsd openbsd darwin solaris

package hostworker

import psmem "github.com/shirou/gopsutil/v3/mem"

func addPlatformPsutil() {
	addPsutilExports(
		"Mem",
		psmem.VirtualMemoryEx,
	)
}
