# iLO 100 ("Mahogany") Remote Console Protocol

Description of the KVM protocol spoken between the HP Lights-Out 100 BMC
(ServerEngines Pilot II) and its Java viewer `M2.JAR`
(`com.serverengines.mahogany.MahoganyViewer`, viewer version 2.0.0 build 11811).

The viewer is served by the iLO itself at `/M2.JAR` (LO100 firmware 4.26,
internal build V9.82). Class references below are relative to
`com.serverengines.` in the decompiled viewer.

Status markers:
- **[code]** derived directly from the decompiled viewer
- **[fw]** derived from strings in the firmware image
- **[unverified]** inferred, needs confirmation on real hardware

## 1. Overview

```
 browser/applet                         iLO 100 (Pilot II)
 --------------                         ------------------
 HTTP(S) GET kvms.html  ----------->    web server (Basic/Digest auth)
                        <-----------    HTML with <APPLET> + PARAMs
 TCP connect NonSecure_KVMPort ---->    KVM server (plain TCP or SSL)
 binary "Mahogany" protocol <------>    video out, keyboard/mouse in
```

There is no RFB/VNC framing. The viewer reuses some class names from the
RealVNC Java viewer (`kvm/CConnection`, `rdr/InStream`) but the wire protocol
is proprietary.

## 2. Web login and applet parameters

### 2.1 Authentication [fw]

The embedded web server (Interpeak based, `qlgchttp.c`) uses plain HTTP
authentication. Strings present: `WWW-Authenticate: Basic`,
`WWW-Authenticate: Digest`, `realm="%s", qop="auth", nonce="%lx"`,
`md5-sess`, `auth-int`. There are no cookies: every request carries Basic or
Digest credentials. The KVM session itself is authorized by the `httpdata`
token from the KVM page (2.4).

### 2.2 The KVM page [fw]

The page `kvms.html` (linked as "Start %s Session" from `kvm_session.html`)
renders this template:

```html
<APPLET CODE="com.serverengines.mahogany.MahoganyViewer.class" mayscript="true"
        ARCHIVE = "M2.JAR" WIDTH="400" HEIGHT="250" ...>
<PARAM NAME="NonSecure_KVMPort" VALUE="%u">
<PARAM NAME="sessiontype"       VALUE="%s">   <!-- "kvm" or "kvmssl" -->
<PARAM NAME="port"              VALUE="%s">   <!-- storage server port -->
<PARAM NAME="ipaddress"         VALUE="%s">
<PARAM NAME="sessiontype"       VALUE="%s">
<PARAM NAME="httpdata"          VALUE="%s">
<PARAM NAME="username"          VALUE="%s">
</APPLET>
```

Static files served by the BMC: `/M2.JAR`, `/M2.EXE` (Windows storage
server), `/M2.E` (Linux storage server), `/M2.R` (resource file).

### 2.3 Parameter semantics [code]

`mahogany/MahoganyViewer#start`, `mahogany/CConn#init`:

| PARAM               | Use |
|---------------------|-----|
| `ipaddress`         | host to connect the KVM socket to |
| `NonSecure_KVMPort` | TCP port of the KVM socket (used for both `kvm` and `kvmssl`) |
| `sessiontype`       | `kvmssl` (case-insensitive) = wrap the socket in SSL, anything else = plain TCP |
| `username`          | sent in the client handshake |
| `httpdata`          | sent in the client handshake in the *password* fields |
| `port`              | local storage server port (default 5901), virtual media only |
| `NonSecure_KMPort`, `NonSecure_VPort`, `SSL_VMPort`, `SSL_KPort` | parsed and logged, otherwise unused by this viewer build |

Defaults from `app.properties` if a PARAM is missing: `kvms.port=80`,
`keyboard.port=5902`, `keyboard.ssl.port=5904`, storage `5901`.

### 2.4 Observed on hardware (firmware 4.26 page, fetched via plain HTTP)

