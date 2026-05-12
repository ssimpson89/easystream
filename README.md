# EasyStream

A local campus streaming app for sending church services to YouTube (and other destinations) without a third-party streaming subscription.

EasyStream is a Go application that embeds a volunteer-friendly web UI, supervises FFmpeg for capture and encoding, and optionally uses the YouTube Live Streaming API for scheduled broadcasts, go-live transitions, and health checks.

## Quick start

```bash
# Build and run
go build -o easystream ./cmd/easystream
./easystream

# Or run directly
go run ./cmd/easystream
```

Open [http://127.0.0.1:8080](http://127.0.0.1:8080) in your browser.

### Prerequisites

- **Go 1.22+**
- **FFmpeg** installed and available on `PATH` (`brew install ffmpeg` on macOS, `apt install ffmpeg` on Linux)

#### Optional: FFmpeg with SRT support

To stream via SRT (Cloudflare Stream, MediaMTX, custom SRT receivers), FFmpeg must be built with `libsrt`. **Homebrew's default `ffmpeg` formula on macOS does NOT include libsrt.** EasyStream will detect this at startup, log a warning, and grey out the SRT option in the destination picker.

To enable SRT:

**macOS — Homebrew tap** (recommended):

```bash
brew uninstall ffmpeg
brew tap homebrew-ffmpeg/ffmpeg
brew install homebrew-ffmpeg/ffmpeg/ffmpeg --with-srt
```

**Linux — distribution packages**: most distros ship `ffmpeg` with libsrt enabled. Verify with:

```bash
ffmpeg -protocols 2>&1 | grep '^ *srt$'
```

If that prints `srt` you're good. If not, install `libsrt-dev` (or your distro's equivalent) and either rebuild ffmpeg with `--enable-libsrt` or grab a static build from [BtbN/FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds/releases).

EasyStream picks up the new ffmpeg automatically on next restart (it probes `exec.LookPath("ffmpeg")` plus the standard Homebrew/MacPorts/Linux install paths).

### Production deployment

Run EasyStream under **launchd** (macOS) or **systemd** (Linux) with restart-on-crash enabled. EasyStream persists operator intent to disk, so if the supervisor crashes mid-broadcast and the service manager restarts it, the stream picks up where it left off — the platform sees a brief reconnect rather than a stream end.

### Releases

Release builds for macOS and Linux are created by GoReleaser when a `v*` tag is pushed:

```bash
git tag v0.1.0
git push origin v0.1.0
```

GoReleaser injects the tag version, commit, and build date into the app. The current version appears in the EasyStream header and `/api/status` response.

## Features

- **Volunteer-friendly web UI** — step-by-step workflow: choose quality, set destination, pick capture source, go live
- **Multiple quality presets** — from 480p emergency to 1080p60 high-motion, matched to upload bandwidth
- **YouTube integration** — OAuth login, scheduled broadcasts, automatic go-live, "Go Live Now" button
- **Recurring schedules** — "Every Sunday at 8:45am and 10:45am CST" with special event overrides
- **HLS output** — serve an m3u8 playlist for Cloudflare, CDNs, or direct playback
- **Manual RTMP/RTMPS** — paste any ingest URL and stream key
- **FFmpeg supervisor** — automatic restart with exponential backoff, stall detection, health metrics
- **Live preview** — low-res MJPEG preview from capture source in the browser
- **Platform auto-detect** — defaults to avfoundation (macOS), dshow (Windows), or v4l2 (Linux)

## Configuration

EasyStream reads its config from environment variables. On startup it also
loads a `.env` file in the working directory if one is present. Real
environment variables take precedence over `.env` entries, so production
deployments (systemd, Docker, etc.) keep working unchanged.

```bash
cp .env.example .env
# edit .env with your values
go run ./cmd/easystream
```

`.env` is gitignored; only `.env.example` is committed.

| Variable | Default | Description |
|---|---|---|
| `EASYSTREAM_ADDR` | `127.0.0.1:8080` | Listen address for the web UI |
| `EASYSTREAM_DATA_DIR` | `~/.easystream` | Directory for tokens, schedules, HLS segments |
| `YOUTUBE_CLIENT_ID` | *(none)* | Google OAuth client ID (enables YouTube features) |
| `YOUTUBE_CLIENT_SECRET` | *(none)* | Google OAuth client secret |

## YouTube setup

YouTube integration is optional. Without it, you can still stream using manual RTMP/RTMPS or HLS output. To enable YouTube features (scheduled broadcasts, Go Live Now, auto go-live):

### 1. Create a Google Cloud project

