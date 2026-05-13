// EasyStream — Alpine.js component.
//
// State model:
//   - Server-sourced state lives in this.server.* (read-only from UI POV).
//     It is replaced atomically by SSE pushes, never patched in place.
//   - The "form" inputs (selectedPreset, ingestUrl, etc.) mirror server
//     state but track their own dirty flag so an SSE push from another
//     tab doesn't clobber what the user is actively typing.
//   - Pure formatting helpers live in format.js; capture-source helpers
//     in sources.js; WebRTC preview in preview.js.
//
// Real-time:
//   /api/stream/state is an SSE stream. We subscribe once on init and
//   replace local state when frames arrive. A small "safety net" full
//   reload every 30 s catches any pathological cases.
const F = window.EasyStreamFormat;
const S = window.EasyStreamSources;

document.addEventListener("alpine:init", () => {
  Alpine.data("app", () => ({

    // -------- Server-sourced state (replaced wholesale by SSE) --------
    stream:            { state: "idle", lastProgress: {}, restartCount: 0 },
    app:               { version: "dev" },
    config:            null,
    youtube:           { authenticated: false, configured: false, channelName: "" },
    scheduler:         null,
    nextEvents:        [],
    presets:           [],
    confidence:        [],
    adaptive:          { enabled: true, isFallback: false },
    health:            {},
    activeBroadcastId: "",
    devices:           { video: [], audio: [] },
    schedules:         [],
    overrides:         [],
    encoders:          [],
    capabilities:      { srt: true }, // assume yes until /api/status disabuses us
    // Always-on SRT receiver state. Populated when the saved source
    // is srt-listener; the pre-flight Video pill keys off this
    // independently of the main stream supervisor so the operator
    // sees "OBS connected — receiving 30 fps" before pressing Go Live.
    ingest:            { state: "idle", peerConnected: false, fps: 0, port: 0 },

    // -------- UI state --------
    view:            "dashboard",    // "dashboard" | "settings"
    settingsTab:     "source",       // "source" | "quality" | "youtube" | "schedule" | "destination"
    presetMenuOpen:  false,
    schedFormOpen:   false,
    ovrFormOpen:     false,
    stopConfirm:     false,
    advancedOpen:    false,
    toast:           "",
    deviceStatusText: "Scanning for devices...",
    connectionOK:    true,
    nowTick:         Date.now(),     // refreshed every second for live timers

    ytModal:     { open: false, title: "Live Stream", privacy: "unlisted", warn: "", busy: false },
    customModal: { open: false, warn: "", busy: false },

    // Preview controller (instance assigned in init)
    preview:        null,
    previewVisible: true,
    previewError:   "",
    audioMeterLevel:  0,
    audioMeterPeak:   0,
    audioMeterText:   "No audio",
    audioMeterActive: false,

    // Form mirror — kept in sync with server unless dirty=true.
    videoSourceValue: "",
    audioSourceValue: "",
    networkUrl:       "",
    networkNoAudio:   false,
    sourceIsHDR:      false,
    // SRT listener mode: EasyStream binds a local port and waits
    // for an upstream encoder to push to it.
    srtListenPort:       9999,
    srtListenPassphrase: "",
    // hasSavedSRTPassphrase reflects "the server has a passphrase
    // stored" without the UI ever learning its value. Set from the
    // sentinel the API returns in place of the real passphrase
    // (RedactedCredentialSentinel). Used to drive the placeholder so
    // the operator sees "set — leave blank to keep" instead of an
    // empty field that would imply no encryption.
    hasSavedSRTPassphrase: false,
    localIPs:            [],
    // Operator-chosen host IP for the rendered publish URL. Empty
    // means "use whatever localIPs[0] resolves to", which is the
    // common case. Stored only in this tab — not persisted, since
    // it depends on where the encoder is, not on the EasyStream
    // config itself.
    srtSelectedHostIP:   "",
    selectedPreset:   "recommended",
    selectedEncoder:  "libx264",
    outputMode:       "rtmp",
    ingestUrl:        "",
    streamKey:        "",
    hasStreamKey:     false,
    enableHls:        false,
    hlsUrl:           "http://127.0.0.1:8080/hls/stream.m3u8",

    // Dirty flags — set when the user is actively editing a field.
    // SSE state pushes respect these so we never overwrite a key the
    // user is typing. Cleared after a save round-trip.
    _dirtyIngest:    false,
    _dirtyStreamKey: false,
    _dirtyAudio:     false,
    _dirtyVideo:     false,
    // Passphrase needs its own dirty flag — _dirtyVideo gets cleared
    // by every save, but the server never echoes the passphrase back
    // (write-only), so reusing _dirtyVideo would let the next SSE
    // push overwrite a freshly-saved passphrase with "".
    _dirtySRTPass:   false,

    // Tracks the previous videoSourceValue so onVideoSourceChange
    // can detect a kind transition (e.g. webcam → network) and
    // reset kind-specific defaults. Synced both on user-driven
    // changes (in onVideoSourceChange) and on SSE-driven updates
    // (in applyState) so the baseline never drifts.
    _lastVideoSourceValue: "",

    schedForm: { id: "", name: "", days: [], time: "09:00", timezone: "America/Chicago", durationMin: 120, prepLeadMinutes: 0, title: "", description: "", privacy: "unlisted", enabled: true },
    ovrForm:   { id: "", name: "", wallClock: "", timezone: "America/Chicago", durationMin: 120, prepLeadMinutes: 0, title: "", description: "", privacy: "unlisted" },

    // Internal
    _eventSource:   null,
    _savePending:   null,
    _toastTimer:    null,
    _stopTimer:     null,
    _safetyPoll:    null,
    _nowTimer:      null,
    _wasLive:       false,

    dayList: [
      { value: "sunday",    label: "Sun", fullLabel: "Sunday" },
      { value: "monday",    label: "Mon", fullLabel: "Monday" },
      { value: "tuesday",   label: "Tue", fullLabel: "Tuesday" },
      { value: "wednesday", label: "Wed", fullLabel: "Wednesday" },
      { value: "thursday",  label: "Thu", fullLabel: "Thursday" },
      { value: "friday",    label: "Fri", fullLabel: "Friday" },
      { value: "saturday",  label: "Sat", fullLabel: "Saturday" },
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

    // ============================================================
    // INIT
    // ============================================================
    async init() {
      // Pre-fill schedule timezone from the browser if it's one we list.
      try {
        const browserTZ = Intl.DateTimeFormat().resolvedOptions().timeZone;
        if (this.tzList.some((t) => t.value === browserTZ)) {
          this.schedForm.timezone = browserTZ;
          this.ovrForm.timezone = browserTZ;
        }
      } catch (_) {}

      // One-shot fetches for things not pushed via SSE.
      await this.scanEncoders();
      await this.scanDevices();

      // Connect to SSE — replaces 2-second polling entirely.
      this.connectSSE();

      // Safety-net active probe every 30 s. ensureSSEHealthy() checks
      // readyState rather than the soft connectionOK flag — a half-open
      // socket whose error never fired (mobile background eviction)
      // would otherwise sit stale.
      this._safetyPoll = setInterval(() => this.ensureSSEHealthy(), 30000);

      // Mobile browsers throttle and often kill SSE on background. When
      // the tab returns, reconnect immediately so we don't show stale
      // data while waiting for the safety poll.
      document.addEventListener("visibilitychange", () => {
        if (!document.hidden) this.ensureSSEHealthy();
      });

      // Live-timer tick. Only running when a uptime/countdown is
      // visible — gates on isLive or having a next event.
      this.updateNowTimer();
      this.$watch("isLive",         () => this.updateNowTimer());
      this.$watch("nextEvent",      () => this.updateNowTimer());

      // Watch isLive transitions to update title, favicon, preview, and
      // any in-flight stop confirmation. Only act on real transitions
      // (Alpine $watch fires on every reassignment of stream).
      this._wasLive = this.isLive;
      this.$watch("isLive", (now, prev) => {
        if (now === prev) return;
        this._wasLive = now;
        document.title = now ? "● LIVE · EasyStream" : "EasyStream";
        const favicon = document.querySelector("#favicon");
        if (favicon) {
          favicon.href = now
            ? "data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Crect width='16' height='16' fill='%230d1117'/%3E%3Ccircle cx='8' cy='8' r='5' fill='%23ff1a1a'/%3E%3C/svg%3E"
            : "data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Crect width='16' height='16' fill='%230d1117'/%3E%3Cpath d='M5 4 L12 8 L5 12 Z' fill='%232f81f7'/%3E%3C/svg%3E";
        }
        // If the stream stops while the operator was mid-confirm, drop
        // the confirm state — the banner is about to unmount.
        if (!now) {
          this.stopConfirm = false;
          clearTimeout(this._stopTimer);
        }
        // After main-stream FFmpeg or preview FFmpeg has swapped over,
        // re-establish the preview PeerConnection so packets flow. Only
        // do this when the preview controller already has a connection
        // — the init path handles cold start.
        if (this.previewVisible && this.preview?.isRunning()) {
          setTimeout(() => this.preview?.refresh(), 1500);
        }
      });

      // Preview controller — bridges WebRTC + audio meter into reactive state.
      this.preview = window.EasyStreamPreview.create({
        videoEl: this.$refs.previewVideo,
        onAudioLevel: (level, peak, text, active) => {
          this.audioMeterLevel = level;
          this.audioMeterPeak = peak;
          this.audioMeterText = text;
          this.audioMeterActive = active;
        },
        onError: (msg) => { this.previewError = msg; },
      });
      window.addEventListener("pointerdown", () => this.preview?.resumeAudio(), { passive: true });
      window.addEventListener("keydown",      () => this.preview?.resumeAudio());
      window.addEventListener("beforeunload", () => this.preview?.stop());

      // Initial preview start. The $watch(isLive) above only acts on
      // transitions; the very first paint needs an explicit start.
      // preview.start() is idempotent (a `starting` flag inside the
      // controller prevents the well-known double-fire).
      if (this.previewVisible) this.$nextTick(() => this.preview?.start());
    },

    // ============================================================
    // SSE — server-sent state
    // ============================================================
    connectSSE() {
      // Null out the prior listener BEFORE close so a delayed onerror
      // from the old connection can't flip connectionOK against the new one.
      if (this._eventSource) {
        const prev = this._eventSource;
        prev.onerror = null;
        try { prev.close(); } catch (_) {}
        this._eventSource = null;
      }
      const es = new EventSource("/api/stream/state");
      this._eventSource = es;
      es.addEventListener("state",     (e) => { if (this._eventSource === es) { this.connectionOK = true; this.applyState(JSON.parse(e.data)); }});
      es.addEventListener("schedules", (e) => { if (this._eventSource === es) { this.connectionOK = true; this.schedules = JSON.parse(e.data) || []; }});
      es.addEventListener("overrides", (e) => { if (this._eventSource === es) { this.connectionOK = true; this.overrides = JSON.parse(e.data) || []; }});
      es.addEventListener("open",      ()  => { if (this._eventSource === es) { this.connectionOK = true; }});
      es.onerror = () => {
        // EventSource auto-reconnects with the `retry:` hint. Mark
        // disconnected so the topbar pill turns yellow.
        if (this._eventSource === es) this.connectionOK = false;
      };
    },

    // ensureSSEHealthy is called from the safety-poll interval and on
    // visibilitychange. It actively probes EventSource.readyState rather
    // than relying on a connectionOK flag that a half-open connection
    // may never have cleared.
    ensureSSEHealthy() {
      const es = this._eventSource;
      if (!es || es.readyState === EventSource.CLOSED) {
        this.connectSSE();
      }
    },

    applyState(data) {
      // Replace, don't merge — server is authoritative.
      this.stream            = data.stream     || this.stream;
      this.app               = data.app        || this.app;
      this.config            = data.config     || this.config;
      this.youtube           = data.youtube    || this.youtube;
      this.scheduler         = data.scheduler  || null;
      this.nextEvents        = data.nextEvents || [];
      this.presets           = data.presets    || [];
      this.confidence        = data.confidence || [];
      this.adaptive          = data.adaptive   || this.adaptive;
      this.health            = data.health     || {};
      this.capabilities      = data.capabilities || this.capabilities;
      this.activeBroadcastId = data.activeBroadcastId || "";
      this.localIPs          = data.localIPs || [];
      this.ingest            = data.ingest || { state: "idle", peerConnected: false, fps: 0, port: 0 };
      this.syncFormFromConfig(data.config);
    },

    syncFormFromConfig(config) {
      if (!config) return;
      // Preset + encoder are non-dirtyable: there's no free-text input,
      // so server is always authoritative.
      this.selectedPreset  = config.preset?.id || this.selectedPreset;
      this.selectedEncoder = config.encoder || "libx264";
      this.hasStreamKey    = !!config.hasStreamKey;
      // Migrate legacy outputMode=hls (older daemons or saved configs)
      // to the new shape: primary=rtmp + enableHls=true.
      let mode = config.outputMode || "rtmp";
      if (mode === "hls") { mode = "rtmp"; this.enableHls = true; }
      this.outputMode      = mode;
      this.enableHls       = !!config.enableHls || this.enableHls;
      if (config.hlsUrl) this.hlsUrl = config.hlsUrl;
      // Free-text fields: respect dirty flag so an SSE push doesn't
      // overwrite what the user is actively typing or selecting.
      if (!this._dirtyIngest) this.ingestUrl = config.ingestUrl || "";
      // streamKey is write-only: server never sends it back.

      // Re-resolve the video source by name in case AVFoundation indexes
      // shifted. Skip the re-encode when the current selection already
      // decodes to the same kind/backend/device — avoids dropdown flicker
      // when two devices share a name and .find() picks the first one.
      if (!this._dirtyVideo) {
        const encoded = S.encodeSourceValue(config.input, this.devices);
        if (encoded && encoded !== this.videoSourceValue) {
          // Only accept the new value if it actually decodes differently.
          const cur = S.decodeSourceValue(this.videoSourceValue);
          const next = S.decodeSourceValue(encoded);
          const sameTriple = cur && next &&
            cur.kind === next.kind && cur.backend === next.backend && cur.videoDevice === next.videoDevice;
          if (!sameTriple) {
            this.videoSourceValue = encoded;
            // Keep _lastVideoSourceValue in sync with programmatic
            // changes too — otherwise onVideoSourceChange's kind-
            // transition detection would compare against a stale
            // "" baseline and falsely fire the network-default
            // reset on the next user-initiated change.
            this._lastVideoSourceValue = encoded;
            this.$nextTick(() => this.syncSelectElements());
          }
        }
      }
      if (!this._dirtyAudio) {
        this.audioSourceValue = config.input?.audioDevice || "";
      }
      // Network + SRT-listener: hydrate fields from server (unless
      // operator is actively typing).
      if (!this._dirtyVideo) {
        this.networkUrl = config.input?.url || "";
        this.networkNoAudio = !!config.input?.noAudio;
        this.sourceIsHDR = !!config.input?.sourceIsHdr;
        this.srtListenPort = config.input?.srtListenPort || 9999;
      }
      // Passphrase is write-only on the wire. The server replies
      // with the literal sentinel "REDACTED" when a passphrase is
      // stored, and "" otherwise — never the actual value. We
      // track only the "set or not" bit; the input field itself is
      // never touched by hydration so a freshly-typed value stays
      // visible (and stays in the rendered publish URL the operator
      // is about to paste into OBS). Cleared explicitly on kind
      // transition away from srt-listener, not here.
      const serverPass = config.input?.srtListenPassphrase || "";
      this.hasSavedSRTPassphrase = serverPass === "REDACTED";
    },

    // ============================================================
    // API + one-shot fetches
    // ============================================================
    async api(path, options = {}) {
      const headers = {};
      if (options.body) headers["content-type"] = "application/json";
      const resp = await fetch(path, { ...options, headers });
      const body = await resp.json();
      if (!resp.ok) throw new Error(body.error || "Request failed");
      return body;
    },

    async scanDevices(force) {
      try {
        const url = `/api/devices${force ? "?refresh=1" : ""}`;
        const data = await this.api(url);
        this.devices = data;
        const v = (data.video || []).length;
        const a = (data.audio || []).length;
        this.deviceStatusText = v === 0
          ? "No video devices detected. Connect a camera or capture card and click Refresh."
          : `${v} video, ${a} audio detected.`;
        // Resolve source value by name now that device list is loaded.
        if (this.config?.input) this.syncFormFromConfig(this.config);
      } catch (e) {
        this.deviceStatusText = "Device scan failed.";
      }
    },

    async scanEncoders() {
      try {
        const data = await this.api("/api/encoders");
        this.encoders = (data || []).filter((e) => e.available);
      } catch (_) {}
    },

    // ============================================================
    // COMPUTED — derived from state
    // ============================================================
    get isLive() {
      return ["starting", "running", "degraded", "restarting"].includes(this.stream.state);
    },
    get videoOK() { return !!this.videoSourceValue; },
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
    get activeEventName() { return this.scheduler?.activeEventName || ""; },
    get liveHeadline()    { return this.activeEventName || "Live stream"; },
    get liveUptime() {
      // Touch nowTick so this getter re-runs every second.
      void this.nowTick;
      return F.elapsedSince(this.startedAt);
    },
    get liveEndsAt()      { return this.scheduleEndsAt ? F.fmtTime(this.scheduleEndsAt) : null; },
    get nextEvent()       { return this.nextEvents[0] || null; },
    get nextEventCountdown() {
      if (!this.nextEvent) return "";
      // Read nowTick so this re-evaluates every second.
      void this.nowTick;
      return F.relativeUntil(new Date(this.nextEvent.startTime));
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
    get showExtendButton() { return !!this.activeEventName; },
    get bitrateText() {
      const k = this.stream.lastProgress?.bitrateKbps;
      return k ? `${Math.round(k)} kbps` : "-";
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
    get problemVisible() {
      return ["degraded", "restarting", "failed"].includes(this.stream.state);
    },
    get idleLabel() {
      return {
        idle: "Ready", stopping: "Stopping", failed: "Stopped",
      }[this.stream.state] || "Ready";
    },
    get idleDetail() {
      if (this.stream.state === "stopping") return "Stopping encoder...";
      if (this.stream.state === "failed") return this.stream.lastError || this.stream.lastExit || "Last attempt did not complete.";
      return "Ready to stream.";
    },
    get currentPreset() { return this.presets.find((p) => p.id === this.selectedPreset); },
    get isSDISource()        { return S.decodeSourceValue(this.videoSourceValue)?.kind === "sdi"; },
    get isNetworkSource()    { return S.decodeSourceValue(this.videoSourceValue)?.kind === "network"; },
    get isSRTListenerSource() { return S.decodeSourceValue(this.videoSourceValue)?.kind === "srt-listener"; },
    // activeSRTHostIP returns the IP that should appear in the
    // rendered publish URL: the operator's explicit pick if it's
    // still in the detected list, otherwise the first detected IP.
    // If the chosen IP disappears (e.g. interface dropped between
    // SSE pushes), fall through to the first available so the URL
    // never references a stale value.
    get activeSRTHostIP() {
      const ips = this.localIPs || [];
      if (this.srtSelectedHostIP && ips.includes(this.srtSelectedHostIP)) {
        return this.srtSelectedHostIP;
      }
      return ips[0] || "";
    },
    // The URL operators hand the upstream encoder so it knows where
    // to push. Uses activeSRTHostIP — the operator's pick when they
    // clicked an alternate IP chip, otherwise the first detected
    // address. If interface enumeration found nothing (rare: VPN-
    // only host, sandbox), returns "" so the template can swap in
    // a helpful "find your IP" message instead.
    get srtListenerPublishURL() {
      const ip = this.activeSRTHostIP;
      if (!ip) return "";
      const port = this.srtListenPort || 9999;
      const pass = (this.srtListenPassphrase || "").trim();
      return pass
        ? `srt://${ip}:${port}?passphrase=${encodeURIComponent(pass)}`
        : `srt://${ip}:${port}`;
    },
    get hasSRTPublishURL() {
      return !!this.srtListenerPublishURL;
    },
    // Inline length validation so the operator sees the SRT spec rule
    // (10-79 chars) as soon as they're in the invalid band, not after
    // the backend save round-trips an error. Empty is valid (= no
    // encryption). The input also carries maxlength=79 so typing past
    // the upper bound is blocked at the keyboard level; the upper-
    // bound branch below is a belt-and-suspenders catch for paste
    // operations that route around maxlength on some browsers.
    get srtPassphraseLengthError() {
      const v = this.srtListenPassphrase || "";
      if (v.length === 0) return "";
      if (v.length < 10) {
        return `Too short — SRT requires 10–79 characters (currently ${v.length}).`;
      }
      if (v.length > 79) {
        return `Too long — SRT requires 10–79 characters (currently ${v.length}).`;
      }
      return "";
    },
    selectSRTHostIP(ip) {
      this.srtSelectedHostIP = ip;
    },
    copySRTPublishURL() {
      const u = this.srtListenerPublishURL;
      if (!u) return;
      window.EasyStreamClipboard(u).then((ok) => {
        this.showToast(ok ? "Publish URL copied!" : "Copy failed — select the URL manually.");
      });
    },
    get destinationLabel() {
      if (this.activeBroadcastId) return "YouTube";
      if (this.outputMode === "hls") return "Local HLS";
      return F.platformFromURL(this.ingestUrl) || "Custom RTMP";
    },
    get customDestLabel() {
      if (this.outputMode === "hls") return `Local HLS · ${this.hlsUrl}`;
      const platform = F.platformFromURL(this.ingestUrl);
      return platform ? `${platform} · ${this.ingestUrl}` : (this.ingestUrl || "(not set)");
    },

    // Pre-flight status rows. Color + icon + text so a single channel
    // failure (e.g. color-blind operator on a sanctuary projector) never
    // hides the signal.
    get preflightRows() {
      // Video source: verify the saved device name is actually present
      // in the live device list. If not, surface RED so the operator
      // sees the problem BEFORE the scheduled go-live silently fails
      // its preflight server-side. This is layer 1 of the
      // sticky-source defense.
      let v;
      if (!this.videoOK) {
        v = { icon: "video", label: "Video", status: "red", detail: "No video source picked" };
      } else if (this.videoSourceValue === "test-video::") {
        v = { icon: "video", label: "Video", status: "green", detail: "Test pattern" };
      } else if (this.isNetworkSource) {
        // Network sources can't be presence-checked the way hardware
        // devices can. We key off the live ffmpeg's progress: fps > 0
        // means frames are flowing. The displayed URL is the SERVER-
        // confirmed one (config.input.url, redacted by the backend)
        // when available, falling back to the form mirror only when
        // the server hasn't seen this URL yet — that way the pill
        // doesn't show a half-typed URL while ffmpeg is pulling the
        // older saved one.
        const formUrl = (this.networkUrl || "").trim();
        const serverUrl = this.config?.input?.url || "";
        const effectiveUrl = serverUrl || formUrl;
        const liveUrl = F.redactUrl(effectiveUrl);
        // Coerce fps to a number — JSON consumers occasionally send
        // it as a string ("30.000") and .toFixed would throw.
        const fpsRaw = this.stream?.lastProgress?.fps;
        const fps = Number(fpsRaw) || 0;
        if (!effectiveUrl) {
          v = { icon: "video", label: "Video", status: "red", detail: "Enter a network URL" };
        } else if (this.stream?.state === "running") {
          if (fps > 0) {
            v = { icon: "video", label: "Video", status: "green", detail: `${liveUrl} (${fps.toFixed(0)} fps)` };
          } else {
            v = { icon: "video", label: "Video", status: "yellow",
                  detail: `${liveUrl} — connected, waiting for frames` };
          }
        } else if (this.stream?.state === "starting" || this.stream?.state === "restarting") {
          v = { icon: "video", label: "Video", status: "yellow",
                detail: `Connecting to ${liveUrl}…` };
        } else if (this.stream?.state === "failed") {
          v = { icon: "video", label: "Video", status: "red",
                detail: this.stream?.lastError || `Could not reach ${liveUrl}` };
        } else {
          v = { icon: "video", label: "Video", status: "yellow",
                detail: `${liveUrl} — not verified (start stream to check)` };
        }
      } else if (this.isSRTListenerSource) {
        // SRT listener mode is fed by the always-on ingest receiver
        // (binds the SRT port as soon as the source is saved), not
        // by the main supervisor. So the pre-flight pill keys off
        // ingest state — that's the only signal that proves an
        // upstream encoder is actually connected. Frames flow into
        // the local UDP relay BEFORE the operator presses Go Live,
        // and the preview shows them: by the time they press Start,
        // they've already verified the feed.
        const port = this.ingest?.port || Number(this.config?.input?.srtListenPort) || this.srtListenPort || 9999;
        const fpsRaw = this.ingest?.fps;
        const fps = Number(fpsRaw) || 0;
        const ingestState = this.ingest?.state || "idle";
        const peer = !!this.ingest?.peerConnected;
        const lastErr = this.ingest?.lastError || "";
        if (ingestState === "running" && peer) {
          v = { icon: "video", label: "Video", status: "green",
                detail: `Encoder connected — receiving ${fps.toFixed(0)} fps on port ${port}` };
        } else if (ingestState === "running") {
          v = { icon: "video", label: "Video", status: "yellow",
                detail: `Listening on port ${port} — waiting for your encoder to connect` };
        } else if (ingestState === "starting") {
          v = { icon: "video", label: "Video", status: "yellow",
                detail: `Opening port ${port}…` };
        } else if (ingestState === "failed") {
          v = { icon: "video", label: "Video", status: "red",
                detail: lastErr || `Could not open port ${port}` };
        } else {
          v = { icon: "video", label: "Video", status: "yellow",
                detail: `Receiver idle — save the source to bind port ${port}` };
        }
      } else {
        const presence = this.devicePresence("video");
        if (presence.connected) {
          v = { icon: "video", label: "Video", status: "green", detail: presence.label };
        } else if (presence.label) {
          v = { icon: "video", label: "Video", status: "red",
                detail: `${presence.label} not detected — plug it in or pick a different source` };
        } else {
          v = { icon: "video", label: "Video", status: "yellow", detail: "Saved source not in current device list" };
        }
      }
      // Audio source
      let aSource;
      // Supervisor signals "running with silent audio because the
      // configured mic vanished" via audioFallbackDevice. Surface
      // that first so the operator sees what's happening: silent
      // audio is live, and we'll reconnect when the mic returns.
      // Skip for network/SRT-listener sources where audio comes from
      // the upstream stream — the fallback would be a stale signal
      // carried over from a previous local-device session.
      const remoteAudio = this.isNetworkSource || this.isSRTListenerSource;
      const fallback = !remoteAudio ? this.stream?.audioFallbackDevice : "";
      if (fallback) {
        aSource = {
          icon: "mic", label: "Audio", status: "yellow",
          detail: `${fallback} disconnected — silent audio (will reconnect when mic returns)`,
        };
      } else if (this.isSDISource) {
        aSource = { icon: "mic", label: "Audio", status: "green", detail: "Embedded SDI audio" };
      } else if (this.isNetworkSource) {
        aSource = this.networkNoAudio
          ? { icon: "mic", label: "Audio", status: "yellow", detail: "Silent — source has no audio" }
          : { icon: "mic", label: "Audio", status: "green", detail: "Embedded from network source" };
      } else if (this.isSRTListenerSource) {
        aSource = this.networkNoAudio
          ? { icon: "mic", label: "Audio", status: "yellow", detail: "Silent — source has no audio" }
          : { icon: "mic", label: "Audio", status: "green", detail: "Embedded from incoming SRT stream" };
      } else if (!this.audioSourceValue) {
        aSource = { icon: "mic", label: "Audio", status: "yellow", detail: "No audio source — silence will be sent" };
      } else {
        const presence = this.devicePresence("audio");
        if (presence.connected) {
          aSource = { icon: "mic", label: "Audio", status: "green", detail: presence.label };
        } else if (presence.label) {
          aSource = { icon: "mic", label: "Audio", status: "red",
                      detail: `${presence.label} not detected` };
        } else {
          aSource = { icon: "mic", label: "Audio", status: "yellow", detail: "Saved audio source not in current device list" };
        }
      }
      // YouTube
      const yt = !this.youtube.configured
        ? { icon: "yt", label: "YouTube", status: "off", detail: "Not configured" }
        : this.youtube.authenticated
          ? { icon: "yt", label: "YouTube", status: "green", detail: `Connected as ${this.youtube.channelName || "—"}` }
          : { icon: "yt", label: "YouTube", status: "yellow", detail: "Not signed in" };
      return [v, aSource, yt];
    },

    // devicePresence checks whether the saved video/audio device is
    // currently visible in /api/devices. Returns the persisted name as
    // the label so the operator sees what they picked, and a boolean
    // for green/red status. Names are matched against the server's
    // device scan so unplugged hardware shows red.
    devicePresence(kind) {
      const cfg = this.config?.input;
      if (!cfg) return { connected: false, label: "" };
      const wantName = kind === "video" ? cfg.videoDeviceName : cfg.audioDeviceName;
      const idx      = kind === "video" ? cfg.videoDevice     : cfg.audioDevice;
      const backend  = cfg.backend;
      const list = (this.devices[kind] || []);
      // Name-based match (the source of truth).
      if (wantName) {
        const byName = list.find((d) => d.name === wantName && (!backend || d.backend === backend));
        if (byName) return { connected: true, label: wantName };
      }
      // Fall back to index match for legacy configs without a name.
      if (idx && !wantName) {
        const byIndex = list.find((d) => String(d.index) === String(idx) && (!backend || d.backend === backend));
        if (byIndex) return { connected: true, label: byIndex.name };
      }
      return { connected: false, label: wantName || "" };
    },

    deviceLabel(value) {
      if (!value || value === "test-video::") return "Test pattern";
      const parts = value.split(":");
      const idx = parts[parts.length - 1];
      const found = (this.devices.video || []).find((d) => String(d.index) === String(idx) && d.backend === parts[1]);
      return found ? found.name : value;
    },

    // Live-state health pills.
    livePill(label) {
      const c = (this.confidence || []).find((x) => x.label === label);
      return c || { status: "unknown", detail: "" };
    },

    // statusWord maps a traffic-light status to a single word so screen
    // readers announce severity verbally, not just via the colored dot.
    statusWord(s) {
      return { green: "OK", yellow: "warning", red: "problem", off: "off", unknown: "unknown" }[s] || "";
    },

    // ============================================================
    // STREAM CONTROLS
    // ============================================================
    async startScheduledNow() {
      try {
        if (!(await this.saveConfig())) return;
        await this.api("/api/start", { method: "POST" });
        this.showToast("Starting...");
      } catch (e) { this.showToast(e.message); }
    },

    openYTModal() {
      this.ytModal.warn = "";
      this.ytModal.title = this.nextEvent?.title || this.nextEvent?.name || "Live Stream";
      this._modalReturnFocus = document.activeElement;
      this.ytModal.open = true;
      this.$nextTick(() => this.focusModal("ytModalCard"));
    },
    openCustomModal() {
      this.customModal.warn = "";
      this._modalReturnFocus = document.activeElement;
      this.customModal.open = true;
      this.$nextTick(() => this.focusModal("customModalCard"));
    },

    // focusModal moves focus to the first focusable control inside the
    // modal card. Returning focus to the trigger on close happens via
    // closeModal below.
    focusModal(refName) {
      const card = this.$refs[refName];
      if (!card) return;
      const first = card.querySelector("input, select, textarea, button, [tabindex]");
      if (first) first.focus();
    },

    // closeModal centralizes "modal close" logic: hide, restore focus
    // to the trigger, clear any error state.
    closeYTModal()     { this.ytModal.open = false;     this.restoreModalFocus(); },
    closeCustomModal() { this.customModal.open = false; this.restoreModalFocus(); },
    restoreModalFocus() {
      const el = this._modalReturnFocus;
      this._modalReturnFocus = null;
      if (el && typeof el.focus === "function") el.focus();
    },

    // Tab-key focus trap. Cycles focus among the modal's focusables.
    // Invoked from @keydown.tab on the modal card.
    trapModalTab(refName, ev) {
      const card = this.$refs[refName];
      if (!card) return;
      const items = Array.from(card.querySelectorAll("input, select, textarea, button, [tabindex]"))
        .filter((el) => !el.disabled && el.offsetParent !== null);
      if (items.length === 0) return;
      const first = items[0], last = items[items.length - 1];
      const active = document.activeElement;
      if (ev.shiftKey && active === first) { ev.preventDefault(); last.focus(); }
      else if (!ev.shiftKey && active === last) { ev.preventDefault(); first.focus(); }
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
      } catch (e) { this.ytModal.warn = e.message; }
      finally { this.ytModal.busy = false; }
    },

    async startCustom() {
      this.customModal.busy = true;
      this.customModal.warn = "";
      try {
        if (!(await this.saveConfig())) return;
        await this.api("/api/start", { method: "POST" });
        this.customModal.open = false;
        this.showToast("Stream starting...");
      } catch (e) { this.customModal.warn = e.message; }
      finally { this.customModal.busy = false; }
    },

    showStopConfirm() {
      this.stopConfirm = true;
      clearTimeout(this._stopTimer);
      this._stopTimer = setTimeout(() => { this.stopConfirm = false; }, 5000);
      // Move focus so keyboard users land on the confirm button.
      this.$nextTick(() => this.$refs.confirmStop?.focus());
    },
    async doStop() {
      clearTimeout(this._stopTimer);
      this.stopConfirm = false;
      try { await this.api("/api/stop", { method: "POST" }); }
      catch (e) { this.showToast(e.message); }
    },

    async extend() {
      try {
        const result = await this.api("/api/extend", { method: "POST", body: JSON.stringify({ minutes: 15 }) });
        const endsAt = new Date(result.endsAt).toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
        this.showToast(`Extended — now ends at ${endsAt}`);
      } catch (e) { this.showToast(e.message); }
    },

    async setAdaptive(enabled) {
      try {
        await this.api("/api/adaptive", { method: "POST", body: JSON.stringify({ enabled }) });
        this.showToast(enabled ? "Auto-quality enabled" : "Auto-quality disabled");
      } catch (e) { this.showToast(e.message); }
    },

    setEnableHLS(enabled) {
      this.enableHls = enabled;
      this.saveConfig().then(() => this.showToast(enabled ? "HLS monitoring on" : "HLS monitoring off"));
    },

    // ============================================================
    // CONFIG SAVE
    // ============================================================
    async saveConfig() {
      // Real serialisation: chain each call onto the previous promise
      // so two concurrent callers can't both reassign _savePending and
      // race their POSTs. The previous version awaited then assigned,
      // which only deduplicated — two callers entering simultaneously
      // both passed the await with stale state and both fired _doSaveConfig.
      const prev = this._savePending || Promise.resolve();
      const next = prev.then(() => this._doSaveConfig(), () => this._doSaveConfig());
      this._savePending = next;
      try {
        return await next;
      } finally {
        // Clear only if no later caller already chained onto us — they'll
        // own the slot until their own finally runs.
        if (this._savePending === next) this._savePending = null;
      }
    },
    async _doSaveConfig() {
      const decoded = S.decodeSourceValue(this.videoSourceValue) || S.decodeSourceValue(S.encodeSourceValue(this.config?.input, this.devices));
      if (!decoded) {
        this.showToast("Pick a video source before saving.");
        return false;
      }
      const payload = {
        presetId: this.selectedPreset,
        encoder: this.selectedEncoder,
        ingestUrl: (this.ingestUrl || "").trim(),
        outputMode: this.outputMode,
        enableHls: this.enableHls,
        input: {
          kind: decoded.kind,
          backend: decoded.backend,
          videoDevice: decoded.videoDevice,
          audioDevice: this.audioSourceValue || "",
        },
      };
      // Network source: carry URL + NoAudio flag.
      if (decoded.kind === "network") {
        payload.input.url = (this.networkUrl || "").trim();
        payload.input.noAudio = !!this.networkNoAudio;
      }
      // SRT listener: carry port + passphrase + NoAudio.
      // Passphrase round-trip rules:
      //   - operator typed something (dirty) → send that literal value,
      //     including "" to deliberately clear an existing passphrase.
      //   - operator didn't touch the field but server has one stored
      //     → send the sentinel so the backend keeps the stored value.
      //   - no stored passphrase and operator didn't type → omit the
      //     field entirely.
      if (decoded.kind === "srt-listener") {
        payload.input.srtListenPort = Number(this.srtListenPort) || 9999;
        if (this._dirtySRTPass) {
          payload.input.srtListenPassphrase = this.srtListenPassphrase;
        } else if (this.hasSavedSRTPassphrase) {
          payload.input.srtListenPassphrase = "REDACTED";
        }
        payload.input.noAudio = !!this.networkNoAudio;
      }
      // HDR flag applies to any non-test source.
      if (decoded.kind !== "test-video") {
        payload.input.sourceIsHdr = !!this.sourceIsHDR;
      }
      // Persist device names so the backend can resolve stable
      // AVFoundation indexes even when indexes shift between reboots.
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
        await this.api("/api/config", { method: "POST", body: JSON.stringify(payload) });
        // SSE will push the new config back. Clear write-only fields
        // and dirty flags so the next push can refresh the form.
        this.streamKey = "";
        this._dirtyIngest = false;
        this._dirtyStreamKey = false;
        this._dirtyAudio = false;
        this._dirtyVideo = false;
        this._dirtySRTPass = false;
        return true;
      } catch (e) {
        this.showToast("Save failed: " + e.message);
        return false;
      }
    },

    onVideoSourceChange() {
      const prevKind = S.decodeSourceValue(this._lastVideoSourceValue)?.kind || "";
      const nextKind = S.decodeSourceValue(this.videoSourceValue)?.kind || "";
      this._lastVideoSourceValue = this.videoSourceValue;
      this._dirtyVideo = true;
      if (this.isSDISource) this.audioSourceValue = "";
      // Reset network-only flags when switching INTO network from a
      // different source kind. NoAudio defaults to false: most network
      // sources (RTSP cameras, SRT pulls, HLS streams) carry audio, so
      // assume the source has audio until the operator says otherwise.
      // Without this reset, a stale checked state from a previous
      // session could carry over via the dirty flag.
      if (nextKind === "network" && prevKind !== "network") {
        this.networkNoAudio = false;
      }
      // Same defensive reset for SRT-listener mode: most upstream
      // encoders push audio, so default NoAudio off when picking
      // this source kind for the first time.
      if (nextKind === "srt-listener" && prevKind !== "srt-listener") {
        this.networkNoAudio = false;
      }
      // Leaving SRT-listener: drop the in-memory passphrase so it
      // doesn't linger in the form (or get re-posted) if the operator
      // later flips back to srt-listener for a different upstream.
      if (prevKind === "srt-listener" && nextKind !== "srt-listener") {
        this.srtListenPassphrase = "";
        this._dirtySRTPass = false;
      }
      // Picking "Network stream" without a URL would POST invalid
      // config (backend requires Input.URL) and show a save-failed
      // toast. Defer save until the operator types a URL.
      if (this.isNetworkSource && !(this.networkUrl || "").trim()) {
        this.preview?.refresh();
        return;
      }
      this.saveConfig().then(() => this.preview?.refresh());
    },
    onSRTListenPortChange()       { this._dirtyVideo = true; },
    onSRTListenPortBlur()         {
      // x-model.number coerces empty/NaN inputs to NaN. The backend
      // would then reject the save with a port-range error. Clamp
      // to the default before posting so the operator gets a working
      // listener instead of a red toast. Surface the clamp via a
      // toast — silently rewriting an operator's input is worse than
      // a redundant message they'll ignore.
      const original = this.srtListenPort;
      let p = Number(original);
      const valid = Number.isFinite(p) && p >= 1024 && p <= 65535;
      if (!valid) {
        p = 9999;
        this.srtListenPort = p;
        this.showToast("Port must be 1024–65535 — reset to 9999.");
      }
      this.saveConfig().then(() => this.preview?.refresh());
    },
    onSRTListenPassphraseChange() { this._dirtySRTPass = true; },
    onSRTListenPassphraseBlur()   {
      // Don't save if length is in the invalid band — the inline
      // warning is already telling the operator what's wrong, and
      // posting would just produce a duplicate red toast from the
      // backend. Empty is valid (no encryption).
      if (this.srtPassphraseLengthError) return;
      this.saveConfig();
    },
    onNetworkUrlChange()    { this._dirtyVideo = true; },
    onNetworkUrlBlur()      {
      // Blank URL means nothing valid to save yet. Skip.
      if (!(this.networkUrl || "").trim()) return;
      this.saveConfig();
    },
    onNetworkNoAudioToggle() {
      this._dirtyVideo = true;
      // For network sources NoAudio is only meaningful once a URL
      // exists (otherwise the backend rejects the save). For SRT-
      // listener sources there's no URL, so the gate doesn't apply.
      if (this.isNetworkSource && !(this.networkUrl || "").trim()) return;
      this.saveConfig();
    },
    onHDRToggle() {
      this._dirtyVideo = true;
      // Same guard for network sources without a URL yet.
      if (this.isNetworkSource && !(this.networkUrl || "").trim()) return;
      this.saveConfig();
    },
    onAudioSourceChange()   { this._dirtyAudio = true; this.saveConfig(); },
    onEncoderChange()       { this.saveConfig().then(() => this.showToast("Encoder saved")); },
    onIngestChange()        { this._dirtyIngest = true; },
    onStreamKeyChange()     { this._dirtyStreamKey = true; },
    onIngestBlur()          { this.saveConfig(); },
    onStreamKeyBlur()       { this.saveConfig(); },

    encoderDescription() {
      const enc = this.encoders.find((e) => e.id === this.selectedEncoder);
      return enc ? enc.description : "";
    },

    selectPreset(id) {
      this.selectedPreset = id;
      this.presetMenuOpen = false;
      this.saveConfig().then(() => this.showToast("Quality saved"));
    },

    syncSelectElements() {
      // Native <select> sometimes ignores x-model when options arrive
      // later from /api/devices (the value reference predates the matching
      // <option>). Force-set after the next tick.
      if (this.$refs.videoSourceSelect) this.$refs.videoSourceSelect.value = this.videoSourceValue;
      if (this.$refs.audioSourceSelect) this.$refs.audioSourceSelect.value = this.audioSourceValue;
    },

    videoSourceOptions() { return S.videoSourceOptions(this.devices); },
    audioSourceOptions() { return S.audioSourceOptions(this.devices, this.isSDISource); },

    // ============================================================
    // PREVIEW (delegated to controller)
    // ============================================================
    togglePreview() {
      this.previewVisible = !this.previewVisible;
      if (this.previewVisible) { this.preview?.start(); }
      else { this.preview?.stop(); this.previewError = ""; }
    },
    refreshPreview() { this.previewError = ""; this.preview?.refresh(); },

    // ============================================================
    // YOUTUBE AUTH
    // ============================================================
    async loginYouTube() {
      try {
        const data = await this.api("/api/youtube/auth/url");
        if (data.url) window.open(data.url, "_blank", "width=600,height=700");
      } catch (e) { this.showToast("YouTube login failed: " + e.message); }
    },
    async logoutYouTube() { await this.api("/api/youtube/auth/logout", { method: "POST" }); },

    // ============================================================
    // SCHEDULES + OVERRIDES
    // ============================================================
    _blankSchedForm() {
      return { id: "", name: "", days: [], time: "09:00", timezone: this.schedForm.timezone || "America/Chicago",
               durationMin: 120, prepLeadMinutes: 0, title: "", description: "", privacy: "unlisted", enabled: true };
    },
    _blankOvrForm() {
      return { id: "", name: "", wallClock: "", timezone: this.ovrForm.timezone || "America/Chicago",
               durationMin: 120, prepLeadMinutes: 0, title: "", description: "", privacy: "unlisted" };
    },
    openSchedForm() { this.schedForm = this._blankSchedForm(); this.ovrFormOpen = false; this.schedFormOpen = true; },
    openOvrForm()   { this.ovrForm = this._blankOvrForm();     this.schedFormOpen = false; this.ovrFormOpen = true; },
    editSchedule(s) {
      this.schedForm = {
        id: s.id, name: s.name || "", days: [...(s.days || [])],
        time: s.time || "09:00", timezone: s.timezone || "America/Chicago",
        durationMin: s.durationMin || 120,
        // Use ?? so a saved 0 (JIT — the common case) doesn't fall
        // through to a falsy-coercion default. The whole point of the
        // field is that 0 is a real, intentional value.
        prepLeadMinutes: s.prepLeadMinutes ?? 0,
        title: s.title || "",
        description: s.description || "", privacy: s.privacy || "unlisted",
        enabled: s.enabled !== false,
      };
      if (s.presetId) this.selectedPreset = s.presetId;
      this.ovrFormOpen = false;
      this.schedFormOpen = true;
    },
    editOverride(o) {
      this.ovrForm = {
        id: o.id, name: o.name || "", wallClock: this.toDateTimeLocal(o.startTime),
        timezone: o.timezone || this.ovrForm.timezone || "America/Chicago",
        durationMin: o.durationMin || 120,
        prepLeadMinutes: o.prepLeadMinutes ?? 0,
        title: o.title || "",
        description: o.description || "", privacy: o.privacy || "unlisted",
      };
      if (o.presetId) this.selectedPreset = o.presetId;
      this.schedFormOpen = false;
      this.ovrFormOpen = true;
    },
    toggleDay(day) {
      const idx = this.schedForm.days.indexOf(day);
      if (idx < 0) this.schedForm.days.push(day);
      else this.schedForm.days.splice(idx, 1);
    },
    toDateTimeLocal(value) {
      if (!value) return "";
      const d = new Date(value);
      if (Number.isNaN(d.getTime())) return "";
      const pad = (n) => String(n).padStart(2, "0");
      return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
    },
    async saveSchedule() {
      if (this.schedForm.days.length === 0) { this.showToast("Select at least one day."); return; }
      // Coerce to a whole number — Go's JSON decode into int rejects
      // a fractional value (the backend would 400). Round on the way
      // in and write the rounded value back so the form reflects what
      // we're about to save.
      const pl = Math.round(Number(this.schedForm.prepLeadMinutes));
      if (!Number.isFinite(pl) || pl < 0 || pl > 60) {
        this.showToast("Pre-create minutes must be a whole number between 0 and 60.");
        return;
      }
      this.schedForm.prepLeadMinutes = pl;
      const sched = {
        ...this.schedForm,
        presetId: this.selectedPreset,
        title: (this.schedForm.title || this.schedForm.name).trim(),
        name: this.schedForm.name.trim(),
        description: this.schedForm.description.trim(),
        enabled: this.schedForm.enabled !== false,
      };
      try {
        const editing = !!sched.id;
        await this.api(editing ? `/api/schedules/${sched.id}` : "/api/schedules",
          { method: editing ? "PUT" : "POST", body: JSON.stringify(sched) });
        this.schedFormOpen = false;
        this.showToast(editing ? "Schedule updated." : "Schedule created.");
      } catch (e) { this.showToast(e.message); }
    },
    async toggleScheduleEnabled(s) {
      try {
        await this.api(`/api/schedules/${s.id}`,
          { method: "PUT", body: JSON.stringify({ ...s, enabled: !s.enabled }) });
        this.showToast(!s.enabled ? "Schedule resumed." : "Schedule paused.");
      } catch (e) { this.showToast(e.message); }
    },
    async deleteSchedule(id) {
      if (!confirm("Delete this schedule?")) return;
      try { await this.api(`/api/schedules/${id}`, { method: "DELETE" }); }
      catch (e) { this.showToast(e.message); }
    },
    async saveOverride() {
      if (!this.ovrForm.wallClock) { this.showToast("Pick a date and time."); return; }
      // Same integer-coercion as saveSchedule — Go's JSON decode into
      // int rejects fractional values. Round + write back.
      const pl = Math.round(Number(this.ovrForm.prepLeadMinutes));
      if (!Number.isFinite(pl) || pl < 0 || pl > 60) {
        this.showToast("Pre-create minutes must be a whole number between 0 and 60.");
        return;
      }
      this.ovrForm.prepLeadMinutes = pl;
      const override = {
        ...this.ovrForm,
        presetId: this.selectedPreset,
        title: (this.ovrForm.title || this.ovrForm.name).trim(),
        name: this.ovrForm.name.trim(),
        description: this.ovrForm.description.trim(),
      };
      try {
        const editing = !!override.id;
        await this.api(editing ? `/api/overrides/${override.id}` : "/api/overrides",
          { method: editing ? "PUT" : "POST", body: JSON.stringify(override) });
        this.ovrFormOpen = false;
        this.showToast(editing ? "Special event updated." : "Special event created.");
      } catch (e) { this.showToast(e.message); }
    },
    async deleteOverride(id) {
      if (!confirm("Delete this event?")) return;
      try { await this.api(`/api/overrides/${id}`, { method: "DELETE" }); }
      catch (e) { this.showToast(e.message); }
    },

    // ============================================================
    // QUICK FILL + UTIL
    // ============================================================
    quickFill(platform) {
      const RTMP = {
        youtube:    "rtmps://a.rtmps.youtube.com/live2",
        cloudflare: "rtmps://live.cloudflare.com:443/live/",
        twitch:     "rtmp://live.twitch.tv/app",
      };
      if (RTMP[platform]) {
        this.outputMode = "rtmp";
        this.ingestUrl = RTMP[platform];
      } else {
        return;
      }
      this._dirtyIngest = true;
      this.saveConfig();
      this.showToast(`URL filled — paste your stream key/ID.`);
    },
    copyHLSUrl() {
      window.EasyStreamClipboard(this.hlsUrl).then((ok) => {
        this.showToast(ok ? "HLS URL copied!" : "Copy failed — select the URL manually.");
      });
    },
    copyWatchURL(broadcastId) {
      window.EasyStreamClipboard(`https://youtube.com/watch?v=${broadcastId}`).then((ok) => {
        this.showToast(ok ? "Watch link copied!" : "Copy failed — open the link manually.");
      });
    },

    presetTitle(p) { return F.presetTitle(p); },
    presetName(id) { return this.presets.find((p) => p.id === id)?.name || id; },
    fmtTime(d)            { return F.fmtTime(d); },
    formatEventWhen(d)    { return F.formatEventWhen(d); },
    formatDateTime(d)     { return F.formatDateTime(d); },
    formatDays(days)      { return F.formatDays(days); },
    shortTZ(tz)           { return F.shortTZ(tz); },
    platformFromURL(url)  { return F.platformFromURL(url); },

    showToast(msg) {
      this.toast = msg;
      clearTimeout(this._toastTimer);
      this._toastTimer = setTimeout(() => { this.toast = ""; }, 3000);
    },

    // updateNowTimer starts the 1s tick only when something visible
    // depends on it (uptime when live, countdown when next event exists).
    // Stopped otherwise to avoid needless reactive churn on idle dashboards
    // and on the Settings view.
    updateNowTimer() {
      const need = this.isLive || !!this.nextEvent;
      if (need && !this._nowTimer) {
        this._nowTimer = setInterval(() => { this.nowTick = Date.now(); }, 1000);
      } else if (!need && this._nowTimer) {
        clearInterval(this._nowTimer);
        this._nowTimer = null;
      }
    },
  }));

  // copyToClipboard: graceful fallback for browsers without the async
  // Clipboard API (older Safari, insecure origin, etc). Returns a
  // Promise<boolean> indicating success so callers can toast appropriately.
  window.EasyStreamClipboard = function copyToClipboard(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text).then(() => true, () => false);
    }
    // Legacy execCommand fallback.
    return new Promise((resolve) => {
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.top = "-9999px";
      document.body.appendChild(ta);
      ta.select();
      let ok = false;
      try { ok = document.execCommand("copy"); } catch (_) {}
      document.body.removeChild(ta);
      resolve(ok);
    });
  };
});