```
NonSecure_KVMPort = 80          same port as the web server
sessiontype       = kvm         plain TCP, no SSL
port              = 5901
ipaddress         = <BMC hostname>
username          = admin
httpdata          = <hash-like value, not the account password>
```

- The KVM session shares TCP port 80 with the HTTP server; the BMC must tell
  them apart by the first bytes (HTTP verb vs. `0xD2` ClientNOP).
- `httpdata` is a server-generated value, so the web login is required: fetch
  `kvms.html` with HTTP auth, extract `httpdata`, and send it in the
  ClientHandshake password fields with the embedded signature `0x5A5A5A5A`.
- `httpdata` is 96 hex characters (48 bytes, opaque) and changes on every load
  of `kvms.html`. Sent as ASCII exactly like the viewer does: first 20
  characters in the 20-byte password field, all 96 in the 128-byte full
  password field. Fetch a fresh one before every KVM connect.
  **[unverified]** whether it is single use or expires.

### 2.5 Observed KVM session (firmware 4.26, LO100 Advanced)

- ServerHandshake version string `MAHOGANY KVM`; FirmwareVersion
  `V9.82  Build Number: 4.26 (Testing)`.
- The first MultiUserState has flags `0x00101EEF`: status view-only (bits 3-4
  = 1) although no other user is connected, user0 flags `0x81` (valid, self,
  no full control). The client must send `RequestPrimaryControl` (0xD3 00)
  to get input; the original viewer asks the user first.
- Text mode (BIOS, GRUB, Linux console) arrives as InformVesaMode
  `mode 3, 640x400, bpp 0`, then full-screen BitBlt type 0x107 (12216 bytes)
  roughly every 820 ms while the screen changes.
- During part of POST the iLO may report InformVesaMode `mode 0x200, 8x1,
  bpp 8`, followed by many SetPalette (64 attributes, 256 colors) and no
  BitBlt/EnhanceBitBlt at all: the iLO cannot determine the host's video
  mode and reports a size computed from zero CRTC registers.
- On most boots the iLO reports text mode 3 at power-on but sends no BitBlt
  during POST and the GRUB menu, only SetTextCursor/SetPalette; text starts
  with the kernel loader. The original viewer shows the same blank screen,
  so this is a firmware limitation, not a client issue.
- StandbyPower (0xE4) is sent around mode switches and while the host is off.

SSL mode: `SSLContext.getInstance("SSL")` with a trust-all manager
(`mahogany/ServerEgninesTrustManager`). The BMC uses the Interpeak IPCRYPTO
SSL stack.

Measured on firmware 4.26 (HTTPS port 443, raw ClientHello probes):

- Protocols: SSLv3 and TLS 1.0; a TLS 1.1/1.2 ClientHello is answered
  with TLS 1.0.
- Exactly one cipher suite: `TLS_DHE_DSS_WITH_3DES_EDE_CBC_SHA` (0x0013),
  1024-bit DH group.
- Certificate: DSA 1024-bit, dsaWithSHA1, self-signed
  `CN=pilotchip.com, OU=pilot` (ServerEngines), valid 2002-2004. Likely the
  same on every device with this chip.
- `kvmssl` presumably uses the same stack **[unverified]**. Go's crypto/tls
  supports neither DHE nor DSA, so the proxy only speaks plain HTTP and KVM.

## 3. Transport and framing [code]

- One TCP (or SSL) connection carries everything, in both directions.
- Socket read timeout 20 ms, but timeouts are just retried
  (`rdr/JavaInStream#read`).
- **All integers are little-endian** (`buffer/LittleEndianBufferMgr`,
  `rdr/InStream`).
- A message is `u8 id` followed by an id-specific body. There is **no length
  prefix**: the body length is implied by the id and by fields inside the body.
  An unknown id is fatal (`mahogany/MessageReceiverThread#getProtocol` throws).
- Strings are raw bytes (Java `String.getBytes()`, effectively ASCII/Latin-1),
  not NUL terminated unless padded.
