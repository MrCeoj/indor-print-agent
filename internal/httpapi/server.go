// Package httpapi is the agent's entire external surface: three routes on
// loopback, and the CORS dance a browser needs to reach them.
//
// The API is deliberately ignorant of receipts. It lists queues and it writes
// bytes. Everything about what a ticket looks like - layout, the logo, the
// drawer pulse, the cut - is built in the web app and arrives here as an opaque
// blob, so a change to the ticket never ships a new binary to a counter.
package httpapi

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thr1vefitstudio/print-agent/internal/config"
	"github.com/thr1vefitstudio/print-agent/internal/spooler"
)

// MaxJobBytes caps a single print job. A full-page 576-dot raster receipt is
// around 60KB; 4MB is room for a pathological one and still small enough that
// a runaway caller cannot exhaust a till's memory.
const MaxJobBytes = 4 << 20

// Server holds the agent's runtime state.
type Server struct {
	cfg     *config.Config
	sp      spooler.Spooler
	version string
	logger  *log.Logger

	// One printer, one drawer, one job at a time. Two tabs printing at once
	// would interleave two ESC/POS streams into one queue, and the result is
	// not two receipts - it is one unreadable one.
	printMu sync.Mutex

	jobSeq atomic.Uint64
}

// New builds the server.
func New(cfg *config.Config, sp spooler.Spooler, version string, logger *log.Logger) *Server {
	return &Server{cfg: cfg, sp: sp, version: version, logger: logger}
}

// Addr is the loopback socket the agent listens on. The host is hardcoded:
// binding anywhere else would put a printer on the LAN, and no configuration
// value should be able to do that.
func (s *Server) Addr() string {
	return net.JoinHostPort("127.0.0.1", fmt.Sprint(s.cfg.Port))
}

// Handler wires the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.HandleFunc("/v1/printers", s.requireAuth(s.handlePrinters))
	mux.HandleFunc("/v1/print", s.requireAuth(s.handlePrint))
	return s.withCORS(mux)
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: msg, Code: code})
}

// withCORS answers preflights and stamps the response headers.
//
// The Private Network Access header is the part that is easy to miss: Chrome
// treats a request from a public HTTPS page to 127.0.0.1 as a private-network
// request and preflights it, and without the matching allow header the fetch
// fails before it ever reaches a route.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// Health is readable from any origin. It discloses presence and a
		// version number and nothing else, and having it answer freely is what
		// lets a terminal with a mistyped allowlist report "wrong origin"
		// instead of looking like an agent that was never installed.
		health := r.URL.Path == "/v1/health"
		allowed := s.cfg.OriginAllowed(origin)

		if origin != "" && (allowed || health) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}

		if r.Method == http.MethodOptions {
			if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
				w.Header().Set("Access-Control-Allow-Private-Network", "true")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if !health && !allowed {
			// Includes the no-Origin case: curl, another local process, a
			// native app. The browser is this agent's only client.
			s.logf("refused %s %s from origin %q", r.Method, r.URL.Path, origin)
			writeErr(w, http.StatusForbidden, "origin_not_allowed",
				"this origin is not allowed to use the print agent")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// requireAuth checks the bearer token in constant time.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token == "" {
			writeErr(w, http.StatusServiceUnavailable, "no_token",
				"the agent has no token configured; re-run install.ps1")
			return
		}
		presented := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if subtle.ConstantTimeCompare([]byte(presented), []byte(s.cfg.Token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "bad_token", "invalid terminal token")
			return
		}
		next(w, r)
	}
}

type healthBody struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	writeJSON(w, http.StatusOK, healthBody{
		Name:    "thr1ve-print-agent",
		Version: s.version,
		OS:      goOS(),
		Arch:    goArch(),
	})
}

func (s *Server) handlePrinters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	printers, err := s.sp.List()
	if err != nil {
		s.logf("listing printers: %v", err)
		writeErr(w, http.StatusInternalServerError, "enumerate_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"printers": printers})
}

type printRequest struct {
	Printer    string `json:"printer"`
	JobName    string `json:"jobName"`
	DataBase64 string `json:"dataBase64"`
}

func (s *Server) handlePrint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}

	// Base64 inflates by a third; the limit is on the wire, the cap below is
	// on the decoded bytes.
	r.Body = http.MaxBytesReader(w, r.Body, MaxJobBytes*4/3+1024)

	var req printRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", "could not read the print request")
		return
	}
	if strings.TrimSpace(req.Printer) == "" {
		writeErr(w, http.StatusBadRequest, "no_printer", "no printer named")
		return
	}

	data, err := base64.StdEncoding.DecodeString(req.DataBase64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_payload", "dataBase64 is not valid base64")
		return
	}
	if len(data) == 0 {
		writeErr(w, http.StatusBadRequest, "empty_job", "nothing to print")
		return
	}
	if len(data) > MaxJobBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large",
			fmt.Sprintf("job is %d bytes, limit is %d", len(data), MaxJobBytes))
		return
	}

	s.printMu.Lock()
	err = s.sp.WriteRaw(req.Printer, req.JobName, data)
	s.printMu.Unlock()

	if err != nil {
		s.logf("print to %q failed: %v", req.Printer, err)
		writeErr(w, http.StatusBadGateway, "spool_failed", err.Error())
		return
	}

	id := fmt.Sprintf("%d-%d", time.Now().Unix(), s.jobSeq.Add(1))
	s.logf("printed %d bytes to %q as job %s", len(data), req.Printer, id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "jobId": id, "bytes": len(data)})
}

func (s *Server) logf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Printf(format, args...)
	}
}
