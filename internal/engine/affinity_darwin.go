//go:build darwin

package engine

import "fmt"

// pinToCore on macOS: macOS does not expose sched_setaffinity.
// We print an informational warning and return nil so dev builds work seamlessly.
// In production (Linux), affinity_linux.go takes effect via build tags.
func pinToCore(cpuIndex int) error {
	fmt.Printf("[AFFINITY] macOS: skipping hard pin for CPU core %d (not supported by kernel). On Linux this would call sched_setaffinity.\n", cpuIndex)
	return nil
}
