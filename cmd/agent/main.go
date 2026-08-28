// Command agent is the Thr1ve front desk print agent.
//
// It replaces QZ Tray with something a counter can be expected to keep running:
// one static binary, no Java, no certificates, and an HTTP API on loopback with
// exactly three routes. The web app builds the whole ESC/POS stream - the
// rasterised ticket, the drawer pulse, the cut - and this process only decides
// which queue receives it.
//
//	agent run         start in the foreground (and the SCM's entry point)
//	agent install     register the Windows service, minting a token if needed
//	agent uninstall   stop and remove the service
//	agent start/stop  control an installed service
//	agent token       print this terminal's token, for pasting into the app
//	agent status      show where the config lives and what is configured
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/thr1vefitstudio/print-agent/internal/config"
	"github.com/thr1vefitstudio/print-agent/internal/console"
	"github.com/thr1vefitstudio/print-agent/internal/httpapi"
	"github.com/thr1vefitstudio/print-agent/internal/service"
	"github.com/thr1vefitstudio/print-agent/internal/spooler"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=1.0.0"
var version = "dev"

func main() {
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	// Started by the service control manager: no arguments we can rely on, and
	// nowhere for a usage message to go.
	if service.IsService() {
		if err := runService(); err != nil {
			os.Exit(1)
		}
		return
	}

	var err error
	switch cmd {
	case "run":
		err = runForeground()
	case "install":
		err = install()
	case "uninstall":
		err = service.Uninstall()
		if err == nil {
			fmt.Println("Removed the Thr1ve Print Agent service.")
		}
	case "start":
		err = service.Start()
		if err == nil {
			fmt.Println("Started.")
		}
	case "stop":
		err = service.Stop()
		if err == nil {
			fmt.Println("Stopped.")
		}
	case "token":
		err = printToken()
	case "status":
		err = printStatus()
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		err = fmt.Errorf("unknown command %q", cmd)
	}

	if err != nil {
		fail(err)
	}
}

// fail prints an error and, when this window is about to vanish with it, waits.
//
// Without the pause a double-clicked binary reports every problem the same way:
// a console that appears and is gone before it can be read. The pause is the
// difference between "it does not work" and "Access is denied reading
// config.json", which are not the same bug report.
func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	if console.StartedFromExplorer() {
		fmt.Fprintln(os.Stderr, "\nPress Enter to close this window.")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
	os.Exit(1)
}

func usage() {
	fmt.Print(`Thr1ve Print Agent

  agent run         start in the foreground
  agent install     register the Windows service (run as administrator)
  agent uninstall   stop and remove the service
  agent start       start the installed service
  agent stop        stop the installed service
  agent token       print this terminal's token
  agent status      show the configuration

`)
}

// build wires a config, a spooler and a logger into an HTTP server.
func build() (*httpapi.Server, *config.Config, io.Closer, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, nil, err
	}

	// Log to a file and to stdout. The file is what a remote diagnosis reads;
	// stdout is what a person running it in the foreground reads.
	var closer io.Closer = io.NopCloser(nil)
	var out io.Writer = os.Stdout
	if f, ferr := os.OpenFile(cfg.LogFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); ferr == nil {
		out = io.MultiWriter(os.Stdout, f)
		closer = f
	}
	logger := log.New(out, "", log.LstdFlags|log.LUTC)

	return httpapi.New(cfg, spooler.New(), version, logger), cfg, closer, nil
}

