// Package relay makes the agent a print station: it takes jobs from the server
// for devices that cannot reach a printer themselves.
//
// A waiter's tablet has no agent and no spooler, and a browser cannot open a
// socket to a printer. So the tablet queues the ticket's bytes on the server,
// and this loop - running on one always-on PC at the venue - long-polls for
// them and writes them to a local queue exactly as the loopback API would.
//
// Every connection is OUTBOUND. Nothing on the venue network is opened, and no
// printer needs a fixed address: the PC reaches its printers through Windows
// queues, the same as for its own receipts.
//
// The server owns the rules (0153): a job not claimed within 90 s is never
// handed out, and a claimed job is never handed out twice. So this loop does
// not retry a print. It reports what happened and moves on.
package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/thr1vefitstudio/print-agent/internal/config"
	"github.com/thr1vefitstudio/print-agent/internal/spooler"
)

const (
	// waitSeconds is how long the server holds a poll open. Below any reverse
	// proxy's idle timeout; the server caps it at 25 too.
	waitSeconds = 25

	heartbeatEvery = 60 * time.Second
	maxBackoff     = 60 * time.Second

	// A revoked or re-paired station is refused until someone pairs it again.
	// Asking every second would only fill the log.
	refusedBackoff = 5 * time.Minute

	maxJobBytes = 4 << 20
)

// ErrRefused is the server saying this station's credential is not valid.
var ErrRefused = errors.New("station credential refused; run `agent pair` again")

// Relay is one station's loop.
type Relay struct {
	cfg     config.Relay
	sp      spooler.Spooler
	version string
	logger  *log.Logger
	client  *http.Client
}

// New builds the loop. ValidateServerURL should already have accepted cfg.ServerURL.
func New(cfg config.Relay, sp spooler.Spooler, version string, logger *log.Logger) *Relay {
	return &Relay{
		cfg:     cfg,
		sp:      sp,
		version: version,
		logger:  logger,
		// Longer than the server's hold, so a poll that returns empty is a 204,
		// not a client timeout.
		client: &http.Client{Timeout: (waitSeconds + 15) * time.Second},
	}
}

// ValidateServerURL refuses anything but an HTTPS origin, except loopback for a
// dev server. The station token travels in every request; it does not travel
// in clear text across the internet.
func ValidateServerURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%q is not a URL", raw)
	}
	host := u.Hostname()
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", fmt.Errorf("the server must be https:// (got %q)", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

// Run polls until ctx is cancelled.
func (r *Relay) Run(ctx context.Context) {
	r.logf("relay: station %q polling %s", r.cfg.StationName, r.cfg.ServerURL)

	go r.heartbeats(ctx)

	backoff := time.Second
	for ctx.Err() == nil {
		err := r.pollOnce(ctx)
		switch {
		case err == nil:
			backoff = time.Second
			continue
		case ctx.Err() != nil:
			return
		case errors.Is(err, ErrRefused):
			r.logf("relay: %v", err)
			sleep(ctx, refusedBackoff)
		default:
			r.logf("relay: %v (retrying in %s)", err, backoff)
			sleep(ctx, backoff)
			backoff = min(backoff*2, maxBackoff)
		}
	}
}

type job struct {
	ID         string `json:"id"`
	Printer    string `json:"printer"`
	JobName    string `json:"jobName"`
	DataBase64 string `json:"dataBase64"`
}

// pollOnce waits for one job and, if one comes, prints and reports it.
func (r *Relay) pollOnce(ctx context.Context) error {
	res, err := r.do(ctx, http.MethodGet, fmt.Sprintf("/api/print/agent/jobs?wait=%d", waitSeconds), nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := statusError(res); err != nil {
		return err
	}

	var j job
	if err := json.NewDecoder(io.LimitReader(res.Body, maxJobBytes*2)).Decode(&j); err != nil {
		return fmt.Errorf("reading job: %w", err)
	}

	printErr := r.print(j)
	if printErr != nil {
		r.logf("relay: job %s to %q failed: %v", j.ID, j.Printer, printErr)
	}
	return r.report(ctx, j.ID, printErr)
}

func (r *Relay) print(j job) error {
	data, err := base64.StdEncoding.DecodeString(j.DataBase64)
	if err != nil {
		return fmt.Errorf("payload is not base64")
	}
	if len(data) == 0 || len(data) > maxJobBytes {
		return fmt.Errorf("job is %d bytes", len(data))
	}
	if err := r.sp.WriteRaw(j.Printer, j.JobName, data); err != nil {
		return err
	}
	r.logf("relay: printed %d bytes to %q as job %s", len(data), j.Printer, j.ID)
	return nil
}

func (r *Relay) report(ctx context.Context, id string, printErr error) error {
	body := map[string]any{"ok": printErr == nil}
	if printErr != nil {
		msg := printErr.Error()
		if len(msg) > 500 {
			msg = msg[:500]
		}
		body["error"] = msg
	}
	res, err := r.do(ctx, http.MethodPost, "/api/print/agent/jobs/"+url.PathEscape(id), body)
	if err != nil {
		return fmt.Errorf("reporting job %s: %w", id, err)
	}
	defer res.Body.Close()
	return statusError(res)
}

func (r *Relay) heartbeats(ctx context.Context) {
	tick := time.NewTicker(heartbeatEvery)
	defer tick.Stop()
	for {
		if err := r.heartbeat(ctx); err != nil && ctx.Err() == nil {
			r.logf("relay: heartbeat: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// heartbeat tells the server which queues this PC can see, so a tablet can
// offer them. Advisory, like the status each queue reports.
func (r *Relay) heartbeat(ctx context.Context) error {
	printers, err := r.sp.List()
	if err != nil {
		return fmt.Errorf("listing printers: %w", err)
	}
	if printers == nil {
		printers = []spooler.Printer{}
	}
	res, err := r.do(ctx, http.MethodPost, "/api/print/agent/heartbeat",
		map[string]any{"version": r.version, "printers": printers})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return statusError(res)
}

func (r *Relay) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.cfg.ServerURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.cfg.Token)
	req.Header.Set("User-Agent", "thr1ve-print-agent/"+r.version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return r.client.Do(req)
}

// Pair swaps a one-time code for this station's credential.
func Pair(serverURL, code, version string) (config.Relay, error) {
	raw, _ := json.Marshal(map[string]string{"code": code})
	req, err := http.NewRequest(http.MethodPost, serverURL+"/api/print/agent/pair", bytes.NewReader(raw))
	if err != nil {
		return config.Relay{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "thr1ve-print-agent/"+version)

	res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return config.Relay{}, fmt.Errorf("reaching %s: %w", serverURL, err)
	}
	defer res.Body.Close()
	if err := statusError(res); err != nil {
		return config.Relay{}, err
	}

	var out struct {
		Token string `json:"token"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&out); err != nil || out.Token == "" {
		return config.Relay{}, fmt.Errorf("the server's answer could not be read")
	}
	return config.Relay{ServerURL: serverURL, Token: out.Token, StationName: out.Name}, nil
}

// statusError turns a non-2xx answer into an error carrying the server's code.
func statusError(res *http.Response) error {
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&body)
	if res.StatusCode == http.StatusUnauthorized && body.Code == "bad_station" {
		return ErrRefused
	}
	if body.Error != "" {
		return fmt.Errorf("server answered %d: %s", res.StatusCode, body.Error)
	}
	return fmt.Errorf("server answered %d", res.StatusCode)
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (r *Relay) logf(format string, args ...any) {
	if r.logger != nil {
		r.logger.Printf(format, args...)
	}
}
