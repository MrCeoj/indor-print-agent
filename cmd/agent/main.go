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
//	agent pair        make this agent its venue's print station
//	agent unpair      stop being a print station
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
	"github.com/thr1vefitstudio/print-agent/internal/relay"
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
	case "pair":
		err = pair()
	case "unpair":
		err = unpair()
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
  agent pair <server> <code>
                    make this PC its venue's print station, so tablets
                    can print here (the code is shown on the venue screen)
  agent unpair      stop being a print station

`)
}

// build wires a config, a spooler and a logger into an HTTP server, and into
// the relay loop when this agent is a print station.
func build() (*httpapi.Server, *relay.Relay, *config.Config, io.Closer, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, nil, nil, err
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

	// One spooler, serialised, shared by both: a loopback print and a relayed
	// one must queue behind each other, never interleave.
	sp := spooler.Serial(spooler.New())

	var rl *relay.Relay
	if cfg.Relay != nil {
		rl = relay.New(*cfg.Relay, sp, version, logger)
	}
	return httpapi.New(cfg, sp, version, logger), rl, cfg, closer, nil
}

// serve starts the listener and hands back a shutdown function.
func serve() (stop func(context.Context) error, addr string, err error) {
	srv, rl, cfg, closer, err := build()
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

	// The relay starts only once the loopback API is up, so a second copy that
	// failed to bind above never also starts taking the venue's jobs.
	relayCtx, stopRelay := context.WithCancel(context.Background())
	relayDone := make(chan struct{})
	if rl != nil {
		go func() {
			defer close(relayDone)
			rl.Run(relayCtx)
		}()
	} else {
		close(relayDone)
	}

	return func(ctx context.Context) error {
		stopRelay()
		err := httpSrv.Shutdown(ctx)
		select {
		case <-relayDone:
		case <-ctx.Done():
		}
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
	if cfg.Relay != nil {
		fmt.Printf("station        %q via %s\n", cfg.Relay.StationName, cfg.Relay.ServerURL)
	} else {
		fmt.Println("station        not paired (tablets cannot print here)")
	}

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

// pair makes this agent its venue's print station.
//
// It also adds the server's origin to the allowlist: the list is persisted, so
// an agent installed before this app existed would otherwise refuse the very
// pages that now print through it. And it restarts the service, so the
// station is live when this command returns.
func pair() error {
	if len(os.Args) < 4 {
		return fmt.Errorf("usage: agent pair <server url> <code>")
	}
	serverURL, err := relay.ValidateServerURL(os.Args[2])
	if err != nil {
		return err
	}
	code := strings.Join(os.Args[3:], "")

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if _, err := cfg.EnsureToken(); err != nil {
		return err
	}

	link, err := relay.Pair(serverURL, code, version)
	if err != nil {
		return err
	}
	cfg.Relay = &link
	cfg.AllowOrigin(serverURL)
	if err := cfg.Save(); err != nil {
		return err
	}

	fmt.Printf("Paired as station %q with %s.\n", link.StationName, serverURL)
	restartIfInstalled()
	return nil
}

func unpair() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Relay == nil {
		fmt.Println("This agent is not a print station.")
		return nil
	}
	cfg.Relay = nil
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Println("No longer a print station. Remove it from the venue screen as well.")
	restartIfInstalled()
	return nil
}

// restartIfInstalled picks up a changed config. Best effort: in LogonTask mode
// there is no service, and the person is told what to do instead.
func restartIfInstalled() {
	_ = service.Stop()
	if err := service.Start(); err != nil {
		fmt.Println("Restart the agent (or sign out and back in) for this to take effect.")
		return
	}
	fmt.Println("Restarted the service.")
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
