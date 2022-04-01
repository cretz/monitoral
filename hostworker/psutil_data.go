package hostworker

import (
	"reflect"
	"runtime"
	"strings"

	pscpu "github.com/shirou/gopsutil/v3/cpu"
	psdisk "github.com/shirou/gopsutil/v3/disk"
	pshost "github.com/shirou/gopsutil/v3/host"
	psload "github.com/shirou/gopsutil/v3/load"
	psmem "github.com/shirou/gopsutil/v3/mem"
	psnet "github.com/shirou/gopsutil/v3/net"
	psprocess "github.com/shirou/gopsutil/v3/process"
)

var psutilData = map[string]map[string]interface{}{}

func init() {
	addPsutilExports(
		"Cpu",
		pscpu.Counts,
		pscpu.Info,
		pscpu.Percent,
		pscpu.Times,
	)
	addPsutilExports(
		"Disk",
		psdisk.IOCounters,
		psdisk.Label,
		psdisk.Partitions,
		psdisk.SerialNumber,
		psdisk.Usage,
	)
	addPsutilExports(
		"Host",
		pshost.BootTime,
		pshost.HostID,
		pshost.Info,
		pshost.KernelArch,
		pshost.KernelVersion,
		pshost.KernelVersion,
		pshost.PlatformInformation,
		pshost.SensorsTemperatures,
		pshost.Uptime,
		pshost.Users,
		pshost.Virtualization,
	)
	addPsutilExports(
		"Load",
		psload.Avg,
		psload.Misc,
	)
	addPsutilExports(
		"Mem",
		psmem.SwapDevices,
		psmem.SwapMemory,
		psmem.VirtualMemory,
	)
	addPsutilExports(
		"Net",
		psnet.Connections,
		psnet.ConnectionsMax,
		psnet.ConnectionsPid,
		psnet.ConnectionsPidMaxWithoutUids,
		psnet.ConntrackStats,
		psnet.FilterCounters,
		psnet.Interfaces,
		psnet.IOCounters,
		psnet.IOCountersByFile,
		psnet.ProtoCounters,
	)
	addPsutilExports(
		"Process",
		psprocess.PidExists,
		psprocess.Pids,
		psprocess.Processes,
	)
}

func addPsutilExports(n string, v ...interface{}) {
	m := psutilData[n]
	if m == nil {
		m = map[string]interface{}{}
		psutilData[n] = m
	}
	for _, item := range v {
		m[getFunctionName(item)] = item
	}
}

func getFunctionName(i interface{}) string {
	name := runtime.FuncForPC(reflect.ValueOf(i).Pointer()).Name()
	lastDot := strings.LastIndex(name, ".")
	return strings.TrimSuffix(name[lastDot+1:], "-fm")
}
