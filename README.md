# IMSAI 8080esp Telnet/TCP ↔ WebSocket `/tty` Gateway

Connect any VT-100 terminal (PuTTY, `telnet`, minicom, cool-retro-term, …) to the CP/M console
of an IMSAI 8080esp over plain TCP, without a browser.

```
[VT100 terminal] --TCP (telnet or raw)--> [gateway] --WS client--> ws://<imsai>/tty
```

The IMSAI firmware exposes its console only as a WebSocket (`ws://<host>/tty`) and has no Telnet
server for it (port 23 is the emulated Hayes modem, off by default). The gateway sits in between:
it accepts a normal TCP/Telnet connection and bridges it to that WebSocket.

It ships as a single, dependency-free static binary and as a small container image. There is no
runtime to install.

## Get it

- **Release zip**: download `imsai-gw-<os>-<arch>.zip` from the project's GitHub Releases
  (`linux-amd64`, `linux-arm64`, `linux-armv7` for Raspberry Pi, `windows-amd64`). Each zip
  contains the executable plus a default `imsai-gw.toml`.
- **Container image**: `ghcr.io/electricdream/imsai-tty-gateway:latest` (multi-arch `linux/amd64`,
  `linux/arm64`). Pinned version tags such as `:0.3.0` and `:0.3` are also published.

On Windows the executable is `imsai-gw.exe`.

## Quick start

> ⚠️ **Close the IMSAI WebUI first.** The device accepts only **one WebSocket client per channel**.
> If the browser console stays open it fights the gateway over the console (`/tty`) and the front
> panel (`/cpa`), and reconnecting `/cpa` restarts the device's web server. Keep the WebUI closed
> the whole time the gateway is in use.

The gateway connects **out** to your IMSAI and **listens** for terminals. The one thing it needs
from you is your IMSAI's address.

