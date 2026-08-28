//go:build windows

package console

import (
	"syscall"
	"unsafe"
)

var procGetConsoleProcessList = syscall.NewLazyDLL("kernel32.dll").
	NewProc("GetConsoleProcessList")

// startedFromExplorer counts the processes attached to this console.
//
// Exactly one - us - means the console was created for this process, which is
// what happens on a double-click. Launched from an existing PowerShell or cmd,
// the shell is attached too and the count is at least two.
func startedFromExplorer() bool {
	var pids [8]uint32
	n, _, _ := procGetConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&pids[0])),
		uintptr(len(pids)),
	)
	return n == 1
}
