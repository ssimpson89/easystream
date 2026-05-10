# EasyStream TODO

## Protocol Support

- Add SRT output support for venues, relays, or future central ingest.
- Add SRT input support for remote campus-to-campus or encoder-to-control-room contribution workflows.
- Keep RTMPS output as the YouTube MVP path.
- Evaluate HLS output for YouTube HDR or non-RTMP workflows.
- Evaluate RTMP output for non-YouTube destinations that do not require RTMPS.
- Evaluate RIST as a future contribution protocol if packet-loss resilience becomes important.
- Design the stream destination model so a scheduled event can choose a protocol preset:
  - YouTube RTMPS
  - Generic RTMP/RTMPS
  - SRT caller
  - SRT listener
  - HLS

## MVP

- Local Go web app.
- FFmpeg command builder.
- FFmpeg supervisor with automatic restart.
- Multiple bandwidth/quality tiers.
- Manual start/stop to YouTube RTMPS.
- Scheduled YouTube broadcast creation, bind, go-live, and complete.
- Local health dashboard.