- No stream cipher, XOR, or checksum at the application layer. The only
  encryption is the optional SSL wrapper. (`InformHLevelCompression` has a field
  named `m_isEncrypted`, but it only toggles RLE compression, see 6.2.)

Type notation below: `u8/u16/u32` unsigned, `i16/i32` signed, `bool` = u8 0/1.

## 4. Session setup

### 4.1 Sequence [code]

`mahogany/CConn#init`, `#onServerHandshake`, `#onMultiUserState`:

```
C: connect TCP (optionally SSL handshake)
C: 0xD2 ClientNOP                       (immediately after connect)
S: 0xC8 ServerHandshake                 (version + privilege letters)
C: 0xDD ClientHandshake                 (username, password, requested rights)
S: 0xC9 FirmwareVersion                 (order of C9/C5 not enforced by client)
S: 0xC5 MultiUserState                  (rights + user table; "handshake complete")
C: 0xB1 ClientAbsoluteMode / 0xB2 ClientRelativeMode   (mouse mode)
C: 0xC1 RequestKeyIndicators            (only if user has full control)
S: 0xE1 InformVesaMode                  (current video mode)
C: 0xF2 Invalidate (full screen)        (sent after every mode change)
S: 0xE6 / 0xE2 / 0xE3 / ...             (palette + screen updates)
```

The client only sends input/video requests after `MultiUserState` has been
received (`MessageSender#isHandshakeComplete`). `RequestVesaMode` (0xF1) is the
"refresh" action and can be sent any time to make the server re-announce the
mode.

### 4.2 ServerHandshake (S->C, 0xC8) [code]

`mahoganyprotocol/ServerHandshake#readBuffer`

| Offset | Type      | Field |
|--------|-----------|-------|
| 0      | u8[32]    | ASCII, e.g. `"<version> kvms"`. Non-identifier chars become spaces, then trimmed. |

Everything after the first space is a set of privilege letters:
`k` keyboard, `v` video, `m` mouse, `s` storage. The client stores these and
gates its own messages on them.

### 4.3 ClientHandshake (C->S, 0xDD) [code]

`mahoganyprotocol/ClientHandshake#writeBuffer`,
`mahogany/MessageSender#sendClientHandshake`, padding in `mahogany/CConn#init`.

