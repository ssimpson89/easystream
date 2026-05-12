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
    selectedPreset:   "recommended",
    selectedEncoder:  "libx264",
    outputMode:       "rtmp",
    ingestUrl:        "",
    streamKey:        "",
    hasStreamKey:     false,
    hlsUrl:           "http://127.0.0.1:8080/hls/stream.m3u8",

    // Dirty flags — set when the user is actively editing a field.
    // SSE state pushes respect these so we never overwrite a key the
    // user is typing. Cleared after a save round-trip.
    _dirtyIngest:    false,
    _dirtyStreamKey: false,
    _dirtyAudio:     false,
    _dirtyVideo:     false,

    schedForm: { id: "", name: "", days: [], time: "09:00", timezone: "America/Chicago", durationMin: 120, title: "", description: "", privacy: "unlisted", enabled: true },
    ovrForm:   { id: "", name: "", wallClock: "", timezone: "America/Chicago", durationMin: 120, title: "", description: "", privacy: "unlisted" },

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
      this.activeBroadcastId = data.activeBroadcastId || "";
      this.syncFormFromConfig(data.config);
    },

    syncFormFromConfig(config) {
      if (!config) return;
      // Preset + encoder are non-dirtyable: there's no free-text input,
      // so server is always authoritative.
      this.selectedPreset  = config.preset?.id || this.selectedPreset;
      this.selectedEncoder = config.encoder || "libx264";
      this.hasStreamKey    = !!config.hasStreamKey;
      this.outputMode      = config.outputMode || "rtmp";
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
            this.$nextTick(() => this.syncSelectElements());
          }
        }
      }
      if (!this._dirtyAudio) {
        this.audioSourceValue = config.input?.audioDevice || "";
      }
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
    get isSDISource()   { return S.decodeSourceValue(this.videoSourceValue)?.kind === "sdi"; },
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
      const conf = (this.confidence || []).reduce((a, c) => (a[c.label] = c, a), {});
      const status = (s) => ({ green: "green", yellow: "yellow", red: "red" }[s] || "unknown");
      // Video source
      const v = this.videoOK
        ? { icon: "video", label: "Video", status: "green", detail: this.deviceLabel(this.videoSourceValue) }
        : { icon: "video", label: "Video", status: "red",   detail: "No video source picked" };
      // Audio source
      const aSource = (this.audioSourceValue || this.isSDISource)
        ? { icon: "mic", label: "Audio", status: "green", detail: this.isSDISource ? "Embedded SDI audio" : "Mic configured" }
        : { icon: "mic", label: "Audio", status: "yellow", detail: "No audio source — silence will be sent" };
      // YouTube
      const yt = !this.youtube.configured
        ? { icon: "yt", label: "YouTube", status: "off", detail: "Not configured" }
        : this.youtube.authenticated
          ? { icon: "yt", label: "YouTube", status: "green", detail: `Connected as ${this.youtube.channelName || "—"}` }
          : { icon: "yt", label: "YouTube", status: "yellow", detail: "Not signed in" };
      return [v, aSource, yt];
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

    // ============================================================
    // CONFIG SAVE
    // ============================================================
    async saveConfig() {
      // Single in-flight save promise to prevent overlapping requests.
      if (this._savePending) { await this._savePending; }
      this._savePending = this._doSaveConfig();
      try { return await this._savePending; }
      finally { this._savePending = null; }
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
        input: {
          kind: decoded.kind,
          backend: decoded.backend,
          videoDevice: decoded.videoDevice,
          audioDevice: this.audioSourceValue || "",
        },
      };
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
        return true;
      } catch (e) {
        this.showToast("Save failed: " + e.message);
        return false;
      }
    },

    onVideoSourceChange() {
      this._dirtyVideo = true;
      if (this.isSDISource) this.audioSourceValue = "";
      this.saveConfig().then(() => this.preview?.refresh());
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
               durationMin: 120, title: "", description: "", privacy: "unlisted", enabled: true };
    },
    _blankOvrForm() {
      return { id: "", name: "", wallClock: "", timezone: this.ovrForm.timezone || "America/Chicago",
               durationMin: 120, title: "", description: "", privacy: "unlisted" };
    },
    openSchedForm() { this.schedForm = this._blankSchedForm(); this.ovrFormOpen = false; this.schedFormOpen = true; },
    openOvrForm()   { this.ovrForm = this._blankOvrForm();     this.schedFormOpen = false; this.ovrFormOpen = true; },
    editSchedule(s) {
      this.schedForm = {
        id: s.id, name: s.name || "", days: [...(s.days || [])],
        time: s.time || "09:00", timezone: s.timezone || "America/Chicago",
        durationMin: s.durationMin || 120, title: s.title || "",
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
        durationMin: o.durationMin || 120, title: o.title || "",
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
      const URLS = {
        youtube: "rtmps://a.rtmps.youtube.com/live2",
        cloudflare: "rtmps://live.cloudflare.com:443/live/",
        twitch: "rtmp://live.twitch.tv/app",
      };
      const url = URLS[platform];
      if (!url) return;
      this.ingestUrl = url;
      this._dirtyIngest = true;
      this.saveConfig();
      this.showToast(`${platform} URL filled — paste your stream key.`);
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
