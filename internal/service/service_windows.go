//go:build windows

package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// report writes to the Windows Application log.
//
// A service that fails to start has nowhere else to speak. Its stdout goes to
// no console, and the file log is itself one of the things that can be
// unreachable - a permissions mistake on ProgramData takes out the log and the
// config together, which is exactly the failure that most needs explaining.
// Best-effort throughout: if the source was never registered, the message is
// lost and the service still behaves correctly.
func report(level uint16, message string) {
	log, err := eventlog.Open(Name)
	if err != nil {
		return
	}
	defer log.Close()

	switch level {
	case eventlog.Error:
		_ = log.Error(1, message)
	case eventlog.Warning:
		_ = log.Warning(1, message)
	default:
		_ = log.Info(1, message)
	}
}

// IsService reports whether this process was started by the service control
// manager rather than a person at a prompt.
func IsService() bool {
	is, err := svc.IsWindowsService()
	return err == nil && is
}

type handler struct{ work Work }

// Execute is the SCM's entry point.
//
// The accepted controls include Shutdown as well as Stop: a till is usually
// switched off at the wall at the end of a night, and a service that only
// listens for Stop gets killed rather than closed.
func (h *handler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	status <- svc.Status{State: svc.StartPending}
	if err := h.work.Start(); err != nil {
		report(eventlog.Error, fmt.Sprintf("%s failed to start: %v", Name, err))
		status <- svc.Status{State: svc.Stopped}
		// A non-zero exit code is what makes the SCM's restart policy fire.
		return false, 1
	}
	report(eventlog.Info, fmt.Sprintf("%s started", Name))
	status <- svc.Status{State: svc.Running, Accepts: accepted}

	for c := range req {
		switch c.Cmd {
		case svc.Interrogate:
			status <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			status <- svc.Status{State: svc.StopPending}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = h.work.Stop(ctx)
			cancel()
			status <- svc.Status{State: svc.Stopped}
			return false, 0
		}
	}
	return false, 0
}

// Run hands control to the SCM.
func Run(work Work) error { return svc.Run(Name, &handler{work: work}) }

func connect() (*mgr.Mgr, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf("connecting to the service manager (run as administrator): %w", err)
	}
	return m, nil
}

// Install registers the service to start at boot and restart on failure.
func Install() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating this binary: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", exe, err)
	}

	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	if s, err := m.OpenService(Name); err == nil {
		s.Close()
		return fmt.Errorf("%s is already installed; run `%s uninstall` first", Name, filepath.Base(exe))
	}

	s, err := m.CreateService(Name, exe, mgr.Config{
		DisplayName: DisplayName,
		Description: Description,
		// Delayed: the spooler service and the USB stack want to be up first,
		// and nothing is waiting on this agent at second zero of a boot.
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
	}, "run")
	if err != nil {
		return fmt.Errorf("creating the service: %w", err)
	}
	defer s.Close()

	// Register the event source, so the handler above has somewhere to write.
	// An existing registration is not an error - reinstalling is routine.
	if err := eventlog.InstallAsEventCreate(
		Name, eventlog.Error|eventlog.Warning|eventlog.Info,
	); err != nil && !strings.Contains(err.Error(), "already exists") {
		fmt.Fprintf(os.Stderr, "warning: could not register the event source: %v\n", err)
	}

	// Restart forever, backing off. A print agent that stays down after one
	// crash is a counter that cannot take money until someone notices.
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 86400); err != nil {
		return fmt.Errorf("setting the restart policy: %w", err)
	}

	return nil
}

// Uninstall stops and removes the service.
func Uninstall() error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(Name)
	if err != nil {
		return fmt.Errorf("%s is not installed", Name)
	}
	defer s.Close()

	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err != nil {
			return fmt.Errorf("stopping %s: %w", Name, err)
		}
		waitFor(s, svc.Stopped, 20*time.Second)
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("deleting %s: %w", Name, err)
	}
	_ = eventlog.Remove(Name)
	return nil
}

// Start starts an installed service.
func Start() error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(Name)
	if err != nil {
		return fmt.Errorf("%s is not installed", Name)
	}
	defer s.Close()

	if err := s.Start("run"); err != nil {
		return fmt.Errorf("starting %s: %w", Name, err)
	}
	return waitFor(s, svc.Running, 20*time.Second)
}

// Stop stops an installed service.
func Stop() error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(Name)
	if err != nil {
		return fmt.Errorf("%s is not installed", Name)
	}
	defer s.Close()

	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("stopping %s: %w", Name, err)
	}
	return waitFor(s, svc.Stopped, 20*time.Second)
}

func waitFor(s *mgr.Service, want svc.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := s.Query()
		if err != nil {
			return fmt.Errorf("querying %s: %w", Name, err)
		}
		if st.State == want {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s to reach the requested state", Name)
}
