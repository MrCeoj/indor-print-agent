//go:build !windows

package spooler

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// devSink stands in for a print spooler on a developer's machine.
//
// It exists so the interesting half of this feature - the app rasterizing a
// ticket into ESC/POS - can be built and checked without Windows and without
// paper. Every job is written to a file whose bytes are exactly what the
// printer would have received, which is the artefact the decode script reads
// back into a PNG.
type devSink struct{ dir string }

func newSpooler() Spooler {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return &devSink{dir: filepath.Join(home, ".thr1ve-print", "jobs")}
}

// List reports one fake queue. Named to match the real hardware so the app's
// receipt-printer name heuristic is exercised rather than bypassed.
func (d *devSink) List() ([]Printer, error) {
	return []Printer{
		{Name: "EC Line 80250 (dev sink)", IsDefault: true, Status: "ready", Port: d.dir},
	}, nil
}

func (d *devSink) WriteRaw(printer, jobName string, data []byte) error {
	if err := os.MkdirAll(d.dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", d.dir, err)
	}
	name := fmt.Sprintf("%s-%s.bin", time.Now().Format("20060102-150405.000"), safe(jobName))
	path := filepath.Join(d.dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	fmt.Printf("dev sink: %d bytes -> %s (printer %q)\n", len(data), path, printer)
	return nil
}

func safe(s string) string {
	if s == "" {
		return "job"
	}
	out := []rune(s)
	for i, r := range out {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			out[i] = '_'
		}
	}
	return string(out)
}
