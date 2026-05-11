// EasyStream — Alpine.js front-end.
//
// Single reactive component holds all state. Polls /api/status every 2s and
// mirrors the result into reactive properties. The DOM binds declaratively
// via x-show, x-bind, x-text, x-model etc., so we never call querySelector
// from rendering code.
//
// WebRTC and the clipboard API stay imperative — they're exposed as methods
// on the component and called from Alpine event handlers.

document.addEventListener("alpine:init", () => {
  Alpine.data("app", () => ({

    // ============================================================
    // SERVER-SOURCED STATE (refreshed every 2s)
    // ============================================================
    stream: { state: "idle", lastProgress: {}, restartCount: 0 },
    config: null,
    youtube: { authenticated: false, configured: false, channelName: "" },
    scheduler: null,
    nextEvents: [],
    presets: [],
    confidence: [],
    adaptive: { enabled: true, isFallback: false },
    health: {},
    activeBroadcastId: "",
    devices: { video: [], audio: [] },
    schedules: [],
    overrides: [],

    // ============================================================
    // UI STATE
    // ============================================================
    view: "dashboard",
    presetMenuOpen: false,
    schedFormOpen: false,
    ovrFormOpen: false,
    stopConfirm: false,
    toast: "",
    deviceStatusText: "Scanning for devices...",

    ytModal: { open: false, title: "Live Stream", privacy: "unlisted", warn: "", busy: false },
    customModal: { open: false, warn: "", busy: false },

    // Preview
    previewVisible: true,        // user wants the preview pane open
    previewError: "",
    previewSuppressed: false,    // user explicitly hid it
    audioMeterLevel: 0,
    audioMeterPeak: 0,
    audioMeterText: "No audio",
    audioMeterActive: false,

    // Form mirror of server config (auto-saved on change)
    videoSourceValue: "",
    audioSourceValue: "",
    selectedPreset: "recommended",
    outputMode: "rtmp",
    ingestUrl: "",
    streamKey: "",
    hasStreamKey: false,
    hlsUrl: "http://127.0.0.1:8080/hls/stream.m3u8",

    schedForm: { name: "", days: [], time: "08:45", timezone: "America/Chicago", durationMin: 120, title: "", description: "", privacy: "unlisted" },
    ovrForm: { name: "", wallClock: "", timezone: "America/Chicago", durationMin: 120, title: "", description: "", privacy: "unlisted" },

    // Internal
    _previewPC: null,
    _previewTimeout: null,
    _previewStarting: false,
    _audioMeterInterval: null,
    _audioMeterLastEnergy: null,
    _audioMeterLastDuration: null,
    _audioMeterPeakHold: 0,
    _audioMeterContext: null,
    _audioMeterSource: null,
    _audioMeterAnalyser: null,
    _audioMeterGain: null,
    _audioMeterData: null,
    _configLoaded: false,
    _wasLive: false,
    _stopTimer: null,
    _toastTimer: null,
    _statusInterval: null,
    _deviceInterval: null,

    dayList: [
      { value: "sunday",    label: "Sun" },
      { value: "monday",    label: "Mon" },
      { value: "tuesday",   label: "Tue" },
      { value: "wednesday", label: "Wed" },
      { value: "thursday",  label: "Thu" },
      { value: "friday",    label: "Fri" },
      { value: "saturday",  label: "Sat" },
    ],
    tzList: [
      { value: "America/Chicago",     label: "Central (CST/CDT)" },
      { value: "America/New_York",    label: "Eastern (EST/EDT)" },
      { value: "America/Denver",      label: "Mountain (MST/MDT)" },
      { value: "America/Los_Angeles", label: "Pacific (PST/PDT)" },
      { value: "America/Phoenix",     label: "Arizona (MST)" },
      { value: "Pacific/Honolulu",    label: "Hawaii (HST)" },
      { value: "America/Anchorage",   label: "Alaska (AKST/AKDT)" },
      { value: "UTC",                 label: "UTC" },
    ],
    tzListShort: [
      { value: "America/Chicago",     label: "Central (CST/CDT)" },
      { value: "America/New_York",    label: "Eastern (EST/EDT)" },
      { value: "America/Denver",      label: "Mountain (MST/MDT)" },
      { value: "America/Los_Angeles", label: "Pacific (PST/PDT)" },
      { value: "UTC",                 label: "UTC" },
    ],

    // ============================================================
    // INIT / LIFECYCLE
    // ============================================================
    async init() {
      // Browser timezone as schedForm default (overridden later by user).
      try {
        const browserTZ = Intl.DateTimeFormat().resolvedOptions().timeZone;
        if (this.tzList.some((t) => t.value === browserTZ)) {
          this.schedForm.timezone = browserTZ;
          this.ovrForm.timezone = browserTZ;
        }
      } catch (_) {}

      // Initial loads
      await this.refresh();
      this.loadSchedules();
      this.loadOverrides();
      this.scanDevices();

      // Polling
      this._statusInterval = setInterval(() => this.refresh(), 2000);
      this._deviceInterval = setInterval(() => this.scanDevices(false), 5000);

      // Track previous live state so we only act on actual transitions,
      // not on every reactive re-evaluation (refresh() reassigns this.stream
      // every 2s, which can re-trigger $watch even when isLive stays false).
      this._wasLive = this.isLive;
      this.$watch("isLive", (now) => {
        const prev = this._wasLive;
        this._wasLive = now;
        if (now === prev) return; // no real transition

        document.title = now ? "● LIVE · EasyStream" : "EasyStream";
        const favicon = document.querySelector("#favicon");
        if (favicon) {
          favicon.href = now
            ? "data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Crect width='16' height='16' fill='%230d1117'/%3E%3Ccircle cx='8' cy='8' r='5' fill='%23ff1a1a'/%3E%3C/svg%3E"
            : "data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Crect width='16' height='16' fill='%230d1117'/%3E%3Cpath d='M5 4 L12 8 L5 12 Z' fill='%232f81f7'/%3E%3C/svg%3E";
        }

        if (now && this.previewVisible && !this.previewSuppressed) {
          // Going live — reconnect the audio meter to the existing WebRTC
          // audio track so it picks up audio from the main stream's RTP feed.
          setTimeout(() => {
            if (!this.isLive || !this._previewPC) return;
            const pc = this._previewPC;
            const receivers = pc.getReceivers ? pc.getReceivers() : [];
            for (const r of receivers) {
              if (r.track && r.track.kind === "audio") {
                this.attachAudioMeterTrack(r.track);
                break;
              }
            }
          }, 1500);
        }

        if (!now && this.previewVisible && !this.previewSuppressed) {
          // Going idle — refresh the full PeerConnection after preview
          // FFmpeg has had time to start via Unblock().
          setTimeout(() => {
            if (this.isLive) return;
            if (!this.previewVisible || this.previewSuppressed) return;
            this.refreshPreview();
          }, 2000);
        }
      });

      // Tear down WebRTC PC on tab close.
      window.addEventListener("beforeunload", () => this.stopPreviewPC());
      window.addEventListener("pointerdown", () => this.resumeAudioMeterContext(), { passive: true });
      window.addEventListener("keydown", () => this.resumeAudioMeterContext());

      // Start preview on initial load (idle state) — unless user suppressed.
      this.$nextTick(() => {
        if (!this.isLive) this.maybeStartPreview();
      });
    },

    // ============================================================
    // COMPUTED
    // ============================================================
    get isLive() {
      return ["starting", "running", "degraded", "restarting"].includes(this.stream.state);
    },
    get videoOK() {
      return !!this.videoSourceValue;
    },
    get hasCustomDest() {
      if (!this.ingestUrl) return false;
      if (this.outputMode === "hls") return true;
      return this.streamKey.length > 0 || this.hasStreamKey;
    },
    get ytActionReason() {
      if (!this.youtube.authenticated) return "Connect YouTube in Settings to enable.";
      if (!this.videoOK) return "Pick a video source in Settings.";
      return "";
    },
    get customActionReason() {
      if (!this.videoOK) return "Pick a video source in Settings.";
      if (!this.hasCustomDest) return "Set a server URL & key in Settings.";
      return "";
    },
    get startedAt() {
      const s = this.stream?.startedAt;
      return s && !s.startsWith("0001-01-01") ? new Date(s) : null;
    },
    get scheduleEndsAt() {
      const e = this.scheduler?.activeEndsAt;
      return e && !e.startsWith("0001-01-01") ? new Date(e) : null;
    },
    get activeEventName() {
      return this.scheduler?.activeEventName || "";
    },
    get liveHeadline() {
      return this.activeEventName || "Live stream";
    },
    get liveMeta() {
      const parts = [];
      if (this.startedAt) parts.push(`Started ${this.fmtTime(this.startedAt)}`);
      if (this.scheduleEndsAt) parts.push(`ends ${this.fmtTime(this.scheduleEndsAt)}`);
      return parts.join(" · ") || "Streaming";
    },
    get liveBannerEvent() {
      return this.activeEventName ? `· ${this.activeEventName}` : "";
    },
    get liveBannerTime() {
      return this.startedAt ? `started ${this.fmtTime(this.startedAt)}` : "";
    },
    get liveHealthParts() {
      const parts = [];
      let dest;
      if (this.activeBroadcastId) dest = "LIVE on YouTube";
      else if (this.outputMode === "hls") dest = "Streaming to local HLS playlist";
      else {
        const platform = this.platformFromURL(this.ingestUrl);
        dest = platform ? `Streaming to ${platform}` : "Streaming to custom server";
      }
      parts.push({ text: dest });

      const conf = (this.confidence || []).reduce((a, c) => (a[c.label] = c, a), {});
      const enc = conf.Encoder;
      if (enc?.status === "green" || enc?.status === "yellow") parts.push({ text: "Receiving video" });
      else if (enc?.status === "red") parts.push({ text: "Video problem" });
      else parts.push({ text: "Sending video..." });

      const aud = conf.Audio;
      if (aud?.status === "green") parts.push({ text: "Audio detected" });
      else if (aud?.status === "yellow") parts.push({ text: "Audio very quiet" });
      else if (aud?.status === "red") parts.push({ text: "No audio" });
      else parts.push({ text: "Waiting for audio..." });

      return parts;
    },
    get liveHealthSeverity() {
      let worst = "green";
      for (const c of (this.confidence || [])) {
        if (c.status === "red") return "failed";
        if (c.status === "yellow") worst = "degraded";
      }
      return worst === "green" ? "" : worst;
    },
    get nextEvent() {
      return this.nextEvents[0] || null;
    },
    get canStartScheduledNow() {
      if (!this.nextEvent) return false;
      const when = new Date(this.nextEvent.startTime);
      const minutesUntil = (when.getTime() - Date.now()) / 60000;
      return minutesUntil < 60 && this.stream.state === "idle";
    },
    get extendLabel() {
      const extra = this.scheduler?.extraMinutes || 0;
      return extra > 0 ? `+15 min (extended ${extra}m)` : "+15 min";
    },
    get showExtendButton() {
      return !!this.activeEventName;
    },
    get bitrateText() {
      const k = this.stream.lastProgress?.bitrateKbps;
      return k ? `${Math.round(k)} kbps` : "-";
    },
    get logLineText() {
      return this.stream.lastError || this.stream.lastExit || this.stream.lastLogLine || "Stream is live.";
    },
    get problemTitle() {
      return {
        degraded: "Stream needs attention",
        restarting: "Reconnecting to ingest...",
        failed: "Stream failed",
      }[this.stream.state] || "Stream issue";
    },
    get problemDetail() {
      return this.stream.lastError || this.stream.lastExit || this.stream.lastLogLine || "Check your network and capture source.";
    },
    get idleLabel() {
      return {
        idle: "Idle",
        stopping: "Stopping",
        failed: "Stopped",
      }[this.stream.state] || "Idle";
    },
    get idleDetail() {
      if (this.stream.state === "stopping") return "Stopping encoder...";
      if (this.stream.state === "failed") return this.stream.lastError || this.stream.lastExit || "Last attempt did not complete.";
      return "Ready to stream.";
    },
    get currentPreset() {
      return this.presets.find((p) => p.id === this.selectedPreset);
    },
    get customDestLabel() {
      if (this.outputMode === "hls") return `Local HLS playlist · ${this.hlsUrl}`;
      const platform = this.platformFromURL(this.ingestUrl);
      return platform ? `${platform} · ${this.ingestUrl}` : (this.ingestUrl || "(not set)");
    },
    get isSDISource() {
      return this.decodeSourceValue(this.videoSourceValue)?.kind === "sdi";
    },

    // ============================================================
    // POLLING / API CALLS
    // ============================================================
    async api(path, options = {}) {
      const headers = {};
      if (options.body) headers["content-type"] = "application/json";
      const resp = await fetch(path, { ...options, headers });
      const body = await resp.json();
      if (!resp.ok) throw new Error(body.error || "Request failed");
      return body;
    },

    async refresh() {
      try {
        const data = await this.api("/api/status");
        this.stream = data.stream || this.stream;
        this.config = data.config;
        this.youtube = data.youtube || this.youtube;
        this.scheduler = data.scheduler || null;
        this.nextEvents = data.nextEvents || [];
        this.presets = data.presets || [];
        this.confidence = data.confidence || [];
        this.adaptive = data.adaptive || this.adaptive;
        this.health = data.health || {};
        this.activeBroadcastId = data.activeBroadcastId || "";

        if (data.config && (!this._configLoaded || !this.videoSourceValue)) {
          this.syncConfigToForm(data.config);
        }
      } catch (e) {
        this.showToast("Connection error: " + e.message);
      }
    },

    async scanDevices(force) {
      try {
        const url = `/api/devices${force ? "?refresh=1" : ""}`;
        const data = await this.api(url);
        this.devices = data;
        // Re-resolve the video source value by device name in case
        // AVFoundation indexes shifted since last scan.
        if (this.config?.input?.videoDeviceName && (data.video || []).length > 0) {
          const resolved = this.encodeSourceValue(this.config.input);
          if (resolved && resolved !== this.videoSourceValue) {
            this.videoSourceValue = resolved;
          }
        }
        this.$nextTick(() => this.syncSelectElements());
        const v = (data.video || []).length;
        const a = (data.audio || []).length;
        this.deviceStatusText = v === 0
          ? "No video devices detected. Connect a camera or capture card and click Refresh."
          : `${v} video, ${a} audio detected.`;
      } catch (_) {}
    },

    async loadSchedules() {
      try {
        const data = await this.api("/api/schedules");
        this.schedules = data || [];
      } catch (_) {}
    },

    async loadOverrides() {
      try {
        const data = await this.api("/api/overrides");
        this.overrides = data || [];
      } catch (_) {}
    },

    // ============================================================
    // PREVIEW (WebRTC, imperative)
    //
    // The server-side swaps the RTP source between the preview's own ffmpeg
    // (idle) and the main stream's preview output (live). The UI reconnects
    // on the transition to live so the browser picks up the pipe feed.
    // ============================================================
    togglePreview() {
      this.previewVisible = !this.previewVisible;
      this.previewSuppressed = !this.previewVisible;
      if (this.previewVisible) {
        this.maybeStartPreview();
      } else {
        this.stopPreviewPC();
        this.previewError = "";
      }
    },

    maybeStartPreview() {
      if (!this.previewVisible) return;
      if (this._previewPC || this._previewStarting) return;
      this.startPreview();
    },

    refreshPreview() {
      this.previewError = "";
      this.stopPreviewPC();
      this.$nextTick(() => this.startPreview());
    },

    async startPreview() {
      if (this._previewStarting) return;
      this._previewStarting = true;
      try {
        this.stopPreviewPC();

        const pc = new RTCPeerConnection();
        this._previewPC = pc;
        pc.addTransceiver("video", { direction: "recvonly" });
        pc.addTransceiver("audio", { direction: "recvonly" });
        this.startAudioMeter(pc);
        pc.ontrack = (e) => this.handlePreviewTrack(e);

        // Connect timeout only — once connected, we don't tear down on
        // transient ICE disconnects. The transient state is recoverable
        // and reconnecting would cause visible video stutter.
        let connected = false;
        clearTimeout(this._previewTimeout);
        this._previewTimeout = setTimeout(() => {
          if (!connected && this._previewPC === pc) {
            this.previewError = "Could not connect to preview. Click Refresh to try again.";
            this.stopPreviewPC();
          }
        }, 10000);

        pc.oniceconnectionstatechange = () => {
          if (pc.iceConnectionState === "connected" || pc.iceConnectionState === "completed") {
            connected = true;
            clearTimeout(this._previewTimeout);
          } else if (pc.iceConnectionState === "failed" || pc.iceConnectionState === "closed") {
            if (this._previewPC === pc) {
              this.previewError = "Preview disconnected. Click Refresh.";
              this.stopPreviewPC();
            }
          }
        };

        const offer = await pc.createOffer();
        await pc.setLocalDescription(offer);
        const resp = await fetch("/api/preview/webrtc/offer", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(pc.localDescription),
        });
        if (!resp.ok) {
          const t = await resp.text();
          throw new Error(t || `HTTP ${resp.status}`);
        }
        const answer = await resp.json();
        await pc.setRemoteDescription(answer);
      } catch (e) {
        this.previewError = `Preview connection failed: ${e.message}`;
        this.stopPreviewPC();
      } finally {
        this._previewStarting = false;
      }
    },

    stopPreviewPC() {
      clearTimeout(this._previewTimeout);
      this.stopAudioMeter();
      if (this._previewPC) {
        try { this._previewPC.close(); } catch (_) {}
        this._previewPC = null;
      }
      const v = this.$refs.previewVideo;
      if (v) v.srcObject = null;
    },

    handlePreviewTrack(e) {
      const stream = e.streams && e.streams[0] ? e.streams[0] : new MediaStream([e.track]);
      if (e.track.kind === "video") {
        const v = this.$refs.previewVideo;
        if (v) v.srcObject = stream;
      }
      if (e.track.kind === "audio") {
        this.attachAudioMeterTrack(e.track);
      }
    },

    startAudioMeter(pc) {
      this.stopAudioMeter();
      this.audioMeterLevel = 0;
      this.audioMeterPeak = 0;
      this.audioMeterText = "Waiting";
      this.audioMeterActive = false;
      this._audioMeterPeakHold = 0;
      this._audioMeterFallbackLevel = null;

      // Stats polling loop (runs every 500ms as a fallback if Web Audio is suspended)
      this._audioMeterInterval = setInterval(async () => {
        if (!pc || this._previewPC !== pc) return;
        try {
          const stats = await pc.getStats();
          if (this._previewPC !== pc) return;
          let statsLevel = null;
          stats.forEach((report) => {
            if (report.type !== "inbound-rtp") return;
            if (report.kind !== "audio" && report.mediaType !== "audio") return;
            if (typeof report.audioLevel === "number") {
              statsLevel = report.audioLevel;
              return;
            }
            if (typeof report.totalAudioEnergy !== "number" || typeof report.totalSamplesDuration !== "number") return;
            if (this._audioMeterLastEnergy == null || this._audioMeterLastDuration == null) {
              this._audioMeterLastEnergy = report.totalAudioEnergy;
              this._audioMeterLastDuration = report.totalSamplesDuration;
              return;
            }
            const energyDelta = report.totalAudioEnergy - this._audioMeterLastEnergy;
            const durationDelta = report.totalSamplesDuration - this._audioMeterLastDuration;
            this._audioMeterLastEnergy = report.totalAudioEnergy;
            this._audioMeterLastDuration = report.totalSamplesDuration;
            if (energyDelta >= 0 && durationDelta > 0) {
              statsLevel = Math.sqrt(energyDelta / durationDelta);
            }
          });
          if (statsLevel != null) this._audioMeterFallbackLevel = statsLevel;
        } catch (_) {}
      }, 500);

      // Fast render loop for smooth UI
      const render = () => {
        if (!pc || this._previewPC !== pc) return;
        this._audioMeterRaf = requestAnimationFrame(render);
        this.updateAudioMeterRender();
      };
      this._audioMeterRaf = requestAnimationFrame(render);
    },

    stopAudioMeter() {
      clearInterval(this._audioMeterInterval);
      cancelAnimationFrame(this._audioMeterRaf);
      this._audioMeterInterval = null;
      this._audioMeterRaf = null;
      this.stopAudioMeterGraph();
      this._audioMeterLastEnergy = null;
      this._audioMeterLastDuration = null;
      this._audioMeterPeakHold = 0;
      this._audioMeterFallbackLevel = null;
      this.audioMeterLevel = 0;
      this.audioMeterPeak = 0;
      this.audioMeterText = "No audio";
      this.audioMeterActive = false;
    },

    attachAudioMeterTrack(track) {
      this.stopAudioMeterGraph();
      const AudioContextCtor = window.AudioContext || window.webkitAudioContext;
      if (!AudioContextCtor || !track) return;
      try {
        const ctx = new AudioContextCtor();
        const source = ctx.createMediaStreamSource(new MediaStream([track]));
        const analyser = ctx.createAnalyser();
        const gain = ctx.createGain();
        analyser.fftSize = 512;
        analyser.smoothingTimeConstant = 0.25;
        gain.gain.value = 0;
        source.connect(analyser);
        analyser.connect(gain);
        gain.connect(ctx.destination); // keeps the graph active while remaining silent
        this._audioMeterContext = ctx;
        this._audioMeterSource = source;
        this._audioMeterAnalyser = analyser;
        this._audioMeterGain = gain;
        this._audioMeterData = new Uint8Array(analyser.fftSize);
        this.resumeAudioMeterContext();
      } catch (_) {
        this.stopAudioMeterGraph();
      }
    },

    resumeAudioMeterContext() {
      const ctx = this._audioMeterContext;
      if (ctx && ctx.state === "suspended") {
        ctx.resume().catch(() => {});
      }
    },

    stopAudioMeterGraph() {
      try { this._audioMeterSource?.disconnect(); } catch (_) {}
      try { this._audioMeterAnalyser?.disconnect(); } catch (_) {}
      try { this._audioMeterGain?.disconnect(); } catch (_) {}
      const ctx = this._audioMeterContext;
      if (ctx && ctx.state !== "closed") ctx.close().catch(() => {});
      this._audioMeterContext = null;
      this._audioMeterSource = null;
      this._audioMeterAnalyser = null;
      this._audioMeterGain = null;
      this._audioMeterData = null;
    },

    readAudioMeterLevel() {
      const analyser = this._audioMeterAnalyser;
      const data = this._audioMeterData;
      if (!analyser || !data) return null;
      if (this._audioMeterContext?.state === "suspended") {
        this.resumeAudioMeterContext();
        return null;
      }
      analyser.getByteTimeDomainData(data);
      let max = 0;
      for (const sample of data) {
        const val = Math.abs(sample - 128);
        if (val > max) max = val;
      }
      return max / 128;
    },

    updateAudioMeterRender() {
      try {
        let rawLevel = this.readAudioMeterLevel();
        if (rawLevel == null) rawLevel = this._audioMeterFallbackLevel;

        if (rawLevel == null) {
          this.audioMeterActive = false;
          this.audioMeterLevel = Math.max(0, this.audioMeterLevel - 0.05);
          this.audioMeterPeak = Math.max(0, this.audioMeterPeak - 0.01);
          this.audioMeterText = "Waiting";
          return;
        }

        const clamped = Math.min(1, Math.max(0, rawLevel));
        const db = clamped > 0 ? 20 * Math.log10(clamped) : -60;
        const display = Math.min(1, Math.max(0, (db + 60) / 60));
        
        // Instant attack, smooth release
        if (display > this.audioMeterLevel) {
          this.audioMeterLevel = display;
        } else {
          this.audioMeterLevel = Math.max(0, this.audioMeterLevel - 0.04);
        }
        
        if (display > this._audioMeterPeakHold) {
          this._audioMeterPeakHold = display;
        } else {
          this._audioMeterPeakHold = Math.max(0, this._audioMeterPeakHold - 0.005);
        }
        
        this.audioMeterPeak = this._audioMeterPeakHold;
        this.audioMeterText = clamped > 0 ? `${Math.round(Math.max(-60, db))} dB` : "-∞ dB";
        if (clamped > 0.001) this.audioMeterActive = true;
      } catch (_) {
        this.audioMeterActive = false;
        this.audioMeterText = "No audio";
      }
    },

    // ============================================================
    // CAPTURE SOURCE
    // ============================================================
    encodeSourceValue(input) {
      if (!input || input.kind === "test-video") return "test-video::";
      const kind = input.kind || "webcam";
      const backend = input.backend || "avfoundation";
      let device = input.videoDevice || "";
      // If a device name is persisted and devices are loaded, resolve the
      // current index by name — AVFoundation indexes can shift between boots.
      if (input.videoDeviceName && (this.devices.video || []).length > 0) {
        const match = this.devices.video.find(
          (d) => d.name === input.videoDeviceName && d.backend === backend
        );
        if (match) device = String(match.index);
      }
      return `${kind}:${backend}:${device}`;
    },
    decodeSourceValue(value) {
      if (!value) return null;
      if (value === "test-video::") return { kind: "test-video", backend: "lavfi", videoDevice: "" };
      const [kind, backend, ...rest] = value.split(":");
      return { kind, backend, videoDevice: rest.join(":") };
    },
    syncConfigToForm(config) {
      if (!config) return;
      this.selectedPreset = config.preset.id;
      this.videoSourceValue = this.encodeSourceValue(config.input);
      this.audioSourceValue = config.input.audioDevice || "";
      this.outputMode = config.outputMode || "rtmp";
      this.ingestUrl = config.ingestUrl || "";
      this.hasStreamKey = !!config.hasStreamKey;
      if (config.hlsUrl) this.hlsUrl = config.hlsUrl;
      this._configLoaded = true;
      this.$nextTick(() => this.syncSelectElements());
    },
    syncSelectElements() {
      // Native select rendering can miss an x-model value when options arrive
      // later from /api/devices. Keep the visible control aligned with state.
      if (this.$refs.videoSourceSelect) this.$refs.videoSourceSelect.value = this.videoSourceValue;
      if (this.$refs.audioSourceSelect) this.$refs.audioSourceSelect.value = this.audioSourceValue;
    },
    kindForDeviceType(type) {
      switch (type) {
        case "sdi": return "sdi";
        case "capture-card": return "hdmi";
        default: return "webcam";
      }
    },
    deviceGroups() {
      const labels = {
        camera:         "Cameras",
        "capture-card": "Capture cards (USB HDMI)",
        screen:         "Screen capture",
        sdi:            "SDI (Blackmagic DeckLink)",
      };
      const order = ["camera", "capture-card", "screen", "sdi"];
      const out = [];
      for (const t of order) {
        const matches = (this.devices.video || []).filter((d) => d.type === t);
        if (matches.length === 0) continue;
        out.push({
          label: labels[t],
          devices: matches.map((d) => ({
            kind: this.kindForDeviceType(d.type),
            backend: d.backend,
            index: d.index,
            label: d.backend === "decklink" ? d.name : `${d.name} [${d.index}]`,
          })),
        });
      }
      return out;
    },
    videoSourceOptions() {
      const labels = {
        camera:         "Cameras",
        "capture-card": "Capture cards",
        screen:         "Screen capture",
        sdi:            "SDI",
      };
      const order = ["camera", "capture-card", "screen", "sdi"];
      const out = [
        { key: "group:test", value: "__group:test", label: "Test source", disabled: true },
        { key: "test-video", value: "test-video::", label: "  Test pattern (no hardware)", disabled: false },
      ];
      for (const t of order) {
        const matches = (this.devices.video || []).filter((d) => d.type === t);
        if (matches.length === 0) continue;
        out.push({ key: `group:${t}`, value: `__group:${t}`, label: labels[t] || "Video", disabled: true });
        for (const d of matches) {
          const kind = this.kindForDeviceType(d.type);
          const label = d.backend === "decklink" ? d.name : `${d.name} [${d.index}]`;
          out.push({ key: `${kind}:${d.backend}:${d.index}`, value: `${kind}:${d.backend}:${d.index}`, label: `  ${label}`, disabled: false });
        }
      }
      return out;
    },
    audioDeviceGroups() {
      const labels = { microphone: "Microphones", "audio-input": "Audio inputs" };
      const order = ["microphone", "audio-input"];
      const out = [];
      for (const t of order) {
        const matches = (this.devices.audio || []).filter((d) => d.type === t);
        if (matches.length === 0) continue;
        out.push({ label: labels[t], devices: matches });
      }
      return out;
    },
    audioSourceOptions() {
      const labels = { microphone: "Microphones", "audio-input": "Audio inputs" };
      const order = ["microphone", "audio-input"];
      const out = [{ key: "silent", value: "", label: this.isSDISource ? "Embedded SDI audio" : "None / silent", disabled: false }];
      for (const t of order) {
        const matches = (this.devices.audio || []).filter((d) => d.type === t);
        if (matches.length === 0) continue;
        out.push({ key: `group:${t}`, value: `__group:${t}`, label: labels[t] || "Audio", disabled: true });
        for (const d of matches) {
          out.push({ key: `${t}:${d.index}`, value: d.index, label: `  ${d.name} [${d.index}]`, disabled: false });
        }
      }
      return out;
    },
    onVideoSourceChange() {
      // If we switched to SDI, clear external audio (embedded SDI audio is used).
      if (this.isSDISource) this.audioSourceValue = "";
      this.saveConfig();
      this.refreshPreview();
    },

    // ============================================================
    // CONFIG SAVE
    // ============================================================
    async saveConfig() {
      if (!this.videoSourceValue && this.config?.input) {
        this.videoSourceValue = this.encodeSourceValue(this.config.input);
        this.audioSourceValue = this.config.input.audioDevice || this.audioSourceValue || "";
        this.syncSelectElements();
      }
      const decoded = this.decodeSourceValue(this.videoSourceValue);
      if (!decoded) {
        this.showToast("Pick a video source before saving.");
        return false;
      }
      const payload = {
        presetId: this.selectedPreset,
        ingestUrl: (this.ingestUrl || "").trim(),
        outputMode: this.outputMode,
        input: {
          kind: decoded.kind,
          backend: decoded.backend,
          videoDevice: decoded.videoDevice,
          audioDevice: this.audioSourceValue || "",
        },
      };
      // Persist device names so the backend can resolve stable AVFoundation
      // indexes by name even when indexes shift between reboots/replugs.
      const vDev = (this.devices.video || []).find(
        (d) => String(d.index) === String(decoded.videoDevice) && d.backend === decoded.backend
      );
      if (vDev) payload.input.videoDeviceName = vDev.name;
      const aDev = (this.devices.audio || []).find(
        (d) => String(d.index) === String(this.audioSourceValue)
      );
      if (aDev) payload.input.audioDeviceName = aDev.name;
      const key = (this.streamKey || "").trim();
      if (key) payload.streamName = key;
      try {
        const result = await this.api("/api/config", { method: "POST", body: JSON.stringify(payload) });
        this.config = result;
        this.selectedPreset = result.preset.id;
        this.ingestUrl = result.ingestUrl || "";
        this.streamKey = "";
        this.hasStreamKey = !!result.hasStreamKey;
        this.outputMode = result.outputMode || "rtmp";
        if (result.hlsUrl) this.hlsUrl = result.hlsUrl;
        return true;
      } catch (e) {
        this.showToast("Save failed: " + e.message);
        return false;
      }
    },

    // ============================================================
    // PRESETS
    // ============================================================
    presetTitle(p) {
      const fps = p.fps === 60 ? "60" : "";
      return `${p.name} · ${p.height}p${fps} · ${p.videoKbps / 1000} Mbps`;
    },
    presetName(id) {
      return this.presets.find((p) => p.id === id)?.name || id;
    },
    selectPreset(id) {
      this.selectedPreset = id;
      this.presetMenuOpen = false;
      this.saveConfig().then(() => this.showToast("Quality saved"));
    },

    // ============================================================
    // STREAM CONTROLS
    // ============================================================
    async startNow() {
      try {
        if (!(await this.saveConfig())) return;
        await this.api("/api/start", { method: "POST" });
        this.showToast("Starting...");
        await this.refresh();
      } catch (e) {
        this.showToast(e.message);
      }
    },

    openYTModal() {
      this.ytModal.warn = "";
      this.ytModal.open = true;
    },
    openCustomModal() {
      this.customModal.warn = "";
      this.customModal.open = true;
    },

    async goLiveYouTube() {
      this.ytModal.busy = true;
      this.ytModal.warn = "";
      try {
        if (!(await this.saveConfig())) return;
        await this.api("/api/youtube/go-live-now", {
          method: "POST",
          body: JSON.stringify({ title: (this.ytModal.title || "Live Stream").trim(), privacy: this.ytModal.privacy }),
        });
        this.ytModal.open = false;
        this.showToast("Broadcast created — going live...");
        await this.refresh();
      } catch (e) {
        this.ytModal.warn = e.message;
      } finally {
        this.ytModal.busy = false;
      }
    },

    async startCustom() {
      this.customModal.busy = true;
      this.customModal.warn = "";
      try {
        if (!(await this.saveConfig())) return;
        await this.api("/api/start", { method: "POST" });
        this.customModal.open = false;
        this.showToast("Stream starting...");
        await this.refresh();
      } catch (e) {
        this.customModal.warn = e.message;
      } finally {
        this.customModal.busy = false;
      }
    },

    showStopConfirm() {
      this.stopConfirm = true;
      clearTimeout(this._stopTimer);
      this._stopTimer = setTimeout(() => { this.stopConfirm = false; }, 5000);
    },
    async doStop() {
      clearTimeout(this._stopTimer);
      this.stopConfirm = false;
      try {
        await this.api("/api/stop", { method: "POST" });
        await this.refresh();
      } catch (e) {
        this.showToast(e.message);
      }
    },

    async extend() {
      try {
        const result = await this.api("/api/extend", { method: "POST", body: JSON.stringify({ minutes: 15 }) });
        const endsAt = new Date(result.endsAt).toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
        this.showToast(`Extended — now ends at ${endsAt}`);
        await this.refresh();
      } catch (e) {
        this.showToast(e.message);
      }
    },

    async setAdaptive(enabled) {
      try {
        await this.api("/api/adaptive", { method: "POST", body: JSON.stringify({ enabled }) });
        this.adaptive.enabled = enabled;
        this.showToast(enabled ? "Auto-quality enabled" : "Auto-quality disabled");
      } catch (e) {
        this.showToast(e.message);
      }
    },

    // ============================================================
    // YOUTUBE OAUTH
    // ============================================================
    async loginYouTube() {
      try {
        const data = await this.api("/api/youtube/auth/url");
        if (data.url) window.open(data.url, "_blank", "width=600,height=700");
      } catch (e) {
        this.showToast("YouTube login failed: " + e.message);
      }
    },
    async logoutYouTube() {
      await this.api("/api/youtube/auth/logout", { method: "POST" });
      await this.refresh();
    },

    // ============================================================
    // SCHEDULES
    // ============================================================
    _blankSchedForm() {
      return {
        name: "", days: [], time: "08:45", timezone: "America/Chicago",
        durationMin: 120, title: "", description: "", privacy: "unlisted",
      };
    },
    _blankOvrForm() {
      return {
        name: "", wallClock: "", timezone: "America/Chicago", durationMin: 120,
        title: "", description: "", privacy: "unlisted",
      };
    },
    openSchedForm() {
      this.schedForm = this._blankSchedForm();
      this.schedFormOpen = true;
    },
    toggleDay(day) {
      const idx = this.schedForm.days.indexOf(day);
      if (idx < 0) this.schedForm.days.push(day);
      else this.schedForm.days.splice(idx, 1);
    },
    async saveSchedule() {
      if (this.schedForm.days.length === 0) {
        alert("Select at least one day.");
        return;
      }
      const sched = {
        ...this.schedForm,
        presetId: this.selectedPreset,
        title: (this.schedForm.title || this.schedForm.name).trim(),
        name: this.schedForm.name.trim(),
        description: this.schedForm.description.trim(),
        enabled: true,
      };
      try {
        await this.api("/api/schedules", { method: "POST", body: JSON.stringify(sched) });
        this.schedFormOpen = false;
        this.showToast("Schedule created.");
        await this.loadSchedules();
        await this.refresh();
      } catch (e) {
        alert(e.message);
      }
    },
    async deleteSchedule(id) {
      if (!confirm("Delete this schedule?")) return;
      await this.api(`/api/schedules/${id}`, { method: "DELETE" });
      await this.loadSchedules();
      await this.refresh();
    },

    openOvrForm() {
      this.ovrForm = this._blankOvrForm();
      this.ovrFormOpen = true;
    },
    async saveOverride() {
      if (!this.ovrForm.wallClock) {
        alert("Pick a date and time.");
        return;
      }
      const override = {
        ...this.ovrForm,
        presetId: this.selectedPreset,
        title: (this.ovrForm.title || this.ovrForm.name).trim(),
        name: this.ovrForm.name.trim(),
        description: this.ovrForm.description.trim(),
      };
      try {
        await this.api("/api/overrides", { method: "POST", body: JSON.stringify(override) });
        this.ovrFormOpen = false;
        this.showToast("Special event created.");
        await this.loadOverrides();
        await this.refresh();
      } catch (e) {
        alert(e.message);
      }
    },
    async deleteOverride(id) {
      if (!confirm("Delete this event?")) return;
      await this.api(`/api/overrides/${id}`, { method: "DELETE" });
      await this.loadOverrides();
      await this.refresh();
    },

    // ============================================================
    // QUICK FILL + HLS COPY
    // ============================================================
    quickFill(platform) {
      const URLS = {
        youtube: "rtmps://a.rtmps.youtube.com/live2",
        cloudflare: "rtmps://live.cloudflare.com:443/live/",
        twitch: "rtmp://live.twitch.tv/app",
      };
      const url = URLS[platform];
      if (!url) return;
      this.ingestUrl = url;
      this.saveConfig();
      this.showToast(`${platform} URL filled — paste your stream key.`);
    },
    copyHLSUrl() {
      navigator.clipboard.writeText(this.hlsUrl).then(() => this.showToast("HLS URL copied!"));
    },

    // ============================================================
    // HELPERS / FORMATTING
    // ============================================================
    fmtTime(d) {
      return d.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
    },
    formatEventWhen(d) {
      const now = new Date();
      const sameDay = d.toDateString() === now.toDateString();
      const tomorrow = new Date(now.getTime() + 86400000);
      const isTomorrow = d.toDateString() === tomorrow.toDateString();
      const timeStr = this.fmtTime(d);
      if (sameDay) return `Today at ${timeStr}`;
      if (isTomorrow) return `Tomorrow at ${timeStr}`;
      const dateStr = d.toLocaleDateString(undefined, { weekday: "long", month: "short", day: "numeric" });
      return `${dateStr} at ${timeStr}`;
    },
    formatDateTime(d) {
      const dateStr = d.toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric" });
      const timeStr = d.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
      return `${dateStr} at ${timeStr}`;
    },
    formatDays(days) {
      return (days || []).map((d) => d.charAt(0).toUpperCase() + d.slice(0, 3)).join(", ");
    },
    shortTZ(tz) {
      return (tz || "").split("/").pop().replace("_", " ");
    },
    platformFromURL(url) {
      if (!url) return null;
      const u = url.toLowerCase();
      if (u.includes("youtube.com")) return "YouTube";
      if (u.includes("cloudflare.com")) return "Cloudflare";
      if (u.includes("twitch.tv")) return "Twitch";
      if (u.includes("facebook.com") || u.includes("fb.com")) return "Facebook";
      return null;
    },
    showToast(msg) {
      this.toast = msg;
      clearTimeout(this._toastTimer);
      this._toastTimer = setTimeout(() => { this.toast = ""; }, 3000);
    },
  }));
});
