//go:build !unix && !windows

package inventory

// diskUsage is unsupported here; the agent only ships for Linux, this keeps
// it building elsewhere.
func diskUsage(string) (total, free uint64, ok bool) { return 0, 0, false }

func platformMemory() (total, avail uint64) { return 0, 0 }