| Offset | Type     | Field |
|--------|----------|-------|
| 0      | u32      | signature: `0x5A5A5A5A` embedded applet (params came from the web page), `0x12121212` standalone (user typed host/user/password in the viewer's own dialog) |
| 4      | u32      | username length = 16 |
| 8      | u32      | password length = 20 |
| 12     | u32      | config length = 4 |
| 16     | u32      | key length = 0 |
| 20     | u8[16]   | username, NUL padded / truncated to 16 |
| 36     | u8[20]   | password, NUL padded / truncated to 20 |
| 56     | u32      | requested rights, bitmask: 1 video, 2 mouse, 4 keyboard, 8 storage0, 16 storage1. The viewer always sends `0x1F`. |
| 60     | u8[0]    | key (always empty) |
| 60     | u8[128]  | "full" password, NUL padded / truncated to 128 |

Total body 188 bytes, message 189 bytes.

The web UI has an IPMI-style "PasswordSize 16/20 byte" option, which
explains the 20-byte field; the 128-byte field allows longer passwords.
Embedded mode (`0x5A5A5A5A`) with `httpdata` from `kvms.html` works on
firmware 4.26. Standalone mode with the account password was not tested.

### 4.4 FirmwareVersion (S->C, 0xC9) [code]

| Type        | Field |
|-------------|-------|
| u32         | length N |
| u8[N]       | version string (also used as a directory name for the storage helper) |

### 4.5 MultiUserState (S->C, 0xC5) [code]

`mahoganyprotocol/MultiUserState#readBuffer`

| Type      | Field |
|-----------|-------|
| u32       | flags (below) |
| u8        | user0 name length L0 |
| u8        | user1 name length L1 |
| u8        | user0 flags |
| u8        | user1 flags |
| u8[L0]    | user0 name |
| u8[L1]    | user1 name |

Session flags:

| Bit(s)    | Mask       | Meaning |
|-----------|------------|---------|
| 0         | 0x00000001 | allowed to take full control |
| 1         | 0x00000002 | allowed to disconnect other users |
| 2         | 0x00000004 | allowed to relinquish full control |
| 3-4       | 0x00000018 | this user's status: 0 full control, 1 view only, 2 ephemeral, 3 forcibly disconnected (client then disconnects) |
| 5         | 0x00000020 | right: turn local screen on |
| 6         | 0x00000040 | right: turn local screen off |
| 7         | 0x00000080 | right: change local monitor setting |
| 8         | 0x00000100 | storage allowed |
| 9         | 0x00000200 | storage write allowed |
| 10        | 0x00000400 | DVD-ROM device allowed |
| 11        | 0x00000800 | memory (USB stick) device allowed |
| 12        | 0x00001000 | client-side inactivity timeout enabled |
| 13        | 0x00002000 | remote (network) storage allowed |
| 14        | 0x00004000 | force RLE |
| 15        | 0x00008000 | local monitor is on |
| 16-19     | 0x000F0000 | local monitor setting: 0 always off, 1 always on, 2 on when disconnected, 3 off when disconnected, 4 leave as is |
| 20        | 0x00100000 | use alternate ("alt 0") UI texts |
| 21        | 0x00200000 | storage enabled |
| 22        | 0x00400000 | force high-level compression state |
| 23        | 0x00800000 | use native EXE for KVM |
| 24        | 0x01000000 | use native EXE for storage |

User flags (`mahoganyprotocol/User`): bit0 valid, bit1 has full control,
bit7 "this is you".

Only one user can have full control. Up to 2 concurrent sessions.

### 4.6 ServerDisconnect (S->C, 0xC6) [code]

| Type    | Field |
|---------|-------|
| i32     | reason |
| u16     | message length N |
| u8[N]   | message |

### 4.7 Keepalive / timeouts [code]

The viewer sends no periodic keepalive. `ClientNOP` is only sent once at
connect. The 15-minute idle timeout (`settings.properties: session.timeout`)
is enforced *client side* (`mahogany/MasterThread`) and only when flag
0x1000 is set. **[unverified]** whether the BMC has its own idle timeout. If it
does, a periodic `RequestKeyIndicators` or `ClientNOP` is the obvious
candidate for a keepalive.

## 5. Client -> server messages [code]

Source: `mahogany/MessageSender`, `mahoganyprotocol/*#writeBuffer`.

| Id   | Name                    | Body |
|------|-------------------------|------|
| 0x99 | StorageClientConnect    | virtual media, not documented here |
| 0x9A | StorageClientDisconnect | u8 0 |
| 0xB1 | ClientAbsoluteMode      | bool enable |
| 0xB2 | ClientRelativeMode      | u8: bit0 relative, bit4 hide cursor |
| 0xB3 | ButtonStateAtAbsolute   | i16 x, i16 y, u8 n (=3), u8 button[n] |
| 0xB4 | ButtonStateAtRelative   | i16 dx, i16 dy, u8 n (=3), u8 button[n] |
| 0xB5 | MouseMove               | i16 x, i16 y (absolute pixel, or delta in relative mode) |
| 0xC1 | RequestKeyIndicators    | u8 0 |
| 0xD1 | KeyStateChange          | u16 usage, bool pressed, bool pretranslated (always 0) |
| 0xD2 | ClientNOP               | u8[7] zero |
| 0xD3 | RequestPrimaryControl   | u8 0 |
| 0xD6 | DisconnectUser          | u8 user index |
| 0xD7 | RelinquishFullControl   | u8 0 |
| 0xD8 | ClientDisconnect        | i32 reason (0 ok, 1 force quit, 2 exception), u32 len N, u8[N] message |
| 0xD9 | KeyboardTurnScreenOn    | u8 0 |
| 0xDA | KeyboardTurnScreenOff   | u8 0 |
| 0xDD | ClientHandshake         | see 4.3 |
| 0xF1 | RequestVesaMode         | u8 0 |
| 0xF2 | Invalidate              | u16 n, n x Region |
| 0xF3 | InformHLevelCompression | bool enable RLE ("HLC") |

`Region` = `i16 x, i16 y, i16 width, i16 height` (8 bytes).

### 5.1 Keyboard [code]

`mahogany/CConn#onKeyPressed/#onKeyReleased`, `resmgr/KeyMappingResMgr`,
`keymappings-*.properties`.

The key code is a **USB HID Usage ID** from the Keyboard/Keypad page (0x07),
e.g. 0x04 = A, 0x28 = Enter, 0xE0 = Left Ctrl, 0xE6 = Right Alt. The layout
files map Java key codes to these usages, so the server receives positional
keys and the host OS applies its own layout. One message per press and per
release.

Example, press "A": `D1 04 00 01 00`.

Lock state: `InformKeyIndicators` (0xD5) reports host LED state; the viewer
resyncs local locks. For a VNC bridge this can be ignored or exposed via the
RFB LED pseudo-encoding.

### 5.2 Mouse [code]

`mouse/MouseMgr#sendMouseState`, `mouse/MouseButtons`,
`mahoganyprotocol/ButtonState`, `mouse/MouseAbsoluteDelegate`.

Modes (`settings.properties: mouse.mode`, default 2):

| mode | 0xB1 ClientAbsoluteMode | 0xB2 ClientRelativeMode |
|------|-------------------------|-------------------------|
| 0 hide     | false | 0x11 |
| 1 absolute | true  | 0x00 |
| 2 relative | false | 0x01 |

Absolute mode coordinates are framebuffer pixels (0..width-1, 0..height-1),
not scaled to 0..32767. For a VNC bridge, absolute mode maps 1:1 to RFB
PointerEvents.

Button array (always 3 entries):

| Index | Button |
|-------|--------|
| 0     | left (Java button 1) |
| 1     | right (Java button 3) |
| 2     | middle (Java button 2), also carries the wheel |

Button byte: bit0 = pressed, bits 1-7 = wheel position. Default byte is `0x80`
(wheel = 64, centered, not pressed). A wheel event sets entry 2's wheel field to
`64 + rotation` (rotation is the signed notch count, positive = down).

Mouse moves without a button change use 0xB5. Button changes and wheel use
0xB3 (or 0xB4 in relative mode) carrying the current position.

Example, left click at (100,50): `B3 64 00 32 00 03 81 80 80`, then
`B3 64 00 32 00 03 80 80 80`.

## 6. Server -> client messages [code]

Source: `mahogany/MessageReceiverThread`, `mahoganyprotocol/*#readBuffer`.

| Id   | Name                 | Body |
|------|----------------------|------|
| 0x89 | StorageStatus        | virtual media, not documented here |
| 0xC5 | MultiUserState       | 4.5 |
| 0xC6 | ServerDisconnect     | 4.6 |
| 0xC8 | ServerHandshake      | 4.2 |
| 0xC9 | FirmwareVersion      | 4.4 |
| 0xD5 | InformKeyIndicators  | bool caps, bool num, bool scroll |
| 0xE1 | InformVesaMode       | i16 mode, i16 width, i16 height, i16 bpp |
| 0xE2 | BitBlt               | 6.3 |
| 0xE3 | EnhanceBitBlt        | 6.2 |
| 0xE4 | StandbyPower         | u8[15] padding (host is off: show black 640x480) |
| 0xE6 | SetPalette           | 6.5 |
| 0xEA | SetTextCursor        | u8 col, u8 row, u8 startLine, u8 stopLine (swap if stop < start; start == stop means hidden) |
| 0xEB | SpecialGraphicsBit   | u32 bits, 6.6 |
| 0xEC | MatroxGraphicsCursor | u8[256] control, 64 x u8[48] planes, 6.7 |
| 0xEE | GraphicsRegisterValue| 17 x u32 (5 CRTC, 8 ATTR, 2 CRTCEXT, 2 SEQ), read and ignored by the viewer |

### 6.1 Video modes and framebuffer [code]

`graphics/ScreenModes#getScreenMode`, `mahogany/PixelBufferImage#setVesaMode`,
`graphics/GraphicMode#getColorModel`.

`InformVesaMode.mode` selects text vs graphics:

| mode       | Kind | Text grid |
|------------|------|-----------|
| 0, 2, 3, 7 | text | 80 x 25, 8x16 font |
| 1          | text | 40 x 25, 8x16 font |
| 0x10A      | text | 132 x 43, 8x16 font |
| other      | graphics, `bpp` from the message |

`width`/`height` are the pixel dimensions of the framebuffer in all cases.

Pixel formats for graphics modes:

| bpp | Internal | Wire bytes per pixel | Meaning |
|-----|----------|----------------------|---------|
| 4   | 8-bit index via attribute table | nibble | see 6.4 |
| 8   | 8-bit palette index | 1 | `SetPalette` RGB |
| 15  | 16 | 2 (LE) | RGB555 |
| 16  | 16 | 2 (LE) | RGB565 |
| 24  | 24 | 3 | byte0 = B, byte1 = G, byte2 = R |
| 32  | 32 | 4 | byte0 = B, byte1 = G, byte2 = R, byte3 ignored |

Before any `SetPalette` the 8-bit palette defaults to RGB 3:3:2
(`r = (i&7)*255/7`, `g = ((i>>3)&7)*255/7`, `b = ((i>>6)&3)*255/3`,
`graphics/PaletteMgr`).

After a mode change the client sends `Invalidate` for the full screen
(`mahogany/DesktopWindow#setVesaWinSize`).

### 6.2 EnhanceBitBlt (0xE3): tiled updates [code]

The main graphics-mode update. `mahoganyprotocol/EnhanceBitBlt#readBuffer`,
`graphics/GraphicsMgr#enhanceBitBlt`, `mahogany/PixelBufferImage`.

| Offset | Type        | Field |
|--------|-------------|-------|
| 0      | i16         | blt type |
| 2      | u8          | tile width TW (power of two) |
| 3      | u8          | tile height TH (power of two) |
| 4      | u32         | triplet code (RLE escape, HLC only) |
| 8      | u32         | repeat code (RLE escape, HLC only) |
| 12     | u32         | raw size (informational) |
| 16     | u32         | scrunch size S = payload length |
| 20     | 64 x (u32 lo, u32 hi) | dirty-tile bitmap, interleaved per tile row |
| 532    | u8[S]       | payload |

Blt types (`mahoganyprotocol/BiltType`); bit 15 set = "new frame" marker
(used only for an FPS counter):

| Value            | Decoder |
|------------------|---------|
| 0x01F0 / 0x81F0  | raw tiles |
| 0x01F2 / 0x81F2  | HLC (RLE) tiles |
| 0x01F3 / 0x81F3  | 4bpp tiles |

**Tile walk** (same for all three): tiles cover the framebuffer from the top
left, `cols = ceil(width/TW)`, `rows = ceil(height/TH)`, each clamped to 64.
For tile row `ty` in `0..rows-1`, tile column `tx` in `0..cols-1`, the tile is
present if bit `tx` of `lo[ty]` (tx < 32) or bit `tx-32` of `hi[ty]` is set.
Present tiles appear in the payload in row-major order (ty outer, tx inner).
Each tile is always a full TW x TH block, scanned row by row; pixels that fall
outside the framebuffer are decoded and discarded.

**Raw** (0x1F0): each pixel is `bpp/8` bytes as in 6.1 (8bpp: 1 byte index).
A tile scanline that lies below the framebuffer still occupies `TW * bpp/8`
bytes in the payload.

**HLC** (0x1F2): payload starts with plane lengths, then byte planes:

```
bpp > 8:   u32 len0, u32 len1, [u32 len2 if bpp > 16], plane0, plane1, [plane2]
bpp == 8:  plane0 only (no length header)
```

plane0 = low byte of each pixel (blue for 24/32bpp), plane1 = middle byte,
plane2 = high byte (red). For 16bpp pixel = `plane1 << 8 | plane0` (RGB565).
Each plane is an independent RLE byte stream over all pixels of all present
tiles in tile walk order. Decoding one plane, with `T` = triplet code and
`R` = repeat code:

```
need a new run when count == 0:
  b = next()
  if b == T:  count = 3;  value = next()
  elif b == R:
      n = next()
      if n == 1:   count = 1; value = T        (escaped literal T)
      elif n == 0: count = 1; value = R        (escaped literal R)
      else:        count = n + 1; value = next()
  else:        count = 1;  value = b
emit value; count -= 1
```

**4bpp** (0x1F3): payload is one nibble per pixel, consecutive pixels share a
byte; within each tile row, pixel at even x uses the low nibble, odd x the high
nibble (`index >> 1` over the running pixel counter). Values go through the
attribute table (6.4).

After an `EnhanceBitBlt` the client marks the bounding box of present tiles as
dirty. When using 0x1F2, the viewer flags "HLC on"; the client can request it
via `InformHLevelCompression(true)`.

### 6.3 BitBlt (0xE2): rectangles and text [code]

`mahoganyprotocol/BitBlt#readBuffer`, `graphics/GraphicsMgr#bitBlt`,
`mahogany/PixelBufferImage#bitBlt`, `graphics/TextMode#imageRect`.

| Offset | Type   | Field |
|--------|--------|-------|
| 0      | i16    | blt type |
| 2      | u8     | font height |
| 3      | u8     | font width |
| 4      | Region | source rectangle (unused by the viewer) |
| 12     | Region | destination rectangle x, y, w, h |
| 20     | u32    | data size N |
| 24     | u8[N]  | data |

| Blt type | Meaning |
|----------|---------|
| 0x100 BIT_BLT_COPY | graphics rectangle, `w*h` pixels in the 6.1 format, row-major. For bpp 4: nibbles, see note below. |
| 0x101..0x107 | text; bit0 = character codes, bit1 = attributes, bit2 = font |
| 0x108 | text, interleaved `char, attr` pairs for the whole screen |
| 0x1F3 | 4bpp "special" rectangle |

Graphics 4bpp `BitBlt`: within a row the loop counts `k` from `w-1` down to 0;
the byte is `data[i >> 1]` with `i` the running nibble counter; when `k` is even
the high nibble is used, otherwise the low nibble.

**Text rectangles** (0x101-0x107): x, y, w, h are in character cells. Data is
the concatenation of the selected sections, in order:

1. if bit0: `w*h` character codes (row-major within the rectangle)
2. if bit1: `w*h` attribute bytes
3. if bit2: font table, the rest of the data (up to 64 KiB): 32 bytes per
   glyph, one byte per scanline, glyph `c` at offset `c*32`

Character and attribute tables persist between messages; the viewer always
copies into them starting at index 0 and indexes them as `row * w + col`
relative to the rectangle. For 0x108 it fills `cols*rows` pairs.

**Rendering a cell** (`TextMode#imageRect`):

```
fg = attr & 0x0F           (attr & 0x07 if DBE swap active)
bg = attr >> 4 & 0x0F      (attr >> 4 & 0x07 if blinking enabled)
if blinking enabled and attr & 0x80: fg = bg   (blink "off" phase)
glyph = c (+ primary offset if attr & 0x08, else secondary offset, when DBE swap)
for each scanline s in 0..fontHeight-1:
  bits = font[glyph*32 + s]
  for px in 0..fontWidth-1:          (MSB first: bit fontWidth-1-px)
    pixel = fg if bit set or (cursor here and startLine <= s <= stopLine) else bg
```

The resulting 4-bit value is an attribute-color index, converted to RGB through
the attribute and palette tables (6.4). Blinking text toggles every 500 ms in
the viewer (`MasterThread`).

### 6.4 Attribute table and 4-bit colors [code]

`kvm/PixelBuffer#convertAttributesToPalette`, `graphics/AttributeMgr`.

For text modes and 4bpp graphics, each pixel holds a 4-bit value `v`.
Displayed palette index:

```
p = attr[v | colorIndex << 4]                 colorIndex from SpecialGraphicsBit
if special4bpp and attr[16] & 0x80:          (VGA attribute mode control, P54S)
    p = (p & 0xCF) | (attr[20] & 3) << 4      (VGA color select)
rgb = palette[p]
```

Text mode defaults before `SetPalette`: palette `[0]=black, [1]=white`,
attributes identity.

### 6.5 SetPalette (0xE6) [code]

| Type    | Field |
|---------|-------|
| u16     | attribute entry count A |
| A x {u8 index, u8 value} | attribute table updates (table size 64) |
| u16     | palette entry count P |
| P x {u8 index, u8 red, u8 green, u8 blue} | palette updates (8-bit components) |

### 6.6 SpecialGraphicsBit (0xEB) [code]

u32 bit field:

| Bits | Meaning |
|------|---------|
| 0    | blinking text enabled |
| 1    | DBE swap (dual character sets) |
| 2-4  | primary character map select |
| 5-7  | secondary character map select |
| 8-9  | 4bpp color index (attribute table bank) |

Character map select values map to glyph offsets
`[0, 512, 1024, 1536, 256, 768, 1280, 1792]` (glyph units, VGA character map
select semantics).

### 6.7 MatroxGraphicsCursor (0xEC) [code]

`mahoganyprotocol/MatroxGraphicsCursor`, `graphics/DecodedMouseCursor`,
`mahogany/DesktopWindow#matroxGraphicsCursor`.

The host GPU (Matrox G200 family) draws the mouse pointer as a hardware cursor
overlay, so **the pointer is not part of the framebuffer**. The server forwards
the cursor RAM: 256 control bytes plus 64 rows x 48 bytes.

- Cursor mode = `control[6]`: 1 three-color, 2 XGA, 3 X-Window, 4 direct (16 color).
  Any other value: hide the custom cursor.
- 16 cursor palette colors: red/green/blue from control bytes
  `{8,12,16,96,99,...,132}`, `{9,13,17,97,100,...,133}`,
  `{10,14,18,98,101,...,134}` (step 3 from 96).
- Output: 64x64 image, hotspot fixed at (10,10) by the viewer.
- Modes 1-3: two bit planes per pixel; the 2-bit value maps through
  `{1: [T,0,1,2], 2: [0,1,T,XOR], 3: [T,T,0,1]}` (T = transparent).
- Mode 4: 4-bit color nibbles plus a 1-bit transparency mask.

The exact byte/bit walk order of the planes is intricate
(`DecodedMouseCursor#InitializeColorPositionInfoArray`) and not specified
here. ilo100vnc does not decode the cursor; it gives viewers a local arrow
through the RFB `Cursor` pseudo-encoding.

## 7. Open questions

1. Whether the `httpdata` token is single use or expires (2.4).
2. Whether the KVM is license-gated (LO100 Standard vs Advanced) and how a
   refusal looks on the wire (likely missing `v`/`k`/`m` letters in
   ServerHandshake or a ServerDisconnect).
3. Whether the BMC enforces its own idle timeout (4.7).
4. Whether 24/32bpp modes are ever sent, or only 8/16bpp.
