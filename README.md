# ilo100vnc

HTML5 / VNC remote console for the HP Lights-Out 100 (iLO 100) BMC, replacing
the legacy Java applet. No changes to the BMC firmware are needed.

`ilo100vnc` logs into the iLO web interface, speaks the iLO's proprietary
KVM protocol, and re-exposes the console as:

- an embedded noVNC web client at `http://localhost:8080/`
- standard VNC (RFB 3.8) on a TCP port, usable by any VNC client

One static Go binary. Its only direct dependency is
[go-ipmi](https://github.com/bougou/go-ipmi), for power control.

Developed against a ProLiant DL160 G6, LO100 firmware 4.26, LO100 Advanced
license. The protocol is documented in [docs/PROTOCOL.md](docs/PROTOCOL.md).

## Status

- KVM (video, keyboard, mouse): implemented; video (text mode and VBE
  0x101) and keyboard verified on real hardware.
- Power control: implemented; "BIOS Setup" verified on real hardware.
- Virtual media: not supported (see known limitations).

## Requirements

- An iLO 100 with a license that includes the remote console (LO100 Advanced).
- Network access from the proxy host to the iLO web port (the KVM session uses
  the port announced in `kvms.html`, port 80 on firmware 4.26).
- For power control: IPMI over LAN enabled on the iLO (UDP port 623).
  Without it the power buttons are still shown, but fail.
- To build: Go 1.27+, `make`, `curl`, `tar`, `sha256sum`.

## Build

```sh
make build        # static bin/ilo100vnc, downloads noVNC on first run
GOARCH=arm64 make build  # same, for another architecture
make test         # unit tests
make lint         # golangci-lint and gofumpt (fetched with go run)
```

noVNC (v1.7.0, MPL-2.0) is downloaded by `make novnc`, checksum verified, and
embedded into the binary. It is not committed to this repository; its license
files are copied to `internal/web/novnc/` and embedded alongside. A binary
built without noVNC still serves VNC and the WebSocket endpoint.

## Usage

```sh
export ILO100VNC_PASSWORD='...'
ilo100vnc -host 10.0.0.5 -user admin -vnc localhost:5900
```

Then open `http://localhost:8080/`, or point a VNC client at
`localhost:5900`. The web client listens on localhost by default, and VNC
is off unless `-vnc` is set. Use `-listen :8080` or `-vnc :5900` to serve
on all interfaces.

The iLO session is opened when the first viewer connects and closed
`-idle-timeout` (default 30s) after the last one leaves. Each (re)connect
performs a fresh web login, because the iLO issues a new single session token
on every load of `kvms.html`. All viewers share the one iLO session.

### Options

| Flag | Env (`ILO100VNC_`...) | Default | Meaning |
|------|-----------------------|---------|---------|
| `-config` | | | JSON config file (keys below) |
| `-host` | `HOST` | | iLO host name or address, or an `http://` URL (see limitations) |
| `-user` | `USER` | `admin` | iLO user |
| `-password-file` | `PASSWORD_FILE` | | file with the iLO password |
| | `PASSWORD` | | iLO password (never a flag, to keep it out of `ps`) |
| `-listen` | `LISTEN` | `localhost:8080` | HTTP address for noVNC + WebSocket, empty disables |
| `-vnc` | `VNC` | | VNC address, e.g. `localhost:5900`, empty disables |
| | `VNC_PASSWORD` | | require VNC authentication (max 8 chars, protocol limit) |
| `-idle-timeout` | `IDLE_TIMEOUT` | `30s` | `0` keeps the iLO session open |
| `-relative-mouse` | `RELATIVE_MOUSE` | false | relative instead of absolute mouse reports |
| `-debug` | `DEBUG` | false | debug log with hex dumps of the iLO protocol |

Precedence: flags, then environment, then config file. Config file example:

```json
{
  "host": "10.0.0.5",
  "user": "admin",
  "password_file": "/run/secrets/ilo",
  "vnc": "localhost:5900",
  "vnc_password": "changeme"
}
```

### Power control

noVNC shows a power menu (power icon in the side bar). The commands travel
over the VNC connection (RFB XVP extension) and `ilo100vnc` runs them
through the iLO's IPMI over LAN interface, with the same user and password
as the console:

| Button | IPMI |
|--------|------|
| Shutdown (ACPI) | soft off: ACPI power button, the OS shuts down |
| Power cycle | power off and on |
| Reset | hard reset |
| Power on | power on |
| BIOS Setup | one-time boot flag "BIOS Setup", then power on, or reset if running |

Every button asks for confirmation. "Power on" and "BIOS Setup" are added
to noVNC by `internal/web/ilo.js`, which is injected into noVNC's page; the
vendored noVNC files stay unmodified. Other VNC clients with XVP support
only get the first three (as Shutdown, Reboot, Reset). Failures are only
logged, in the `ilo100vnc` log and the browser console.

### Apache Guacamole

Add a VNC connection to the `-vnc` address (and password, if set).
Guacamole sends X11 keysyms, which are mapped assuming a US layout (see
limitations). Display resizes are signalled with the DesktopSize encoding.

## Security

- The console gives full keyboard access to the server. Without
  `VNC_PASSWORD` anyone who can reach `-listen`/`-vnc` gets it; a warning is
  logged when listening on non-loopback addresses without a password.
  `-listen` defaults to localhost; for remote access prefer an
  authenticating reverse proxy or a VPN over listening on all interfaces.
- VNC authentication (DES challenge) is weak by protocol design; it does not
  encrypt the session. Use a reverse proxy with TLS for browsers, and an SSH
  tunnel or VPN for raw VNC.
- The WebSocket endpoint rejects cross-origin requests.
- Everyone with console access can also power the host off or reset it.
  The VNC password protects both.
- The connection to the iLO is plain HTTP and unencrypted KVM, as the iLO
  offers no usable TLS (see limitations). Keep the iLO and the proxy on an
  isolated management network and expose only the proxy.

## Debugging and reporting

Run with `-debug` and reproduce the problem:

```sh
ILO100VNC_PASSWORD=... ilo100vnc -host 10.0.0.5 -debug 2> ilo100vnc.log
```

The log contains hex dumps of every message sent to the iLO (`tx`) and of the
first 256 bytes of every message received (`rx`). The session token in the
handshake dump (`tx 0xDD`) is masked with `*`; the iLO password is never
logged. Host names and user names do appear in the log.

Every 2 seconds the debug log also contains a `video summary` line: the
video mode, how many text and graphics updates, tiles, palettes and cursor
messages arrived, the text font size and the cursor scanlines, or `quiet`
when nothing arrived. This shows which boot stages the iLO captures.

## Known limitations

- Only partly tested on real hardware (text mode, VBE 0x101 640x480 in BIOS
  Setup, keyboard); the mouse and larger graphics modes are unverified.
- The host's mouse pointer is a Matrox hardware cursor that is not part of the
  video stream. Viewers get a local arrow pointer instead, which matches the
  host pointer in absolute mouse mode. If the host OS does not support the
  absolute (tablet) device, use `-relative-mouse`; pointer alignment is then
  approximate.
- Keyboard: noVNC sends layout-independent scancodes (best). Clients that only
  send keysyms (Guacamole, most VNC viewers) are mapped as a US keyboard; with
  other layouts, set the same layout on the server OS and client, or expect
  wrong symbols for non-letter keys.
- Only Raw and Zlib encodings; expect high bandwidth on large graphics modes.
- Only one iLO session is used for all viewers. The iLO starts every session
  view-only; full control is requested automatically when no other user is
  connected. If another user (e.g. the Java applet) is connected, input stays
  disabled and a warning is logged.
- POST and the GRUB menu are only shown on some boots (roughly 1 in 7 on a
  DL160 G6 with firmware 4.26). On the other boots the iLO stays in text
  mode until the OS loader runs. This is a limitation of the iLO firmware;
  the original Java viewer shows the same.

  To get into BIOS Setup reliably, use the "BIOS Setup" button, or set the
  one-time IPMI boot flag yourself and boot the host:

  ```sh
  ipmitool -I lanplus -H <ilo> -U admin -f <password-file> chassis bootdev bios
  ipmitool -I lanplus -H <ilo> -U admin -f <password-file> chassis power on
  ```

  Use `power cycle` or `power reset` if the host is running. Inside Setup,
  F10 means "Save and Exit".
- No TLS towards the iLO. Firmware 4.26 only offers SSLv3/TLS 1.0 with a
  single cipher suite, `TLS_DHE_DSS_WITH_3DES_EDE_CBC_SHA` (1024-bit DH, a
  generic expired 1024-bit DSA certificate). Go supports neither DHE nor DSA,
  and the iLO's TLS would not add real security anyway. `https://` URLs
  are rejected, and an iLO configured for encrypted KVM (`kvmssl`) fails with
  a clear error.
- Virtual media (mounting ISOs) is deliberately not supported: the iLO opens
  the data connection back to the client, which typical setups (iLO behind a
  jump host or NAT) do not allow. For OS installs use PXE or a USB stick.

## Layout

```
cmd/ilo100vnc/    flags, config, wiring
internal/ilo/     web login, KVM protocol, video decoder, keymaps, session lifecycle
internal/ipmi/    power control through IPMI over LAN
internal/rfb/     RFB server
internal/web/     embedded noVNC, power buttons for noVNC (ilo.js), WebSocket bridge
docs/             KVM protocol
```

## Legal

ilo100vnc is licensed under the MIT License, see [LICENSE](LICENSE).

Interoperability project for owned hardware. No HP code is included; the
protocol documentation was derived from analysis of the vendor's viewer.
noVNC is licensed under MPL-2.0 (see `internal/web/novnc/LICENSE.txt` after
`make novnc`), go-ipmi under Apache-2.0.
