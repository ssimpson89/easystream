# EasyStream

EasyStream is a self-hosted live-stream scheduler for small churches and other small organisations — set Sunday 10am once and your YouTube broadcast goes live, runs, and ends without anyone touching a button. It runs on a Mac mini, a Linux box, or a Raspberry Pi tucked in the AV closet, supervises FFmpeg for you, and gives volunteers a browser-based dashboard. Never go live and hope.

By default the dashboard listens on `127.0.0.1:8080` (only reachable from the same machine). To open it from another device on your LAN — and to use YouTube OAuth from a different browser — set `EASYSTREAM_ADDR` to a routable address (e.g. `0.0.0.0:8080` or `easystream.local:8080`). The same address is used as the YouTube OAuth redirect URI, so it has to match what you register in Google Cloud Console.

> If you're already running OBS or vMix and just want a scheduler that drives the YouTube broadcast for you, that's exactly what this is. Push to EasyStream over SRT and it handles the rest.

## Why use it

- **Set it and forget it.** Recurring weekly schedules ("Sunday 10am") plus one-off event overrides for the holiday service. EasyStream creates the YouTube broadcast right when the stream goes live (no scheduled-broadcast indicator showing on your channel before the service), transitions it to live once frames are flowing, and transitions it to complete when the service ends. Each schedule has an optional "pre-create minutes ahead" setting (0–60) if you'd rather have the watch URL available in advance.
- **A scheduler for OBS.** OBS has no native scheduler — pair it with EasyStream's SRT receiver and you get scheduling for free. Keep OBS for scene switching; EasyStream drives the YouTube lifecycle.
- **Preview every source before you go live.** A WebRTC preview in the dashboard shows the actual encoded frame — what your viewers will see — not just an "input connected" indicator.
- **Auto-resume if the power flickers.** EasyStream persists your intent to be live. If the Mac mini reboots mid-service, it picks the stream back up automatically — viewers see a brief reconnect, not a stream end.
- **Adaptive bitrate when the internet wobbles.** If upload bandwidth drops, EasyStream steps down to a lower quality preset and steps back up when the network recovers, instead of dropping the stream.

## Quick start

For most users:

```bash
go build -o easystream ./cmd/easystream
./easystream
```

Then open <http://127.0.0.1:8080> in your browser. That's it — the dashboard walks you through the rest.

You'll need Go 1.25+ and FFmpeg installed (`brew install ffmpeg` on macOS, `apt install ffmpeg` on Debian/Ubuntu, `dnf install ffmpeg` on Fedora). If you plan to receive a stream over SRT from OBS or vMix, see [Advanced: FFmpeg with SRT support](#ffmpeg-with-srt-support) below — Homebrew's default ffmpeg formula doesn't include libsrt.

## What you'll need

