//go:build !windows

package console

// A developer's shell always owns the terminal, and there is no Explorer to be
// launched from.
func startedFromExplorer() bool { return false }
