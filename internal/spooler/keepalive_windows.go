//go:build windows

package spooler

import "runtime"

// runtimeKeepAlive is a named wrapper so the reason is greppable: the strings
// returned by EnumPrinters point into the byte slice we allocated, and the GC
// is free to collect that slice the instant the last direct reference dies.
func runtimeKeepAlive(v any) { runtime.KeepAlive(v) }