1. Go to [Google Cloud Console](https://console.cloud.google.com/)
2. Click **Select a project** > **New Project**
3. Name it (e.g., "EasyStream") and click **Create**

### 2. Enable the YouTube Data API

1. In your project, go to **APIs & Services** > **Library**
2. Search for **YouTube Data API v3**
3. Click **Enable**

### 3. Configure the OAuth consent screen

1. Go to **APIs & Services** > **OAuth consent screen**
2. Choose **External** (or **Internal** if using Google Workspace)
3. Fill in:
   - **App name**: EasyStream
   - **User support email**: your email
   - **Developer contact email**: your email
4. Click **Save and Continue**
5. On the **Scopes** page, click **Add or Remove Scopes**
6. Add `https://www.googleapis.com/auth/youtube`
7. Click **Save and Continue**
8. On the **Test users** page, add the Google account you'll use for streaming
9. Click **Save and Continue**

> **Note**: While in "Testing" mode, only test users can authenticate. For production use, submit the app for verification.

### 4. Create OAuth credentials

1. Go to **APIs & Services** > **Credentials**
2. Click **Create Credentials** > **OAuth client ID**
3. Application type: **Web application**
4. Name: EasyStream
5. Under **Authorized redirect URIs**, add: `http://127.0.0.1:8080/api/youtube/auth/callback`
   - If using a different address, match the `EASYSTREAM_ADDR` value
6. Click **Create**
7. Copy the **Client ID** and **Client Secret**

### 5. Run EasyStream with YouTube credentials

Easiest: copy `.env.example` to `.env` and fill in your values:

```bash
cp .env.example .env
# edit .env:
#   YOUTUBE_CLIENT_ID=your-client-id.apps.googleusercontent.com
#   YOUTUBE_CLIENT_SECRET=your-client-secret
go run ./cmd/easystream
```

`.env` is gitignored — your secrets stay local.

If you'd rather export inline (CI/CD, systemd, etc.):

```bash
export YOUTUBE_CLIENT_ID="your-client-id.apps.googleusercontent.com"
export YOUTUBE_CLIENT_SECRET="your-client-secret"
go run ./cmd/easystream
```

### 6. Connect your YouTube account

1. Open the EasyStream web UI
2. Go to **Step 2: Destination** > **Scheduled** tab
3. Click **Connect YouTube**
4. Sign in with your Google account and grant access
5. You should see your channel name appear in the header

## HLS / Cloudflare setup

EasyStream can output HLS segments served over HTTP, useful for:
- **Cloudflare Stream** (pull mode)
- **CDN distribution**
- **Direct playback** in VLC, Safari, or any HLS-compatible player

### Using HLS output

1. In the web UI, go to **Step 2: Destination** > **Manual** tab
2. Change **Output type** to **HLS**
3. Click **Save Settings**
4. Click **Go Live** — FFmpeg starts writing `.m3u8` and `.ts` files
5. The playlist URL is shown in the UI (e.g., `http://127.0.0.1:8080/hls/stream.m3u8`)

### Cloudflare Stream (pull mode)

1. Set up EasyStream with HLS output as above
2. Make the playlist URL reachable from the internet:
   - **Port forwarding**: forward port 8080 on your router
   - **Cloudflare Tunnel**: `cloudflared tunnel --url http://127.0.0.1:8080` gives you a public URL
   - **Reverse proxy**: nginx/caddy in front of EasyStream
3. In Cloudflare Dashboard > **Stream** > **Live Inputs**:
   - Create a new live input
   - Choose **Pull** as the input type
   - Paste your public HLS URL (e.g., `https://your-tunnel.trycloudflare.com/hls/stream.m3u8`)

### Testing with VLC

```bash
vlc http://127.0.0.1:8080/hls/stream.m3u8
```

## Scheduling

### Recurring schedules

Set up recurring events like weekly services:

1. Go to **Step 2: Destination** > **Scheduled** tab
2. Click **+ Add** under **Recurring schedules**
3. Pick days (e.g., Sunday), time (e.g., 08:45), and timezone
4. Set the YouTube broadcast title and duration
5. Click **Save Schedule**

The scheduler will:
- Create a YouTube broadcast **30 minutes** before the scheduled time
- Auto-start FFmpeg at the scheduled time
- Transition the broadcast to **live** once the stream is active
- Auto-stop after the configured duration

### Special events (overrides)

For one-time events like holiday services:

1. Click **+ Add** under **Special events**
2. Pick a specific date, time, and duration
3. Save

## Architecture

```
cmd/easystream/         Entry point
internal/
  app/                  HTTP server, API routes, wiring
  ffmpeg/               Config builder, supervisor, progress parser
  hls/                  HLS segment server with CORS
  preview/              MJPEG preview from capture source
  quality/              Bandwidth/quality presets
  schedule/             Recurring schedules, overrides, scheduler
  ui/                   Embedded web UI (HTML/CSS/JS)
  youtube/              OAuth 2.0, YouTube Data API v3 client
```

## Data storage

All persistent data is stored in `~/.easystream/` (configurable via `EASYSTREAM_DATA_DIR`):

- `tokens.json` — YouTube OAuth tokens (contains secrets, do not share)
- `schedules.json` — Recurring schedules and special events
- `hls/` — HLS segments (temporary, cleaned on each stream start)
