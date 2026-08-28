//go:build windows

package spooler

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

// The winspool surface, bound lazily so a missing export is a runtime error on
// the one call that needs it rather than a process that will not start.
//
// These are declared here rather than taken from golang.org/x/sys/windows on
// purpose: the set is small, the signatures are stable since NT 4, and pinning
// them locally keeps the print path independent of that module's API churn.
var (
	winspool = syscall.NewLazyDLL("winspool.drv")

	procEnumPrinters      = winspool.NewProc("EnumPrintersW")
	procGetDefaultPrinter = winspool.NewProc("GetDefaultPrinterW")
	procOpenPrinter       = winspool.NewProc("OpenPrinterW")
	procClosePrinter      = winspool.NewProc("ClosePrinter")
	procStartDocPrinter   = winspool.NewProc("StartDocPrinterW")
	procEndDocPrinter     = winspool.NewProc("EndDocPrinter")
	procStartPagePrinter  = winspool.NewProc("StartPagePrinter")
	procEndPagePrinter    = winspool.NewProc("EndPagePrinter")
	procWritePrinter      = winspool.NewProc("WritePrinter")
)

const (
	printerEnumLocal       = 0x00000002
	printerEnumConnections = 0x00000004

	errInsufficientBuffer = 122
)

// printerInfo2 mirrors PRINTER_INFO_2W. Field order and widths are the ABI -
// do not reorder, and do not "tidy" the unused members away, because the
// spooler writes into this layout by offset.
type printerInfo2 struct {
	ServerName         *uint16
	PrinterName        *uint16
	ShareName          *uint16
	PortName           *uint16
	DriverName         *uint16
	Comment            *uint16
	Location           *uint16
	DevMode            uintptr
	SepFile            *uint16
	PrintProcessor     *uint16
	Datatype           *uint16
	Parameters         *uint16
	SecurityDescriptor uintptr
	Attributes         uint32
	Priority           uint32
	DefaultPriority    uint32
	StartTime          uint32
	UntilTime          uint32
	Status             uint32
	Jobs               uint32
	AveragePPM         uint32
}

// docInfo1 mirrors DOC_INFO_1W.
type docInfo1 struct {
	DocName    *uint16
	OutputFile *uint16
	Datatype   *uint16
}

type winSpooler struct{}

func newSpooler() Spooler { return &winSpooler{} }

// utf16PtrToString walks a NUL-terminated wide string the spooler placed
// inside our own buffer.
func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	n := 0
	for ptr := unsafe.Pointer(p); *(*uint16)(ptr) != 0; ptr = unsafe.Add(ptr, 2) {
		n++
		if n > 1<<16 {
			break // a runaway scan means a corrupt buffer; do not walk the heap
		}
	}
	return syscall.UTF16ToString(unsafe.Slice(p, n))
}

// statusText renders PRINTER_STATUS_* as something a cashier could act on.
// Only the flags that matter at a receipt printer are named; the rest collapse
// to "busy", because "PRINTER_STATUS_PAGE_PUNT" helps nobody at a counter.
func statusText(status uint32) string {
	if status == 0 {
		return "ready"
	}
	var parts []string
	for _, f := range []struct {
		bit  uint32
		text string
	}{
		{0x00000080, "offline"},
		{0x00000010, "paper out"},
		{0x00000008, "paper jam"},
		{0x00400000, "door open"},
		{0x00000001, "paused"},
		{0x00000002, "error"},
		{0x00001000, "not available"},
		{0x00100000, "needs attention"},
	} {
		if status&f.bit != 0 {
			parts = append(parts, f.text)
		}
	}
	if len(parts) == 0 {
		return "busy"
	}
	return strings.Join(parts, ", ")
}

// defaultPrinter is best-effort: a machine with no default is normal, and it
// only affects which queue the app preselects.
func defaultPrinter() string {
	var n uint32
	// First call fails with ERROR_INSUFFICIENT_BUFFER and sets n.
	procGetDefaultPrinter.Call(0, uintptr(unsafe.Pointer(&n)))
	if n == 0 || n > 1<<12 {
		return ""
	}
	buf := make([]uint16, n)
	ret, _, _ := procGetDefaultPrinter.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&n)),
	)
	if ret == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

