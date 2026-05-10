# EasyStream Research and Architecture

Date: 2026-05-10

## Goal

Replace a third-party church-campus-to-YouTube streaming service with a simple local campus encoder app:

- Accept SDI, HDMI capture, and webcam inputs.
- Encode and stream to YouTube.
- Provide a volunteer-friendly web UI for start/stop, schedule status, health, and diagnostics.
- Reliably auto-start scheduled services.
- Later add centralized multi-campus management.

The current third-party cost is $50/month/campus for 5 campuses: $250/month, or $3,000/year before any future increases.

## Recommendation

Build a local Go application that manages FFmpeg as a child process, embeds a web UI, stores config locally, and talks directly to the YouTube Live Streaming API.

The Go binary should be the control plane, not the media engine. FFmpeg should remain the media engine because it already supports common capture APIs, mature encoders, RTMPS output, and device-specific capture paths. Trying to implement capture, sync, encoding, and RTMP in Go would add significant risk with no practical benefit.

The single distributed unit should be:

- `easystream` Go binary.
- External `ffmpeg` dependency, either installed on the machine or shipped beside the binary per platform.
- Local config/state directory containing campus settings, schedules, OAuth tokens, and logs.

This preserves the "single binary" operational model for our application while avoiding licensing, platform, and hardware-driver complexity from statically embedding FFmpeg.

## Key Findings

### YouTube

YouTube has two separate live resources:

- `liveStream`: the ingest feed we push to YouTube. It contains ingest URLs, stream key/name, health status, and whether YouTube is receiving data.
- `liveBroadcast`: the scheduled YouTube event/video. It contains title, scheduled start/end, privacy, DVR/latency/auto-start settings, and lifecycle state.

The app must create or reuse a stream, create a scheduled broadcast, bind the broadcast to the stream, start FFmpeg, wait until YouTube reports the bound stream as `active`, then transition the broadcast to `live`. YouTube explicitly requires the stream to be active before transition to `testing` or `live`.

YouTube recommends RTMPS for live streaming. For normal church services, RTMPS + H.264 + AAC is the best default. HLS is mainly useful for HDR or codecs/features that RTMP does not support, and adds more latency and complexity.

The API exposes stream health issues such as low bitrate, framerate mismatch, no audio stream, no video stream, long GOP/keyframe interval, unsupported codec, and ingestion starvation. These should be shown directly in the UI in volunteer-readable language.

### Encoding Defaults

Use these quality presets first. The tiers intentionally include more than one fallback level so a campus can keep streaming when upload bandwidth is degraded instead of treating `1080p` as all-or-nothing.

| Preset | Resolution | FPS | Video bitrate | Audio | Use case |
| --- | --- | --- | --- | --- | --- |
| Emergency | 480p | 30 | 1.5 Mbps | AAC 96k | last-resort continuity when the network is struggling |
| Low | 720p | 30 | 3 Mbps | AAC 128k | weak upload links |
| Standard | 720p | 30 | 4.5 Mbps | AAC 128k | safe fallback with decent quality |
| Recommended | 1080p | 30 | 8 Mbps | AAC 128k | most campuses |
| High | 1080p | 30 | 10 Mbps | AAC 160k | stable links with visual detail |
| High Motion | 1080p | 60 | 12 Mbps | AAC 160k | fast motion, if upload is stable |

All presets should use:

- RTMPS ingest.
- H.264 video.
- AAC audio.
- Constant bitrate.
- 2-second keyframe interval.
- Progressive scan.
- Stereo audio unless a campus explicitly needs more.

The UI should expose "Quality" as simple presets first, with an "Advanced" panel for exact bitrate, resolution, frame rate, device format, and audio input.

### Capture

Use FFmpeg device backends:

- macOS testing: `avfoundation`.
- Windows testing/field machines: `dshow`.
- Linux field machines: `v4l2` for UVC devices.
- Blackmagic SDI/HDMI: `decklink`, but only with FFmpeg builds compiled with the Blackmagic DeckLink SDK.

For campus reliability, prefer capture hardware that appears as a standard UVC device when possible. HDMI-to-USB UVC devices are operationally simpler than vendor-specific SDK cards. SDI is more robust over distance, so campuses with SDI should use proven SDI capture hardware, but that likely means Blackmagic or similar drivers and platform-specific install steps.