**1. Start the gateway, pointing it at your IMSAI** (use your own device's IP here):

```sh
./imsai-gw --host 192.168.1.50
```

**2. Connect a terminal to the gateway** on port 2323. If your terminal is on the *same machine*
as the gateway, that machine is `localhost`:

```sh
telnet localhost 2323
```

If your terminal is on a *different machine*, use the address of the machine running the gateway
instead (not the IMSAI's), for example `telnet 192.168.1.10 2323`.

**3. Get to the `A>` prompt.** On connect the gateway prints a banner with the machine's `POWER`
and `RUN` state; what you do next depends on it:

- **POWER on, RUN on** (the normal case): the screen may look blank, because CP/M only redraws its
  prompt in response to input. Press **Enter** once and the `A>` prompt appears.
- **POWER on, RUN off** (the CPU is stopped): start it first. Open the control panel with
  **`Ctrl+\`**, go to the **Panel** tab, actuate **RUN**, then leave the panel (`q`) and press
  **Enter**.
- **POWER off**: the machine is off and has no console. The front-panel power switch is not exposed
  over the network, so power it on from the device itself (or its WebUI); once it has booted CP/M,
  press **Enter** for the prompt. You can still open the control TUI with **`Ctrl+\`** even while
  the machine is off, as long as the ESP32 is powered and has loaded its firmware: the Disks, LIB
  and SYS panes talk to the device's HTTP API, which is up regardless of the emulation's state.

On the gateway side, `--host` is the **only** required setting. Everything else has a sensible
default, so the gateway listens on all network interfaces on port `2323` out of the box. If you
prefer not to type anything, put `host` in `imsai-gw.toml` and run `./imsai-gw` with no arguments
at all (see
[Configuration](#configuration)).

> **No authentication.** The gateway listens on all interfaces by default. On an untrusted
> network, restrict it with `--listen-host 127.0.0.1` (local only) or a firewall rule on the port.

## Run with Docker

Same idea: the image's entrypoint *is* the gateway, so pass it the same options. The gateway still
listens on its default port `2323` inside the container; you only need to **publish** it to the
host with Docker's `-p`, and point the gateway at your IMSAI:

```sh
docker run --rm -p 2323:2323 ghcr.io/electricdream/imsai-tty-gateway:latest --host 192.168.1.50
```

Then connect with `telnet <docker-host> 2323`.

Other ways to configure the container:

```sh
# Use an environment variable instead of a flag
docker run --rm -p 2323:2323 -e IMSAI_GW_HOST=192.168.1.50 ghcr.io/electricdream/imsai-tty-gateway:latest

# Mount your own config file (read-only)
docker run --rm -p 2323:2323 \
    -v "$PWD/imsai-gw.toml:/etc/imsai-gw.toml:ro" \
    ghcr.io/electricdream/imsai-tty-gateway:latest --config /etc/imsai-gw.toml

# Run as a background service that restarts on boot
docker run -d --name imsai-gw --restart unless-stopped -p 2323:2323 \
    ghcr.io/electricdream/imsai-tty-gateway:latest --host 192.168.1.50
```

Notes:

- `-p 2323:2323` is Docker port publishing, written `<host-port>:<container-port>`. The
  container port stays `2323` (the gateway's default); change only the left number to reach the
  gateway on a different port on the host.
- If your IMSAI is reached by a host name that does not resolve inside the container, use its IP
  (`--host 192.168.1.50`), or run with `--network host` on Linux.
- The image bundles a default `/etc/imsai-gw.toml`; override it with flags, env vars, or your own
  mounted file.
- If `docker pull` is denied, the GHCR package is private; make it public in the repo's
  *Packages* settings.

## Terminal clients

- **`telnet`**: works out of the box (`auto` mode negotiates echo and character-at-a-time input).
- **PuTTY**: choose connection type **Telnet** (not Raw) so echo negotiation works.
- **`nc` / raw TCP**: also fine; line endings are still normalized, but local echo is the
  client's own responsibility.

You should **not** need to touch any line-ending or local-echo setting in your terminal; the
gateway normalizes Enter and handles Telnet echo for you.

On connect, the gateway prints a short banner: its name and version, the control-TUI hotkey, a
reminder to press Enter if the screen looks blank, and the machine's `POWER`/`RUN` state.

## Control TUI

Press the hotkey (default **`Ctrl+\`**) to open an in-terminal control panel on the alternate
screen. `Tab` cycles panes; `q` or `Esc` returns to the console.

- **Disks**: list the drive slots, eject a disk, or mount one from the on-device library.
- **Panel**: the front-panel LEDs and momentary command switches
  (Examine / Deposit / Reset / Run / Single-Step).
- **SYS**: device information from `GET /system` (scrollable).
- **LIB**: the disk-image library. List images, **view** a disk's CP/M directory without mounting
  it (browse users 0-15 with `←`/`→`), and **delete** an image (with confirmation). Both floppy
  (`.dsk`) and hard-disk (`.hdd`) images can be viewed.

Turn the TUI off with `--no-tui`, or change the key with `--hotkey`.

## Configuration

Settings can come from the command line, the environment, or a TOML file. When the same setting
is given in more than one place, the **higher-priority source wins**:

> **CLI flags > environment variables (`IMSAI_GW_*`) > TOML config file > built-in defaults**

A config file is read from `--config <path>`, or automatically from `./imsai-gw.toml` if one is
present in the working directory. See [`imsai-gw.toml`](imsai-gw.toml) for a fully commented
sample.

```sh
# These are equivalent ways to set the same options:
./imsai-gw --host 192.168.1.50 --listen-port 2323
IMSAI_GW_HOST=192.168.1.50 ./imsai-gw
./imsai-gw --config imsai-gw.toml
```

| Option | CLI flag | Env var | Default | Meaning |
|--------|----------|---------|---------|---------|
| host | `--host` | `IMSAI_GW_HOST` | *(required)* | IMSAI IP or host name; no default, the gateway exits if unset |
| ws_port | `--ws-port` | `IMSAI_GW_WS_PORT` | `80` | device WebSocket port |
| listen_host | `--listen-host` | `IMSAI_GW_LISTEN_HOST` | `0.0.0.0` | TCP bind address (all interfaces; `127.0.0.1` = local only) |
| listen_port | `--listen-port` | `IMSAI_GW_LISTEN_PORT` | `2323` | TCP port terminals connect to |
| mode | `--mode` | `IMSAI_GW_MODE` | `auto` | `auto` / `telnet` / `raw` |
| eol | `--eol` | `IMSAI_GW_EOL` | `cr` | Enter sent to CP/M as `cr` / `crlf` / `lf` / `raw` |
| crlf_out | `--no-crlf-out` | `IMSAI_GW_CRLF_OUT` | on | output: bare LF to CR LF (turn off for binary) |
| cols | `--cols` | `IMSAI_GW_COLS` | `80` | output: emulate auto-wrap at column N (0 = off) |
| framing | `--framing` | `IMSAI_GW_FRAMING` | `binary` | TCP-to-WS frames: `binary` (8-bit) / `text` (7-bit) |
| throttle | `--no-throttle` | `IMSAI_GW_THROTTLE` | on | pace TCP-to-WS input |
| cr_delay | `--cr-delay` | `IMSAI_GW_CR_DELAY` | `0.100` | delay after a CR, seconds |
| char_delay | `--char-delay` | `IMSAI_GW_CHAR_DELAY` | `0.020` | delay after any other char, seconds |
| max_sessions | `--max-sessions` | `IMSAI_GW_MAX_SESSIONS` | `1` | concurrent client sessions |
| reconnect | `--no-reconnect` | `IMSAI_GW_RECONNECT` | on | auto-reconnect the WS if the device drops it |
| reconnect_delay | `--reconnect-delay` | `IMSAI_GW_RECONNECT_DELAY` | `2.0` | seconds between reconnect attempts |
| reconnect_max | `--reconnect-max` | `IMSAI_GW_RECONNECT_MAX` | `0` | max attempts (0 = unlimited) |
| tui | `--no-tui` | `IMSAI_GW_TUI` | on | in-terminal control TUI |
| hotkey | `--hotkey` | `IMSAI_GW_HOTKEY` | `Ctrl+\` | key that opens the TUI |
| log_level | `--log-level` | `IMSAI_GW_LOG_LEVEL` | `INFO` | `DEBUG` / `INFO` / `WARNING` / `ERROR` |

Run `./imsai-gw --help` for the full list.

## How it works (behavior notes)

- **Line endings, input**: CP/M expects a single CR for Enter. The gateway collapses `CR`,
  `CR LF`, `CR NUL` (the Telnet NVT form), and a lone `LF` down to one `CR`, and drops stray
  `NUL`. Use `--eol raw` to disable this (for example during 8-bit binary transfers).
- **Line endings and width, output**: a bare `LF` from the device is turned into `CR LF` (ONLCR),
  and output is wrapped to emulate an 80-column console (tracking the cursor through ANSI
  sequences). This keeps full-screen apps that rely on auto-wrap looking correct on wider
  terminals. Turn both off with `--no-crlf-out` / `--cols 0` for binary transfers.
- **Telnet**: in `auto` and `telnet` modes the gateway announces `WILL ECHO` + `WILL SGA` on
  connect and parses/strips IAC sequences, so the client runs in character mode with no
  double-echo and no raw IAC bytes leaking to the screen; CP/M provides the echo.
- **8-bit clean**: the device accepts client binary frames, so `binary` framing is the default
  (no UTF-8 corruption for byte values >= 0x80).
- **No WebSocket keepalive ping**: the ESP `/tty` server closes a connection that gets pinged
  (after about 20 s), so the gateway disables WS keepalive pings; console traffic is the liveness
  signal.
- **Auto-reconnect**: if the *device* drops the WebSocket while your terminal stays connected, the
  gateway prints `[gateway] console link lost (...); reconnecting...`, retries, and resumes.
  Keystrokes typed during the gap are buffered by TCP and not lost. A disconnect on the client
  side ends the session.
- **One client per channel**: the device allows only a single WebSocket client per channel, so
  close the WebUI console while you use the gateway.

## Log messages

When a session ends, the cause is logged:

| Cause | Meaning |
|-------|---------|
| `client closed connection` | the terminal closed cleanly (FIN) |
| `client TCP read/write failed` | the terminal reset the connection (RST) or a network error |
| `device WS closed` | the ESP dropped the `/tty` socket (e.g. the TTY was toggled off) |
| `device unreachable` | the WebSocket could not be (re)opened |
