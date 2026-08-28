// Package console answers one question: was this process started by someone
// double-clicking it in Explorer?
//
// It matters because a console program launched that way owns its window. The
// moment it returns, Windows destroys the window - so a startup error prints
// perfectly and is on screen for about four frames. "It opens a terminal and
// it is killed as soon as it appears" is the whole bug report a user can give,
// and it describes every possible failure identically.
package console

// StartedFromExplorer reports whether this process created its own console,
// which is what a double-click does. False when run from an existing prompt,
// and false on every non-Windows platform.
func StartedFromExplorer() bool { return startedFromExplorer() }
