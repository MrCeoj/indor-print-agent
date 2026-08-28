# Thr1ve Print Agent

A single Go binary that lets `/caja` print receipts and open the cash drawer on
a Windows front desk terminal. It replaces QZ Tray.

## Why it exists

QZ Tray is a Java desktop app the browser reaches over `wss://localhost:8181`.
Four things about that shape were unreliable in practice:

- **A JVM on a till.** Nothing about printing a receipt needs one.
- **A PKI dance.** Signing existed only to stop QZ prompting the cashier on
  every reconnect: a server-side signing oracle, a certificate per deployment
  and an `authcert.override=` line in each terminal's `qz-tray.properties`.
  All of it was failure surface for a local printer.
- **A local TLS certificate**, because an HTTPS page cannot open a plain
  WebSocket.
- **Rasterising on the terminal.** The letterhead logo travelled as a URL the
  till fetched at print time, so a terminal that could not reach the CMS
  silently printed a receipt with no letterhead. Text went out as CP437, which
  has no `ó` — a client named `Muñoz` was printed `Mu?oz`.

This agent does none of that. It lists printers and writes bytes. **The whole
ESC/POS stream — the rasterised ticket, the drawer pulse, the cut — is built in
the web app** (`src/lib/printing/`), so changing a receipt never means shipping
a new binary to a counter.

## Install

Unzip the release on the terminal and, from an elevated PowerShell:

```powershell
powershell -ExecutionPolicy Bypass -File install.ps1
```

It copies the binary to `C:\Program Files\Thr1vePrint`, registers the service,
starts it, and prints a token. Paste that token into the printer panel at
`/caja` on that terminal, once.

### If `agent status` reports no printers

The service runs under LocalSystem in **session 0**, which can see machine-wide
printer queues but not ones installed under a single user account. USB receipt
printers normally install machine-wide, so this is rare — but when it happens,
the cashier can print from Word and the agent sees nothing.

Reinstall in the other mode:

```powershell
powershell -ExecutionPolicy Bypass -File install.ps1 -Mode LogonTask
```

That runs the same binary in the cashier's own session at logon. It needs the
terminal to log in automatically, which a till normally does.

## Troubleshooting

### The exe opens a window that closes instantly

It hit a startup error and exited. Since 1.0.1 the window stays open and waits
for Enter when it was double-clicked, so the error is readable. On an older
build, run it from a prompt that already exists instead — the window is then the
shell's and survives:

```powershell
& 'C:\Program Files\Thr1vePrint\thr1ve-print-agent.exe' status
```

The most common cause is not a fault at all: **the service is already running
and holding port 9101**, so a second copy cannot bind. That is the healthy
state. `sc query Thr1vePrint` will say `RUNNING`.

### The service installs without complaint but nothing answers on 9101

Check, in this order:

```powershell
sc query Thr1vePrint
& 'C:\Program Files\Thr1vePrint\thr1ve-print-agent.exe' status
Get-Content C:\ProgramData\Thr1vePrint\agent.log -Tail 40
Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='Thr1vePrint'} -MaxEvents 20 |
  Format-List TimeCreated, LevelDisplayName, Message
```

**If `status` reports `Access is denied` reading `config.json`**, the data
directory's permissions are broken and every path fails at once — the service
cannot start, the binary cannot run by hand, and `/caja` reports no agent.
Installers before 1.0.1 caused this on any non-English Windows: they granted
rights to the account names `Administrators` and `SYSTEM`, which a Spanish
install calls `Administradores` and `SISTEMA`. icacls refused the names it could
not map, but inheritance had already been stripped, leaving a folder readable by
nobody. Recover by deleting the folder and reinstalling:

```powershell
sc stop Thr1vePrint
Remove-Item -Recurse -Force C:\ProgramData\Thr1vePrint
.\install.ps1
```

That mints a **new token**, so paste the new one into `/caja`.

### `agent status` lists no printers

See "If `agent status` reports no printers" under Install — that is the session 0
case, and `-Mode LogonTask` is the answer.

### /caja says "Sin agente en esta terminal" but the agent is running

The agent is up and the browser cannot reach it. Either the origin is not on the
allowlist (`allowedOrigins` in `C:\ProgramData\Thr1vePrint\config.json` — the
panel says so explicitly when that is the cause) or the browser is blocking
loopback. Test the second directly, in DevTools on the page that is failing:

