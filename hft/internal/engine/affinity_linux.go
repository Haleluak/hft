//go:build linux

package engine

import "golang.org/x/sys/unix"

// pinToCore sets sched_setaffinity on linux: hard pins this thread to cpuIndex only.
func pinToCore(cpuIndex int) error {
	var mask unix.CPUSet
	mask.Set(cpuIndex)
	return unix.SchedSetaffinity(0, &mask)
}