// List enumerates local queues and connections to shared ones.
//
// PRINTER_ENUM_LOCAL alone would miss a printer shared from another PC, which
// some counters do; adding CONNECTIONS costs nothing when there are none.
func (w *winSpooler) List() ([]Printer, error) {
	const level = 2
	flags := uintptr(printerEnumLocal | printerEnumConnections)

	var needed, returned uint32
	// Sizing call. It is expected to fail.
	procEnumPrinters.Call(flags, 0, level, 0, 0,
		uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&returned)))

	if needed == 0 {
		return []Printer{}, nil
	}

	buf := make([]byte, needed)
	ret, _, err := procEnumPrinters.Call(flags, 0, level,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(needed),
		uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&returned)))
	if ret == 0 {
		if e, ok := err.(syscall.Errno); ok && e == errInsufficientBuffer {
			return nil, fmt.Errorf("enumerating printers: buffer grew between calls")
		}
		return nil, fmt.Errorf("enumerating printers: %w", err)
	}

	def := defaultPrinter()
	infos := unsafe.Slice((*printerInfo2)(unsafe.Pointer(&buf[0])), returned)

	out := make([]Printer, 0, returned)
	for i := range infos {
		name := utf16PtrToString(infos[i].PrinterName)
		if name == "" {
			continue
		}
		out = append(out, Printer{
			Name:      name,
			IsDefault: name == def,
			Status:    statusText(infos[i].Status),
			Port:      utf16PtrToString(infos[i].PortName),
		})
	}
	// buf owns every string above; keep it alive until they are copied out.
	runtimeKeepAlive(buf)
	return out, nil
}

// WriteRaw spools bytes verbatim.
//
// The datatype is RAW: ESC/POS is a byte stream for the printer's firmware, not
// a document, and any driver given the chance to interpret it emits pages of
// mojibake. Every handle is released on the way out, including on the error
// paths - a leaked printer handle wedges the queue until the service restarts.
func (w *winSpooler) WriteRaw(printer, jobName string, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("empty job")
	}

	namePtr, err := syscall.UTF16PtrFromString(printer)
	if err != nil {
		return fmt.Errorf("printer name: %w", err)
	}

	var h syscall.Handle
	ret, _, callErr := procOpenPrinter.Call(
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(unsafe.Pointer(&h)),
		0,
	)
	if ret == 0 {
		return fmt.Errorf("opening printer %q: %w", printer, callErr)
	}
	defer procClosePrinter.Call(uintptr(h))

	if jobName == "" {
		jobName = "Thr1ve"
	}
	docName, err := syscall.UTF16PtrFromString(jobName)
	if err != nil {
		return fmt.Errorf("job name: %w", err)
	}
	rawType, err := syscall.UTF16PtrFromString("RAW")
	if err != nil {
		return fmt.Errorf("datatype: %w", err)
	}

	doc := docInfo1{DocName: docName, OutputFile: nil, Datatype: rawType}
	job, _, callErr := procStartDocPrinter.Call(uintptr(h), 1, uintptr(unsafe.Pointer(&doc)))
	if job == 0 {
		return fmt.Errorf("starting job on %q: %w", printer, callErr)
	}
	defer procEndDocPrinter.Call(uintptr(h))

	ret, _, callErr = procStartPagePrinter.Call(uintptr(h))
	if ret == 0 {
		return fmt.Errorf("starting page on %q: %w", printer, callErr)
	}
	defer procEndPagePrinter.Call(uintptr(h))

	// WritePrinter is not obliged to take the whole buffer in one call.
	for off := 0; off < len(data); {
		var written uint32
		ret, _, callErr = procWritePrinter.Call(
			uintptr(h),
			uintptr(unsafe.Pointer(&data[off])),
			uintptr(len(data)-off),
			uintptr(unsafe.Pointer(&written)),
		)
		if ret == 0 {
			return fmt.Errorf("writing to %q: %w", printer, callErr)
		}
		if written == 0 {
			return fmt.Errorf("writing to %q: spooler accepted 0 of %d remaining bytes", printer, len(data)-off)
		}
		off += int(written)
	}

	return nil
}
