# ilo100vnc

HTML5 / VNC remote console for the HP Lights-Out 100 (iLO 100) BMC, replacing
the legacy Java applet. No changes to the BMC firmware are needed.

`ilo100vnc` logs into the iLO, speaks its proprietary KVM protocol
([docs/PROTOCOL.md](docs/PROTOCOL.md)) and serves the console as:

- an embedded noVNC web client
- standard VNC (RFB), e.g. for VNC clients or Apache Guacamole

It also adds power control (on, off, reset, boot into BIOS Setup) through
IPMI. Developed against a ProLiant DL160 G6 with LO100 firmware 4.26.

## Requirements

- An iLO 100 with the remote console licensed (LO100 Advanced)
- HTTP access to the iLO; IPMI over LAN (UDP 623) for power control
- To build: Go 1.27+, `make`, `curl`, `tar`, `sha256sum`

## Build

```sh
make build               # bin/ilo100vnc, downloads noVNC on first run
GOARCH=arm64 make build  # for another architecture
make test
make lint
```

## Usage

```sh
export ILO100VNC_PASSWORD='...'
ilo100vnc -host 10.0.0.5 -vnc localhost:5900
```

Open `http://localhost:8080/`, or point a VNC client at `localhost:5900`.
Here both listen on localhost only; use e.g. `-listen :8080` to serve on all
interfaces.

| Flag | Env | Default | Meaning |
|------|-----|---------|---------|
| `-config` | | | JSON config file (keys below) |
| `-host` | `ILO100VNC_HOST` | | iLO host name or address |
| `-user` | `ILO100VNC_USER` | `admin` | iLO user |
| `-password-file` | `ILO100VNC_PASSWORD_FILE` | | file with the iLO password |
| | `ILO100VNC_PASSWORD` | | iLO password |
| `-listen` | `ILO100VNC_LISTEN` | `localhost:8080` | web client address, empty disables |
| `-vnc` | `ILO100VNC_VNC` | | VNC address, empty disables |
| | `ILO100VNC_VNC_PASSWORD` | | VNC password (max 8 chars) |
| `-idle-timeout` | `ILO100VNC_IDLE_TIMEOUT` | `30s` | close the iLO session after the last viewer left, `0` never |
| `-relative-mouse` | `ILO100VNC_RELATIVE_MOUSE` | false | relative mouse reports |
| `-debug` | `ILO100VNC_DEBUG` | false | debug log with protocol hex dumps |

Flags override the environment, which overrides the config file:

```json
{
  "host": "10.0.0.5",
  "password_file": "/run/secrets/ilo",
  "vnc": "localhost:5900",
  "vnc_password": "changeme"
}
```

### Power control

The noVNC side bar has a power menu. Every button asks for confirmation.

| Button | Action |
|--------|--------|
| Shutdown (ACPI) | ACPI power button, the OS shuts down |
| Power cycle | power off and on |
| Reset | hard reset |
| Power on | power on |
| Power off | hard power off |
| BIOS Setup | boot into BIOS Setup once (powers on, or resets if running) |

Other VNC clients with XVP support only get the first three.

## Security

- Console access means full control of the server, including power. Set
  `VNC_PASSWORD` and keep the listeners on localhost; for remote access use a
  reverse proxy with TLS and authentication, an SSH tunnel or a VPN.
- VNC authentication is weak and does not encrypt.
- The iLO only speaks plain HTTP and unencrypted KVM (its TLS is SSLv3/TLS 1.0
  with DHE-DSS, which Go does not support). Keep it on an isolated network.

## Known limitations

- POST and the GRUB menu are only shown on some boots; this is an iLO
  firmware limitation. Use the "BIOS Setup" button to get into Setup.
- The host's mouse pointer is not part of the video stream; viewers get a
  local pointer instead.
- Keysym-only clients (Guacamole, most VNC viewers) are mapped as a US
  keyboard. noVNC sends layout-independent scancodes.
- Screen updates are sent uncompressed (Raw) or deflate-compressed (Zlib)
  only, without Tight, ZRLE or JPEG, so large graphics modes need a lot of
  bandwidth.
- All viewers share one iLO session. If another user (e.g. the Java applet)
  holds full control, input stays disabled.
- No virtual media (mounting images). The iLO does not carry disk data over
  the console connection; it opens its own TCP connection to an address the
  client announces. That makes it cumbersome to use, e.g. the iLO would have
  to reach the ilo100vnc host, which fails behind NAT, jump hosts or
  firewalls.

## License

MIT, see [LICENSE](LICENSE). No HP code is included; the protocol
documentation was derived from analysis of the vendor's viewer. noVNC
(MPL-2.0) is downloaded at build time and embedded with its license;
go-ipmi is Apache-2.0.