```js
await (await fetch('http://127.0.0.1:9101/v1/health')).json();
```

## Commands

|                        |                                                                        |
| ---------------------- | ---------------------------------------------------------------------- |
| `agent run`            | Start in the foreground. Also the service's entry point.               |
| `agent install`        | Register the service, minting a token if there is none.                |
| `agent uninstall`      | Stop and remove the service.                                           |
| `agent start` / `stop` | Control an installed service.                                          |
| `agent token`          | Print this terminal's token. `--new` rotates it.                       |
| `agent status`         | Where the config lives, what is configured, what printers are visible. |

## HTTP API

Loopback only, on `127.0.0.1:9101`.

| Method | Path           | Auth   |                                                             |
| ------ | -------------- | ------ | ----------------------------------------------------------- |
| `GET`  | `/v1/health`   | none   | `{ name, version, os, arch }`                               |
| `GET`  | `/v1/printers` | bearer | `{ printers: [{ name, isDefault, status, port }] }`         |
| `POST` | `/v1/print`    | bearer | `{ printer, jobName, dataBase64 }` → `{ ok, jobId, bytes }` |

There is **no drawer endpoint**. The pulse is five bytes (`ESC p m 0x19 0xfa`)
that the app already builds; it goes through `/v1/print` like everything else,
so there is one code path to debug rather than two.

Errors come back as `{ error, code }`. The app maps `code` onto a Spanish
sentence — `bad_token` and `origin_not_allowed` have different fixes and look
identical from a failed fetch.

## Security

- **Loopback bind, not configurable.** The listen address is hardcoded; no
  config value can put a printer on the LAN.
- **Origin allowlist**, exact match. A request with no `Origin` — curl, another
  local process, a native app — cannot print. The browser is the only client.
- **Bearer token**, 32 random bytes, generated at install into
  `C:\ProgramData\Thr1vePrint\config.json` (ACL: Administrators and SYSTEM),
  compared in constant time.
- The token authorizes **printing on this one machine** and nothing else. It is
  not a session credential, it grants nothing on the server, and it must never
  be sent anywhere but `127.0.0.1`. It is deliberately unrelated to
  `indor_device_secret`, which stays httpOnly and gates the desk RPCs.

### Private Network Access

A request from an HTTPS page to `127.0.0.1` is a private-network request.
Chrome preflights it with `Access-Control-Request-Private-Network: true`, and
the agent answers `Access-Control-Allow-Private-Network: true`. The client also
passes `targetAddressSpace: 'loopback'` where the browser supports it, which
skips the preflight.

Chrome's Local Network Access work replaces that handshake with a permission
prompt. **Check this on a real terminal before a rollout**: open DevTools on the
production origin and run `fetch('http://127.0.0.1:9101/v1/health')`. A prompt
is fine — it is granted once per origin and persists. A block with no prompt
means the transport has to change, and the escape hatch is polling (the agent
subscribing to a server-side job queue), which is why every call in the app goes
through `src/lib/printing/agent.ts`.

## Developing

The agent runs on macOS and Linux with a **dev sink** in place of a spooler:
every job is written to `~/.thr1ve-print/jobs/<timestamp>.bin`, byte for byte
what the printer would have received.

```bash
go build -o /tmp/thr1ve-agent ./cmd/agent
/tmp/thr1ve-agent run
```

Then print from `/caja` against `pnpm dev`, and turn the captured job back into
an image:

```bash
node decode-job.mjs --latest
```

That is the real check on the raster path — `GS v 0` framing and bit packing are
not things a screenshot of a canvas can confirm.

### Building for Windows

```bash
GOOS=windows GOARCH=amd64 go build -ldflags "-X main.version=1.0.0" \
  -o thr1ve-print-agent.exe ./cmd/agent
```

Zip that beside `install.ps1`. Unsigned binaries trip SmartScreen on first run;
sign the exe if that becomes a support cost.

## Layout

```
cmd/agent/main.go             commands, wiring, foreground and service entry
internal/config               config.json, the token, the origin allowlist
internal/httpapi              the three routes, CORS/PNA, auth
internal/spooler              List / WriteRaw
  spooler_windows.go          winspool.drv syscalls
  spooler_other.go            the dev sink
internal/service              Windows service registration and lifecycle
decode-job.mjs                a captured job -> PNG
install.ps1                   terminal setup
```
