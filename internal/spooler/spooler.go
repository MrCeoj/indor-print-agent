// Package spooler is the only part of the agent that knows about an operating
// system's printers. Everything above it deals in "a name and some bytes".
//
// Two implementations: winspool on Windows (the real one), and a file sink
// everywhere else, so the raster pipeline in the web app can be developed and
// verified on a Mac with no printer attached.
package spooler

// Printer is one queue as the OS reports it.
type Printer struct {
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
	// Status is a human-readable summary ("ready", "offline", "paper out").
	// Advisory only: a thermal printer that is merely unplugged frequently
	// still reports ready, so the app must never gate printing on it.
	Status string `json:"status"`
	Port   string `json:"port,omitempty"`
}

// Spooler is the whole surface the HTTP layer needs.
type Spooler interface {
	// List enumerates every queue this process can see. On Windows that is
	// scoped by the session the agent runs in - see the session 0 note in
	// README.md.
	List() ([]Printer, error)

	// WriteRaw sends bytes to a queue as a RAW job, bypassing the driver's
	// rendering. ESC/POS is not a page description language; anything that
	// interprets it produces a printer full of garbage.
	WriteRaw(printer, jobName string, data []byte) error
}

// New returns the platform's spooler.
func New() Spooler { return newSpooler() }