Practical hardware approach:

- Testing: any webcam, or cheap UVC HDMI capture.
- HDMI campuses: UVC HDMI capture device, or a known stable PCIe/Thunderbolt capture device.
- SDI campuses: Blackmagic UltraStudio/DeckLink class device, with a matching FFmpeg build and driver package.
- Avoid depending on a browser/webcam API for production capture. Browser preview can be useful, but FFmpeg should own the stream pipeline.

## Proposed App Architecture

```text
+---------------------------+
| Browser UI                |
| - status dashboard        |
| - schedule editor         |
| - start/stop controls     |
| - setup wizard            |
+-------------+-------------+
              |
              | HTTP/WebSocket
              v
+---------------------------+
| Go local service          |
| - scheduler               |
| - YouTube API client      |
| - FFmpeg supervisor       |
| - device discovery        |
| - health/metrics store    |
| - config/state database   |
+------+------+-------------+
       |      |
       |      +------------------+
       |                         |
       v                         v
+-------------+          +----------------+
| FFmpeg      |  RTMPS   | YouTube Live   |
| capture +   +--------->| ingest/API     |
| encode      |          +----------------+
+-------------+
```

### Go Components

- `cmd/easystream`: starts the local web server and background workers.
- `internal/config`: campus name, timezone, input devices, default quality, YouTube channel settings.
- `internal/store`: SQLite database for schedules, runs, log events, token metadata, and last known health.
- `internal/ffmpeg`: command builder, process supervisor, progress parser, stderr classification, restart policy.
- `internal/youtube`: OAuth, live stream creation/reuse, broadcast creation/update, bind, transition, health polling.
- `internal/scheduler`: service schedule, preflight, warmup, go-live, stop/completion, missed-run handling.
- `internal/devices`: device listing for `avfoundation`, `dshow`, `v4l2`, `decklink`.
- `web`: embedded static assets.

Use SQLite instead of a server database for the campus app. It is reliable, simple to back up, and avoids adding local services. For the future central service, use Postgres.

## Runtime State Machine

```text
Idle
  -> Scheduled
  -> Preparing YouTube Event
  -> Starting Encoder
  -> Waiting For YouTube Ingest
  -> Ready
  -> Live
  -> Completing
  -> Finished
```

Failure states:

- `Input Missing`: capture device disappeared or format unsupported.
- `No Audio`: FFmpeg/YouTube reports no audio stream.
- `Poor Network`: FFmpeg output stalls, reconnects, or YouTube reports ingestion starvation.
- `YouTube Not Receiving`: FFmpeg running but `liveStream.status.streamStatus` is not `active`.
- `API Auth Needed`: OAuth token revoked/expired.
- `Manual Attention`: schedule window missed or transition failed.

Every state should have one clear primary action in the UI.

## Scheduling Design

Use local schedules per campus with timezone-aware rules:

- Weekly recurring service times.
- One-off overrides for special events.
- Optional pre-service warmup duration, default 15 minutes.
- Optional post-service stop delay, default 5 minutes.
- Per-event title/description template.

Recommended automation flow:

1. 30-60 minutes before scheduled start: ensure a YouTube broadcast exists and is bound to a reusable stream.
2. 10-15 minutes before scheduled start: start FFmpeg and push to RTMPS.
3. Poll YouTube stream status and health.
4. At scheduled start, if stream is active, call `liveBroadcasts.transition` to `live`.
5. During the service, poll YouTube health and FFmpeg progress.
6. At scheduled end, transition broadcast to `complete`.
7. Stop FFmpeg after a short grace period.

Do not rely only on YouTube auto-start/auto-stop. Those settings can be enabled as a backup, but the app should own the state machine so the UI can explain exactly what is happening.

## Volunteer UI

The local UI should be dark, calm, and operational:

- Large current state: `Ready`, `Live`, `Needs Attention`, etc.
- One primary action: `Start Preview`, `Go Live`, `Stop Stream`, or `Fix Setup`.
- Schedule timeline showing next event and countdown.
- Audio level and video preview/thumbnail if feasible.
- YouTube health card with plain-language warnings.
- Network card: outbound bitrate, dropped frames, reconnect count, current upload target.
- Input card: selected video/audio device and detected format.
- Logs hidden behind "Details" so volunteers are not staring at FFmpeg output.

