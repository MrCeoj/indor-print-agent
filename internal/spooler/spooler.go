// Package spooler is the only part of the agent that knows about an operating
// system's printers. Everything above it deals in "a name and some bytes".
//
// Two implementations: winspool on Windows (the real one), and a file sink
// everywhere else, so the raster pipeline in the web app can be developed and
// verified on a Mac with no printer attached.
package spooler

import "sync"

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

// Serial wraps a spooler so only one job is written at a time, whoever sent it.
//
// One printer, one drawer, one job at a time. Two jobs at once - a cashier at
// this PC and a waiter's tablet through the relay - would interleave two
// ESC/POS streams into one queue, and the result is not two receipts, it is one
// unreadable one. The lock lives here, below both callers, so neither can
// forget it.
func Serial(sp Spooler) Spooler { return &serial{sp: sp} }

type serial struct {
	mu sync.Mutex
	sp Spooler
}

func (s *serial) List() ([]Printer, error) { return s.sp.List() }

func (s *serial) WriteRaw(printer, jobName string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sp.WriteRaw(printer, jobName, data)
}
