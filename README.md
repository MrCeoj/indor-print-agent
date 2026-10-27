# Thr1ve Print Agent

Its own repo since 1.1.0, copied with its history from
`indor-classes/tools/print-agent/`, which still holds a copy.

Two web apps print through it: **indor-classes** (`/caja`, client in
`src/lib/printing/`) and **indor-analytics**, the club POS (`/pos/caja`, client in
`modules/printing/`). Both build the bytes; this agent only delivers them.

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
the web app** (see above), so changing a receipt never means shipping
a new binary to a counter.

## Install

Download the MSI from the latest release and run it on the terminal:

```
https://github.com/MrCeoj/indor-print-agent/releases/latest/download/thr1ve-print-agent-windows-x64.msi
```

It installs to `C:\Program Files\Thr1vePrint`, registers and starts the
service, and puts the binary on `PATH`. The service mints the terminal token on
its first start. Read it from an elevated prompt and paste it into the printer
panel at `/caja` on that terminal, once:

```powershell
thr1ve-print-agent token
```

Unattended, e.g. over remote management: `msiexec /i thr1ve-print-agent-windows-x64.msi /qn`.

Terminals set up with the old `install.ps1` need nothing special: the MSI takes
over the existing service, and `config.json` (with its token) is left alone.

### If `agent status` reports no printers

The service runs under LocalSystem in **session 0**, which can see machine-wide
printer queues but not ones installed under a single user account. USB receipt
printers normally install machine-wide, so this is rare — but when it happens,
the cashier can print from Word and the agent sees nothing.

Reinstall in the other mode, from an elevated prompt:

```powershell
msiexec /x thr1ve-print-agent-windows-x64.msi
msiexec /i thr1ve-print-agent-windows-x64.msi MODE=LogonTask
```

The mode is remembered, so later upgrades keep it. Switching back is the same
two commands with `MODE=Service`.

That runs the same binary in the cashier's own session at logon. It needs the
terminal to log in automatically, which a till normally does.

One consequence worth knowing: in this mode the agent runs as the cashier, so
the MSI also grants the **Users** group read access to
`C:\ProgramData\Thr1vePrint`. Without it the agent cannot read its own config
and will not start. The token is therefore readable by anyone who can log into
that terminal — acceptable, since it authorizes printing on that machine and the
person logged into it is the one doing the printing, but it is a genuine step
down from Service mode and should not be the default.

### LAN printers

A printer on the venue network works the same as a USB one, as long as it is
installed **in Windows** on the PC: the agent writes to Windows queues and never
to an IP address. The venue router hands out addresses by DHCP, and nobody can
pin them, so the queue must not be pinned to one either:

- Add the printer through *Settings → Printers → Add device* so Windows finds it
  over **WSD**. A WSD port follows the printer when its address changes.
- If it has to be added by hand, use a *Standard TCP/IP port* with the printer's
  **hostname** (printed on its self-test page), not its IP.
- Use the vendor's driver or *Generic / Text Only*. The job goes in RAW either
  way; the driver only has to accept it.

### Print station (tablets)

A waiter's tablet has no agent, so it cannot reach a printer. One PC per venue
becomes a **print station**: the agent on it also polls the app for jobs queued
by tablets and prints them on its own queues. Every connection is outbound
HTTPS. Nothing on the venue network is opened.

1. On the venue screen in the app, *Estaciones de impresión → Agregar*. It shows
   an 8-character code, valid for 15 minutes, once.
2. On the PC, from an elevated PowerShell:

   ```powershell
   & 'C:\Program Files\Thr1vePrint\thr1ve-print-agent.exe' pair https://<app> <code>
   ```

   This stores the station credential in `config.json`, adds the app's origin to
   `allowedOrigins`, and restarts the service.
3. On a tablet, the printer panel picks *Impresora de la sede* by itself when
   no agent answers. Pick the station and the printer.

A tablet's job that is not taken within 90 s expires and never prints. One that
was taken is never handed out again, even if the PC died before printing it. A
relayed job never opens the drawer.

`agent unpair` stops the relay. *Vincular de nuevo* on the venue screen issues a
new code and invalidates the old credential at once, which is also the fix for
a replaced PC.

### Upgrading

Run the new release's MSI, the same as a first install. It replaces the old
version and restarts the service. `config.json` is not one of its files, so the
terminal token pasted in the browser keeps working. Printing pauses for a few
seconds while it runs.

### Uninstalling

*Settings → Apps → Thr1ve Print Agent → Uninstall*, or `msiexec /x` on the MSI.
`C:\ProgramData\Thr1vePrint` stays behind with the token and log; delete it by
hand to start from nothing.

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
msiexec /x thr1ve-print-agent-windows-x64.msi
Remove-Item -Recurse -Force C:\ProgramData\Thr1vePrint
msiexec /i thr1ve-print-agent-windows-x64.msi
```

That mints a **new token**, so paste the new one into `/caja`.

### `agent status` lists no printers

See "If `agent status` reports no printers" under Install — that is the session 0
case, and `MODE=LogonTask` is the answer.

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
| `agent pair <url> <code>` | Become the venue's print station. Restarts the service.             |
| `agent unpair`         | Stop being a print station.                                            |
| `agent logontask`      | Run at each user's logon instead of as a service. The MSI's `MODE=LogonTask`. |

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
  config value can put a printer on the LAN. The print station does not change
  this: it is an outbound client, not a listener.
- **The station credential is a server credential.** Unlike the terminal token,
  `relay.token` lets whoever holds it take that venue's print jobs. It is
  stored only in `config.json`, sent only to the paired server over HTTPS, and
  never logged. Only its sha256 is stored on the server.
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
through the client's `agent.ts` in each app.

## Developing

The agent runs on macOS and Linux with a **dev sink** in place of a spooler:
every job is written to `~/.thr1ve-print/jobs/<timestamp>.bin`, byte for byte
what the printer would have received.

```bash
go build -o /tmp/thr1ve-agent ./cmd/agent
/tmp/thr1ve-agent run
```

Then print from either app's dev server, and turn the captured job back into
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

That is enough to try a build by hand. Releases come from CI.

### Releasing

`.github/workflows/release.yml` builds the exe (with publisher and version in
its file properties, from `winres/winres.json`), packs it into the MSI
(`installer/Product.wxs`, WiX 5), and publishes both as a GitHub release.
Building an MSI needs Windows, so it runs on a Windows runner. To release, tag
and push:

```bash
git tag v1.2.0
git push origin v1.2.0
```

To try an installer change without releasing, run the workflow by hand from the
Actions tab (*Release → Run workflow*). The MSI is attached to that run as an
artifact instead.

Nothing is code-signed yet. Antivirus and SmartScreen treat unsigned
installers with suspicion. The workflow marks where signing goes.

## Layout

```
cmd/agent/main.go             commands, wiring, foreground and service entry
internal/config               config.json, the token, the origin allowlist
internal/httpapi              the three routes, CORS/PNA, auth
internal/relay                the print station: long-poll, print, report
internal/spooler              List / WriteRaw, and Serial (one job at a time)
  spooler_windows.go          winspool.drv syscalls
  spooler_other.go            the dev sink
internal/service              Windows service and logon task registration
installer/Product.wxs         the MSI (WiX 5)
winres/winres.json            publisher and version for the exe's properties
.github/workflows/release.yml build, package, publish
decode-job.mjs                a captured job -> PNG
```
