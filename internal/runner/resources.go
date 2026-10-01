package runner

import (
	"syscall"
	"unsafe"
)

// 资源闸门用的原生调用（纯标准库，不引第三方）：
// 排班不再写死"8 个槽"，而是看主机真实占用——太挤就停止入队，等收工后再放。

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMem      = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes = kernel32.NewProc("GetSystemTimes")
)

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

// FreeRAMMB 当前可用物理内存（MB）。
func FreeRAMMB() uint64 {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, _ := procGlobalMem.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0
	}
	return m.AvailPhys / 1024 / 1024
}

type filetime struct{ Low, High uint32 }

func ftToUint64(f filetime) uint64 { return uint64(f.High)<<32 | uint64(f.Low) }

// CPUPercent 两次采样之间的系统 CPU 占用（%）。首次调用返回 0（还没差值）。
func (r *Runner) CPUPercent() float64 {
	var idle, kern, user filetime
	procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kern)), uintptr(unsafe.Pointer(&user)))
	ti, tk, tu := ftToUint64(idle), ftToUint64(kern), ftToUint64(user)
	if !r.cpuValid {
		r.cpuIdle, r.cpuKern, r.cpuUser, r.cpuValid = ti, tk, tu, true
		return 0
	}
	di, dk, du := ti-r.cpuIdle, tk-r.cpuKern, tu-r.cpuUser
	r.cpuIdle, r.cpuKern, r.cpuUser = ti, tk, tu
	total := dk + du // kernel 时间已含 idle
	if total == 0 {
		return 0
	}
	return 100 * float64(total-di) / float64(total)
}

// Gated 资源是否紧张到该停止入队。
func (r *Runner) Gated(freeMB uint64, cpuPct float64) (bool, string) {
	if r.cfg.MinFreeRamMB > 0 && freeMB < uint64(r.cfg.MinFreeRamMB) {
		return true, "内存空闲偏低"
	}
	if r.cfg.MaxCpuPct > 0 && cpuPct > float64(r.cfg.MaxCpuPct) {
		return true, "CPU 偏高"
	}
	return false, ""
}