- A Mac mini, small Linux box, or Raspberry Pi 4/5 class machine, kept on the same network as your camera and on a reliable internet connection.
- A video source: a USB webcam, an HDMI/SDI capture card, an IP camera with an RTSP feed, or an upstream encoder (OBS, vMix, hardware encoder) that can push SRT.
- A YouTube channel, if you want to stream there. (Streaming to a custom RTMP destination doesn't require this.)
- A Google Cloud OAuth client, if you want EasyStream to manage YouTube broadcasts for you. Setup is walked through below.

## Features

- **Source kinds:** USB webcam, HDMI/SDI capture card, network pull (RTSP / SRT / UDP / HLS), or **SRT listener** — EasyStream binds a port and waits for OBS, vMix, or a hardware encoder to push to it. The listener stays up between streams, so the same OBS connection survives the move from "preview" to "live."
- **Destinations:** YouTube Live (with broadcast scheduling, auto go-live, and stop), custom RTMP/RTMPS (Twitch, Cloudflare, any ingest URL), or SRT push.
- **Local HLS monitoring:** runs alongside your primary destination and serves a low-latency HLS playlist on your LAN. Open it in VLC or Safari from a phone in the sanctuary to spot-check what's going out.
- **Quality presets:** from 480p emergency to 1080p60, matched to your upload bandwidth.
- **Hardware encoder selection:** Apple VideoToolbox, NVIDIA NVENC, Intel QuickSync, VA-API — auto-detected and offered in the UI when available.
- **HDR to SDR tone-mapping:** if your camera is HDR-capable, tick the box and EasyStream tone-maps to Rec.709 before encoding, so colours don't clip on YouTube.
- **Live WebRTC preview** in the dashboard, with audio meters.
- **FFmpeg supervisor:** restart on crash with backoff, stall detection, exposed health metrics — so the encoder stays up even if a USB cable hiccups.

## Setup walk-through

### 1. Connect a source

Open the dashboard and choose **Source**. For a camera or capture card, EasyStream auto-detects connected devices on macOS, Linux, and Windows and shows them in a dropdown — pick one. For a network feed (an IP camera, or a remote SRT pull), paste the URL. For an upstream encoder pushing to you, choose **SRT listener**, pick a port (default 9999), and copy the publish URL EasyStream renders. Paste that URL into OBS or vMix as the stream destination. The receiver stays up the moment you save — you'll see the encoder's feed appear in the preview within a few seconds.

### 2. Connect YouTube (optional)

YouTube integration is optional — without it you can still stream to a custom RTMP destination or local HLS. To enable it you'll create a Google Cloud OAuth client; this takes about ten minutes the first time.

1. In [Google Cloud Console](https://console.cloud.google.com/), create a new project (call it whatever).
2. Under **APIs & Services > Library**, enable **YouTube Data API v3**.
3. Under **OAuth consent screen**, choose **External**, fill in the basics, and add the scope `https://www.googleapis.com/auth/youtube`. Add the Google account you'll stream from as a test user.
4. Under **Credentials**, create an **OAuth client ID** of type **Web application**. Add `http://127.0.0.1:8080/api/youtube/auth/callback` as an authorised redirect URI (match the address you'll run EasyStream on).
5. Copy `.env.example` to `.env` and paste in the client ID and secret:

   ```bash
   cp .env.example .env
   # YOUTUBE_CLIENT_ID=...
   # YOUTUBE_CLIENT_SECRET=...
   ./easystream
   ```

6. Back in the dashboard, click **Connect YouTube** and grant access. Your channel name should appear in the header.

### 3. Schedule a stream

This is the main event — once a schedule is set, most operators never touch the dashboard again on Sunday morning.

Open **Destination > Scheduled** and click **+ Add** under **Recurring schedules**. Pick the days and time (e.g. Sunday 10:00), the timezone, the broadcast title, and a duration. EasyStream handles the entire YouTube broadcast lifecycle: by default it creates the broadcast right when the stream goes live, transitions to live once frames are flowing, and transitions to complete when the duration elapses. You don't open YouTube Studio. If you want the watch URL ahead of time (e.g. to paste in a bulletin), set **Pre-create broadcast (minutes ahead)** on the schedule — 0 is just-in-time, up to 60 minutes early.

For one-off events (Christmas Eve, a funeral), add a **Special event** instead — same fields, but a single date.

### 4. Going live

For a scheduled service, you don't have to do anything — EasyStream goes live on its own. If you want to start a stream manually, click **Stream to YouTube** (or **Custom destination**) on the dashboard, confirm the broadcast title in the modal, and click **Start streaming**. To end early, click **Stop**. The dashboard shows the bitrate, dropped frames, audio levels, and the YouTube broadcast status throughout, and the WebRTC preview shows the encoded output you're sending. If the network degrades and adaptive bitrate is on, you'll see a banner saying so.

## Why EasyStream over the alternatives

- **OBS** is a fantastic production switcher and free, but OBS has no native scheduler — pair it with EasyStream's SRT receiver and you get scheduling for free. Keep OBS for scene switching and push to EasyStream over SRT.
- **vMix** is excellent and has scheduling, but it's Windows-only and expensive — overkill for a small church that just needs to stream one camera on Sunday morning.
- **YouTube Studio's built-in scheduling** lets you create a broadcast ahead of time, but you still have to be at the computer to start the encoder and press Go Live. EasyStream automates both ends.
- **Restream / Castr / other SaaS** charge a monthly subscription and require uploading your video to them first, adding a network hop. EasyStream runs on hardware you already own and pushes directly to YouTube.

---

## Advanced (for sysadmins)

This section covers the bits a non-technical operator can ignore. It's here for the volunteer who's also the IT person.

### Configuration

EasyStream reads its config from environment variables. On startup it also loads a `.env` file from the working directory; real environment variables take precedence so systemd/launchd deployments work unchanged. `.env` is gitignored — only `.env.example` is committed.

| Variable | Default | Description |
|---|---|---|
| `EASYSTREAM_ADDR` | `127.0.0.1:8080` | Listen address for the web UI |
| `EASYSTREAM_DATA_DIR` | `~/.easystream` | Directory for tokens, schedules, HLS segments, intent file |
| `EASYSTREAM_FFMPEG` | *(auto-detect)* | Absolute path to the `ffmpeg` binary. Useful for non-standard installs (snap, flatpak, `/nix/store`, `/opt/...`, manually compiled). |
| `YOUTUBE_CLIENT_ID` | *(none)* | Google OAuth client ID (enables YouTube features) |
| `YOUTUBE_CLIENT_SECRET` | *(none)* | Google OAuth client secret |

Data files in `EASYSTREAM_DATA_DIR`:

- `tokens.json` — YouTube OAuth tokens (contains secrets, do not share)
- `schedules.json` — recurring schedules and special events
- `intent.json` — last known operator intent, used for auto-resume
- `hls/` — HLS segments (temporary; cleaned on each stream start)

### FFmpeg path resolution

EasyStream auto-detects `ffmpeg` in this order, first hit wins:

1. `EASYSTREAM_FFMPEG` env var.
2. Homebrew `ffmpeg-full` keg-only path on macOS (`/opt/homebrew/opt/ffmpeg-full/bin/ffmpeg` on Apple Silicon, `/usr/local/opt/ffmpeg-full/bin/ffmpeg` on Intel).
3. `ffmpeg` on `PATH`.
4. Standard macOS install dirs as a fallback (covers running from Finder/launchd with a minimal PATH).

The resolved path is printed to the daemon log at startup as `ffmpeg binary: /path/to/ffmpeg`.

### FFmpeg with SRT support

To stream **via** SRT (Cloudflare Stream, MediaMTX, custom SRT receivers), or to receive **from** OBS/vMix via the SRT listener, `ffmpeg` must be built with `libsrt`. EasyStream probes this at startup, logs a warning, and disables the SRT options in the UI when it's missing.

Check:

```bash
ffmpeg -protocols 2>&1 | grep '^ *srt$'
```

If `srt` is printed, you're set. If not:

- **macOS** — Homebrew's default `ffmpeg` formula does **not** include libsrt. Install [`ffmpeg-full`](https://formulae.brew.sh/formula/ffmpeg-full) (`brew install ffmpeg-full`); EasyStream auto-detects it on the next restart.
- **Linux** — most distros ship libsrt enabled. Minimal distros: install `libsrt`/`libsrt-dev` and rebuild, or grab a static build from [BtbN/FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds/releases) and point `EASYSTREAM_FFMPEG` at it.

### Production deployment

Run EasyStream under **launchd** (macOS) or **systemd** (Linux) with restart-on-crash enabled. EasyStream persists operator intent to disk in `intent.json`, so if the supervisor crashes mid-broadcast and the service manager restarts it, the stream picks up where it left off — the platform sees a brief reconnect rather than a stream end.

### Releases

Release builds for macOS and Linux are produced by GoReleaser when a `v*` tag is pushed:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The tag version, commit, and build date are injected into the binary and shown in the EasyStream header and `/api/status` response.

### Architecture

```
cmd/easystream/         Entry point
internal/
  app/                  HTTP server, API routes, wiring, intent persistence
  ffmpeg/               Config builder, supervisor, progress parser, adaptive bitrate
  hls/                  HLS segment server with CORS
  ingest/               Always-on SRT receiver and relay
  preview/              WebRTC preview pipeline
  quality/              Bandwidth/quality presets
  schedule/             Recurring schedules, overrides, scheduler
  ui/                   Embedded web UI (HTML/CSS/JS)
  youtube/              OAuth 2.0, YouTube Data API v3 client
```

## License

EasyStream is released under the [MIT License](LICENSE).