Avoid clutter. The default view should answer:

- Is it working?
- Is it live?
- What happens next?
- What do I press if something is wrong?

## FFmpeg Command Shape

Actual input arguments vary by OS/device, but output should look roughly like:

```bash
ffmpeg \
  -f <input_backend> <input_options> -i <video_and_audio_input> \
  -c:v libx264 -preset veryfast -tune zerolatency \
  -b:v 8000k -maxrate 8000k -bufsize 16000k \
  -g 60 -keyint_min 60 -sc_threshold 0 \
  -pix_fmt yuv420p -r 30 \
  -c:a aac -b:a 128k -ar 48000 -ac 2 \
  -f flv "rtmps://.../<stream-name>"
```

Use FFmpeg's progress output (`-progress pipe:1`) for machine-readable telemetry, and classify stderr lines for errors. The app should support reconnect flags for network hiccups and surface when reconnects happen.

For RTMPS, FFmpeg can enable TCP keepalive and bound network read/write waits with protocol options such as `-tcp_keepalive 1` and `-rw_timeout <microseconds>`. These settings help the process notice a broken connection, but the Go app must still supervise the FFmpeg process because an encoder can exit due to input loss, driver failure, network failure, or YouTube ingest rejection.

Supervisor behavior:

- Treat user-requested stop as final and do not restart.
- Treat unexpected FFmpeg exit as recoverable during a scheduled/live run.
- Restart with exponential backoff and jitter, capped at 30 seconds.
- Reset the backoff after a stable run window, such as 2 minutes.
- Track restart count, last exit reason, last progress update, outbound bitrate, dropped frames, and current state.
- If FFmpeg is running but progress stalls beyond a threshold, mark the stream `Degraded` and optionally restart if the stall exceeds the configured recovery window.
- Keep polling YouTube stream health separately; FFmpeg running does not prove YouTube is receiving usable video.

## Centralized Management Later

Do not build central management first. The local campus encoder must work independently if the internet is partially degraded and only YouTube ingest is reachable.

Later add:

- Cloud/server dashboard for all campuses.
- Campus agent registration.
- Remote schedule/config sync.
- Read-only health aggregation.
- Optional remote commands with explicit confirmation.
- Fleet update channel.

The central service should never be required for a campus to start its scheduled stream.

## Open Decisions

- Target OS for campus machines: Windows, macOS, Linux, or a fixed mini-PC image.
- Preferred capture hardware for each campus.
- Whether each campus streams to its own YouTube channel or one shared channel.
- Whether YouTube events are public immediately or scheduled as unlisted/private until go-live.
- Whether sermon metadata comes from Planning Center, Church Online Platform, calendar, or manual entry.
- Whether local preview is required in v1 or whether FFmpeg/YouTube health is enough.

## MVP Scope

1. Local Go app with embedded web UI.
2. Config wizard for FFmpeg path, capture input, quality preset, YouTube OAuth, and schedule.
3. Manual start/stop streaming to a configured YouTube stream key.
4. YouTube API integration for scheduled broadcasts and go-live/complete transitions.
5. Health dashboard using FFmpeg progress plus YouTube stream health.
6. Local logs and last-run report.

Defer:

- Central dashboard.
- Automatic software updates.
- Multi-destination streaming.
- Advanced graphics/lower thirds.
- Full browser preview if it slows the MVP.

## Sources

- YouTube LiveStreams resource: https://developers.google.com/youtube/v3/live/docs/liveStreams
- YouTube LiveBroadcasts resource: https://developers.google.com/youtube/v3/live/docs/liveBroadcasts
- YouTube broadcast transition API: https://developers.google.com/youtube/v3/live/docs/liveBroadcasts/transition
- YouTube encoder settings and bitrate recommendations: https://support.google.com/youtube/answer/2853702
- YouTube OAuth for installed apps: https://developers.google.com/youtube/v3/live/guides/auth/installed-apps
- YouTube API quota calculator: https://developers.google.com/youtube/v3/determine_quota_cost
- FFmpeg device documentation: https://ffmpeg.org/ffmpeg-devices.html
