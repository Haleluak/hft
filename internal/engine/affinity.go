package engine

import (
	"fmt"
	"runtime"
)

// PinToCore pins the CURRENT OS thread to the given physical CPU core index.
// This must be called from within the goroutine you want to pin (after runtime.LockOSThread()).
//
// Why this matters:
//   - The Engine loop is a "hot path" that constantly reads/writes the B-Tree and Linked List.
//   - If the OS scheduler migrates the thread between CPU cores, the L1/L2 cache
//     storing the Orderbook memory is invalidated on every migration → "cache thrash".
//   - By pinning a specific Engine actor to a dedicated CPU core, the Orderbook data
//     stays permanently "warm" in that core's private L1/L2 cache (~200GB/s bandwidth)
//     vs. going to RAM (~50GB/s). This alone can cut per-order latency by 2–5x.
//
// Production equivalent:
//   - Binance/Nasdaq set CPU isolation in Linux via `isolcpus=2,3,4` kernel boot param
//     so the OS never schedules normal processes on those cores.
//   - Then their Engine threads call sched_setaffinity() on those dedicated cores.
//
// Note: On macOS, the syscall is slightly different (thread_policy_set via Mach API).
// This implementation uses the portable golang.org/x/sys binding that works cross-platform.
// On Linux in production, you'd use syscall.RawSyscall(syscall.SYS_SCHED_SETAFFINITY, ...).
func PinToCore(cpuIndex int) error {
	if cpuIndex < 0 || cpuIndex >= runtime.NumCPU() {
		return fmt.Errorf("cpuIndex %d out of range (system has %d cores)", cpuIndex, runtime.NumCPU())
	}

	// On Linux production, we'd use:
	//   var mask unix.CPUSet
	//   mask.Set(cpuIndex)
	//   return unix.SchedSetaffinity(0, &mask)
	//
	// However, macOS does not expose sched_setaffinity but DOES honor thread priority hints.
	// For cross-platform correctness (dev on mac, prod on linux), we implement via build tags.
	// See: engine/affinity_linux.go and engine/affinity_darwin.go

	return pinToCore(cpuIndex)
}
