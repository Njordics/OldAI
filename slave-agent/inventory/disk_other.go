//go:build !unix

package inventory

// diskUsage is unsupported off Unix; the agent only ships for Linux, this
// keeps it building on developer machines.
func diskUsage(string) (total, free uint64, ok bool) { return 0, 0, false }