// serve starts the listener and hands back a shutdown function.
func serve() (stop func(context.Context) error, addr string, err error) {
	srv, cfg, closer, err := build()
	if err != nil {
		return nil, "", err
	}
	if cfg.Token == "" {
		// Not fatal: /v1/health still answers, which is what turns "no token"
		// into a message in the app rather than a terminal that looks dead.
		log.Printf("warning: no token configured; run `agent install` or `agent token --new`")
	}

	httpSrv := &http.Server{
		Addr:              srv.Addr(),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ready := make(chan error, 1)
	go func() {
		err := httpSrv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		ready <- err
	}()

	// Give the bind a moment to fail loudly (port already taken is the common
	// one) rather than reporting a healthy start into a log nobody reads.
	select {
	case err := <-ready:
		if err != nil {
			_ = closer.Close()
			if inUse(err) {
				// Overwhelmingly the ordinary case: the service is already
				// running and someone double-clicked the binary as well. That
				// is not a fault to debug, so it does not read like one.
				return nil, "", fmt.Errorf(
					"something is already listening on %s.\n\n"+
						"If the %s service is running, this is expected - it is already doing the job,\n"+
						"and there is no need to start the agent by hand. Check with:\n\n"+
						"    sc query %s",
					srv.Addr(), config.ServiceName, config.ServiceName)
			}
			return nil, "", fmt.Errorf("listening on %s: %w", srv.Addr(), err)
		}
	case <-time.After(250 * time.Millisecond):
	}

	return func(ctx context.Context) error {
		err := httpSrv.Shutdown(ctx)
		_ = closer.Close()
		return err
	}, srv.Addr(), nil
}

func runForeground() error {
	stop, addr, err := serve()
	if err != nil {
		return err
	}
	fmt.Printf("Thr1ve Print Agent %s listening on http://%s\n", version, addr)
	fmt.Printf("Config: %s\n", config.Path())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig

	fmt.Println("\nShutting down.")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return stop(ctx)
}

func runService() error {
	var stop func(context.Context) error
	return service.Run(service.Work{
		Start: func() error {
			s, _, err := serve()
			if err != nil {
				return err
			}
			stop = s
			return nil
		},
		Stop: func(ctx context.Context) error {
			if stop == nil {
				return nil
			}
			return stop(ctx)
		},
	})
}

func install() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	created, err := cfg.EnsureToken()
	if err != nil {
		return err
	}
	if err := service.Install(); err != nil {
		return err
	}
	if err := service.Start(); err != nil {
		return err
	}

	fmt.Printf("Installed and started the Thr1ve Print Agent on http://127.0.0.1:%d\n", cfg.Port)
	if created {
		fmt.Println("\nA terminal token was generated. Paste it into the printer panel at /caja:")
	} else {
		fmt.Println("\nThis terminal already had a token. Paste it into the printer panel at /caja:")
	}
	fmt.Printf("\n    %s\n\n", cfg.Token)
	fmt.Printf("It is also stored in %s\n", config.Path())
	return nil
}

// printToken shows the token, or mints a new one with --new. Rotating is
// deliberately explicit: it invalidates whatever the browser has saved, and a
// cashier then cannot print until someone pastes the new value.
func printToken() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	rotate := len(os.Args) > 2 && os.Args[2] == "--new"

	if rotate {
		tok, err := config.NewToken()
		if err != nil {
			return err
		}
		cfg.Token = tok
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Println("A new token was generated. The old one no longer works.")
		fmt.Println("Restart the agent, then paste this into the printer panel at /caja:")
	}

	if _, err := cfg.EnsureToken(); err != nil {
		return err
	}
	fmt.Println(cfg.Token)
	return nil
}

func printStatus() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fmt.Printf("version        %s\n", version)
	fmt.Printf("config         %s\n", config.Path())
	fmt.Printf("log            %s\n", cfg.LogFile())
	fmt.Printf("listen         http://127.0.0.1:%d\n", cfg.Port)
	fmt.Printf("token          %s\n", present(cfg.Token))
	fmt.Printf("origins        %v\n", cfg.AllowedOrigins)

	printers, err := spooler.New().List()
	if err != nil {
		fmt.Printf("printers       error: %v\n", err)
		return nil
	}
	if len(printers) == 0 {
		fmt.Println("printers       none visible")
		fmt.Println("               If the cashier can print but this says none, the service is in")
		fmt.Println("               session 0 and cannot see per-user queues. See README.md.")
		return nil
	}
	for _, p := range printers {
		def := ""
		if p.IsDefault {
			def = "  (default)"
		}
		fmt.Printf("printer        %s [%s]%s\n", p.Name, p.Status, def)
	}
	return nil
}

// inUse recognises a taken port across platforms. Windows raises
// WSAEADDRINUSE (10048) rather than the EADDRINUSE Go names, so the message is
// checked too - a wrong guess here only costs a less specific error.
func inUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "address already in use") ||
		strings.Contains(text, "only one usage of each socket address")
}

func present(s string) string {
	if s == "" {
		return "not set"
	}
	return "set"
}
