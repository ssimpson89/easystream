// --- Element references ---
const els = {
  state: document.querySelector("#state"),
  title: document.querySelector("#status-title"),
  bitrate: document.querySelector("#bitrate"),
  frames: document.querySelector("#frames"),
  restarts: document.querySelector("#restarts"),
  speed: document.querySelector("#speed"),
  lastMessage: document.querySelector("#last-message"),
  presetTrigger: document.querySelector("#preset-trigger"),
  presetMenu: document.querySelector("#preset-menu"),
  presetDropdown: document.querySelector("#preset-dropdown"),
  presetInfoTitle: document.querySelector("#preset-info-title"),
  presetInfoDescription: document.querySelector("#preset-info-description"),
  presetInfoUpload: document.querySelector("#preset-info-upload"),
  start: document.querySelector("#start"),
  stop: document.querySelector("#stop"),
  startReason: document.querySelector("#start-reason"),
  saveNotice: document.querySelector("#save-notice"),
  problemBanner: document.querySelector("#problem-banner"),
  problemBannerTitle: document.querySelector("#problem-banner-title"),
  problemBannerDetail: document.querySelector("#problem-banner-detail"),
  ingestURL: document.querySelector("#ingest-url"),
  streamName: document.querySelector("#stream-name"),
  videoSource: document.querySelector("#video-source"),
  audioSource: document.querySelector("#audio-source"),
  audioSourceLabel: document.querySelector("#audio-source-label"),
  // Output mode
  outputMode: document.querySelector("#output-mode"),
  rtmpFields: document.querySelector("#rtmp-fields"),
  hlsFields: document.querySelector("#hls-fields"),
  hlsUrl: document.querySelector("#hls-url"),
  copyHlsUrl: document.querySelector("#copy-hls-url"),
  // YouTube
  ytAccount: document.querySelector("#yt-account"),
  ytChannelName: document.querySelector("#yt-channel-name"),
  ytLogout: document.querySelector("#yt-logout"),
  ytLoginPrompt: document.querySelector("#yt-login-prompt"),
  ytLogin: document.querySelector("#yt-login"),
  scheduleUI: document.querySelector("#schedule-ui"),
  nowYTLogin: document.querySelector("#now-yt-login"),
  nowUI: document.querySelector("#now-ui"),
  // Schedule
  eventsList: document.querySelector("#events-list"),
  schedulesList: document.querySelector("#schedules-list"),
  overridesList: document.querySelector("#overrides-list"),
  deviceStatus: document.querySelector("#device-status"),
  adaptiveBanner: document.querySelector("#adaptive-banner"),
  adaptiveBannerTitle: document.querySelector("#adaptive-banner-title"),
  adaptiveBannerDetail: document.querySelector("#adaptive-banner-detail"),
  adaptiveEnabled: document.querySelector("#adaptive-enabled"),
  // Preview
  previewToggle: document.querySelector("#preview-toggle"),
  previewContainer: document.querySelector("#preview-container"),
  previewImg: document.querySelector("#preview-img"),
};

let selectedPreset = "recommended";
let lastDeviceScan = null;
let pendingVideoValue = "test-video::";
let pendingAudioDevice = "";
let configLoaded = false;
let lastStreamState = "idle";
let currentDestMode = "scheduled";
let previewActive = false;
let cachedPresets = [];

// --- API helper ---
async function api(path, options = {}) {
  const headers = {};
  if (options.body) {
    headers["content-type"] = "application/json";
  }
  const response = await fetch(path, { ...options, headers });
  const body = await response.json();
  if (!response.ok) throw new Error(body.error || "Request failed");
  return body;
}

// --- Status polling ---
async function refresh() {
  try {
    const data = await api("/api/status");
    renderStatus(data);
    if (!configLoaded) {
      loadConfigIntoForm(data.config, data.presets);
      if (data.platform) setPlatformDefault(data.platform);
      configLoaded = true;
    }
    if (data.youtube) renderYouTubeStatus(data.youtube);
    if (data.nextEvents) renderEvents(data.nextEvents);
    if (data.adaptive) renderAdaptiveStatus(data.adaptive);
    cachedPresets = data.presets || [];
  } catch (error) {
    els.state.textContent = "Offline";
    els.state.className = "state failed";
    els.lastMessage.textContent = error.message;
  }
}

function renderStatus(data) {
  const stream = data.stream;
  lastStreamState = stream.state;

  els.state.textContent = labelState(stream.state);
  els.state.className = `state ${stream.state}`;
  els.title.textContent = titleForState(stream.state);
  els.bitrate.textContent = stream.lastProgress?.bitrateKbps
    ? `${Math.round(stream.lastProgress.bitrateKbps)} kbps`
    : "-";
  els.frames.textContent = stream.lastProgress?.frame || "-";
  els.restarts.textContent = stream.restartCount ?? 0;
  els.speed.textContent = stream.lastProgress?.speed || "-";
  els.lastMessage.textContent =
    stream.lastError || stream.lastExit || stream.lastLogLine || "No stream activity yet.";

  renderProblemBanner(stream);
  updateButtonStates(stream.state);
}

function renderProblemBanner(stream) {
  if (!els.problemBanner) return;
  const problemStates = { degraded: true, restarting: true, failed: true };
  if (!problemStates[stream.state]) {
    els.problemBanner.hidden = true;
    return;
  }
  els.problemBanner.hidden = false;
  els.problemBanner.className = `problem-banner ${stream.state}`;
  const titles = {
    degraded: "Stream needs attention",
    restarting: "Reconnecting to ingest...",
    failed: "Stream failed",
  };
  els.problemBannerTitle.textContent = titles[stream.state] || "Stream issue";
  els.problemBannerDetail.textContent =
    stream.lastError || stream.lastExit || stream.lastLogLine || "Check your network and capture source.";
}

function loadConfigIntoForm(config, presets) {
  selectedPreset = config.preset.id;
  els.ingestURL.value = config.ingestUrl || "";
  els.streamName.placeholder = config.hasStreamKey
    ? "Stream key is set (enter a new one to replace)"
    : "Paste your stream key here";

  // Video source value is encoded as "kind:backend:device" so the scanner
  // can match it back when populating the dropdown.
  pendingVideoValue = encodeSourceValue(config.input);
  pendingAudioDevice = config.input.audioDevice || "";

  if (els.outputMode) {
    els.outputMode.value = config.outputMode || "rtmp";
    updateOutputModeVisibility();
  }
  if (config.hlsUrl && els.hlsUrl) {
    els.hlsUrl.textContent = config.hlsUrl;
  }
  renderPresets(presets);
}

function setPlatformDefault(_platform) {
  // No-op: backend is now auto-determined from the selected device.
}

// destinationReadiness inspects current UI state to decide whether Go Live
// can fire. Returns {ready: bool, reason: string} where reason is shown
// inline when ready is false.
function destinationReadiness() {
  const videoOK = els.videoSource?.value && els.videoSource.value !== "";
  if (!videoOK) {
    return { ready: false, reason: "Select a video source in Step 3" };
  }
  switch (currentDestMode) {
    case "scheduled":
      // Scheduled mode handles itself via the scheduler. Manual Go Live
      // from this tab uses the last manually-configured destination,
      // which we treat as Manual mode requirements.
      if (!ytAuthed && !destinationHasManualURL()) {
        return { ready: false, reason: "Connect YouTube or use the Manual tab" };
      }
      return { ready: true };
    case "now":
      if (!ytAuthed) {
        return { ready: false, reason: "Connect YouTube to go live now" };
      }
      return { ready: true };
    case "manual":
      if (!destinationHasManualURL()) {
        return { ready: false, reason: "Paste an ingest URL and stream key" };
      }
      return { ready: true };
    default:
      return { ready: true };
  }
}

function destinationHasManualURL() {
  const url = els.ingestURL?.value?.trim();
  if (!url) return false;
  // RTMP needs a key; HLS doesn't.
  if (els.outputMode?.value === "hls") return true;
  const keyTyped = els.streamName?.value?.trim().length > 0;
  const keyAlreadySaved = els.streamName?.placeholder?.includes("Stream key is set");
  return keyTyped || keyAlreadySaved;
}

let ytAuthed = false;

function updateButtonStates(state) {
  const isActive = ["starting", "running", "degraded", "restarting"].includes(state);
  const isStopping = state === "stopping";
  els.stop.disabled = !isActive;

  if (isActive || isStopping) {
    els.start.disabled = true;
    if (els.startReason) els.startReason.hidden = true;
    return;
  }

  const readiness = destinationReadiness();
  els.start.disabled = !readiness.ready;
  if (els.startReason) {
    if (readiness.ready) {
      els.startReason.hidden = true;
    } else {
      els.startReason.hidden = false;
      els.startReason.textContent = readiness.reason;
    }
  }
}

// --- Output mode ---
function updateOutputModeVisibility() {
  if (!els.outputMode) return;
  const isHLS = els.outputMode.value === "hls";
  if (els.rtmpFields) els.rtmpFields.hidden = isHLS;
  if (els.hlsFields) els.hlsFields.hidden = !isHLS;
}

els.outputMode?.addEventListener("change", updateOutputModeVisibility);

els.copyHlsUrl?.addEventListener("click", () => {
  const url = els.hlsUrl?.textContent;
  if (url) {
    navigator.clipboard.writeText(url).then(() => showNotice("HLS URL copied!"));
  }
});

// --- Capture source ---
// Encode/decode source values for the unified picker.
// Format: "kind:backend:device" (e.g., "webcam:avfoundation:0", "sdi:decklink:DeckLink Mini Recorder")
// Special: "test-video::" for the test pattern.
function encodeSourceValue(input) {
  if (!input || input.kind === "test-video") return "test-video::";
  const kind = input.kind || "webcam";
  const backend = input.backend || "avfoundation";
  const device = input.videoDevice || "";
  return `${kind}:${backend}:${device}`;
}

function decodeSourceValue(value) {
  if (!value || value === "test-video::") {
    return { kind: "test-video", backend: "lavfi", videoDevice: "" };
  }
  const [kind, backend, ...rest] = value.split(":");
  return { kind, backend, videoDevice: rest.join(":") };
}

// Map device type to an input kind for FFmpeg config.
function kindForDeviceType(type) {
  switch (type) {
    case "sdi": return "sdi";
    case "capture-card": return "hdmi";
    case "screen": return "webcam"; // screen capture uses platform backend like a webcam
    case "camera": return "webcam";
    default: return "webcam";
  }
}

// --- Presets (custom card-style dropdown) ---
function presetTitle(preset) {
  const fps = preset.fps === 60 ? "60" : "";
  return `${preset.name} · ${preset.height}p${fps} · ${preset.videoKbps / 1000} Mbps`;
}

function renderPresets(presets) {
  if (!els.presetMenu) return;
  els.presetMenu.innerHTML = "";
  for (const preset of presets) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = `preset-option ${preset.id === selectedPreset ? "active" : ""}`;
    btn.dataset.presetId = preset.id;
    btn.innerHTML = `
      <strong></strong>
      <span></span>
      <span class="preset-upload"></span>
    `;
    btn.querySelector("strong").textContent = presetTitle(preset);
    btn.querySelector("span").textContent = preset.description;
    btn.querySelector(".preset-upload").textContent = `Upload target: ${preset.uploadTarget}`;
    btn.addEventListener("click", () => {
      selectedPreset = preset.id;
      closePresetMenu();
      updatePresetDescription(presets);
      renderPresets(presets);
      autoSave(true);
    });
    els.presetMenu.appendChild(btn);
  }
  updatePresetDescription(presets);
}

function updatePresetDescription(presets) {
  const preset = (presets || cachedPresets).find((p) => p.id === selectedPreset);
  if (!preset) return;
  if (els.presetInfoTitle) els.presetInfoTitle.textContent = presetTitle(preset);
  if (els.presetInfoDescription) els.presetInfoDescription.textContent = preset.description;
  if (els.presetInfoUpload) els.presetInfoUpload.textContent = `Upload target: ${preset.uploadTarget}`;
}

function openPresetMenu() {
  els.presetMenu.hidden = false;
  els.presetTrigger.setAttribute("aria-expanded", "true");
}
function closePresetMenu() {
  els.presetMenu.hidden = true;
  els.presetTrigger.setAttribute("aria-expanded", "false");
}

els.presetTrigger?.addEventListener("click", (e) => {
  e.stopPropagation();
  if (els.presetMenu.hidden) openPresetMenu();
  else closePresetMenu();
});
document.addEventListener("click", (e) => {
  if (!els.presetDropdown.contains(e.target)) closePresetMenu();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") closePresetMenu();
});

// --- State labels ---
function labelState(state) {
  return { idle: "Idle", starting: "Starting", running: "Live", degraded: "Degraded",
    restarting: "Restarting", stopping: "Stopping", failed: "Failed" }[state] || state;
}
function titleForState(state) {
  return { idle: "Ready to go live", starting: "Starting encoder...", running: "Stream is live",
    degraded: "Stream needs attention", restarting: "Recovering stream...",
    stopping: "Stopping encoder...", failed: "Stream failed" }[state] || "Checking status...";
}

// --- Save notice ---
function showNotice(message) {
  if (!els.saveNotice) return;
  els.saveNotice.textContent = message;
  els.saveNotice.classList.add("visible");
  clearTimeout(showNotice._t);
  showNotice._t = setTimeout(() => els.saveNotice.classList.remove("visible"), 3000);
}

// --- Config save ---
async function saveConfig() {
  const decoded = decodeSourceValue(els.videoSource.value);
  const payload = {
    presetId: selectedPreset,
    ingestUrl: els.ingestURL.value.trim(),
    outputMode: els.outputMode?.value || "rtmp",
    input: {
      kind: decoded.kind,
      backend: decoded.backend,
      videoDevice: decoded.videoDevice,
      audioDevice: els.audioSource.value || "",
    },
  };
  const key = els.streamName.value.trim();
  if (key) payload.streamName = key;

  const result = await api("/api/config", { method: "POST", body: JSON.stringify(payload) });
  selectedPreset = result.preset.id;
  els.ingestURL.value = result.ingestUrl || "";
  els.streamName.value = "";
  els.streamName.placeholder = result.hasStreamKey
    ? "Stream key is set (enter a new one to replace)"
    : "Paste your stream key here";
  pendingVideoValue = encodeSourceValue(result.input);
  pendingAudioDevice = result.input.audioDevice || "";
  if (els.outputMode) els.outputMode.value = result.outputMode || "rtmp";
  if (result.hlsUrl && els.hlsUrl) els.hlsUrl.textContent = result.hlsUrl;
  updateOutputModeVisibility();
  applyPendingToDropdowns();
}

// --- Button handlers ---
els.start.addEventListener("click", async () => {
  try {
    els.start.disabled = true;
    els.lastMessage.textContent = "Saving settings and starting stream...";
    await saveConfig();
    await api("/api/start", { method: "POST" });
    await refresh();
  } catch (error) {
    els.lastMessage.textContent = error.message;
    updateButtonStates(lastStreamState);
  }
});

els.stop.addEventListener("click", async () => {
  try {
    els.stop.disabled = true;
    els.lastMessage.textContent = "Stopping stream...";
    await api("/api/stop", { method: "POST" });
    await refresh();
  } catch (error) {
    els.lastMessage.textContent = error.message;
    updateButtonStates(lastStreamState);
  }
});

// --- Auto-save on field change ---
// Replaces the old global "Save Settings" button. Each form change is
// debounced and saved automatically.
let autoSaveTimer = null;
function autoSave(immediate = false) {
  clearTimeout(autoSaveTimer);
  const fire = async () => {
    try {
      await saveConfig();
      showNotice("Settings saved");
      // Refresh button states (destination might now be ready/unready).
      updateButtonStates(lastStreamState);
    } catch (error) {
      els.lastMessage.textContent = error.message;
    }
  };
  if (immediate) {
    fire();
  } else {
    autoSaveTimer = setTimeout(fire, 600);
  }
}

// Text fields save on blur. Update Go Live readiness live as user types.
els.ingestURL?.addEventListener("blur", () => autoSave(true));
els.ingestURL?.addEventListener("input", () => updateButtonStates(lastStreamState));
els.streamName?.addEventListener("blur", () => autoSave(true));
els.streamName?.addEventListener("input", () => updateButtonStates(lastStreamState));
// Selects save immediately on change.
els.outputMode?.addEventListener("change", () => autoSave(true));

// --- Destination mode tabs ---
document.querySelectorAll("#dest-tabs .tab").forEach((tab) => {
  tab.addEventListener("click", () => {
    document.querySelectorAll("#dest-tabs .tab").forEach((t) => t.classList.remove("active"));
    tab.classList.add("active");
    currentDestMode = tab.dataset.mode;
    document.querySelectorAll(".mode-content").forEach((el) => (el.hidden = true));
    document.querySelector(`#mode-${currentDestMode}`).hidden = false;
    updateButtonStates(lastStreamState);
  });
});

// --- YouTube auth ---
function loginYouTube() {
  api("/api/youtube/auth/url").then((data) => {
    if (data.url) window.open(data.url, "_blank", "width=600,height=700");
  }).catch((err) => {
    els.lastMessage.textContent = "YouTube login failed: " + err.message;
  });
}

els.ytLogin?.addEventListener("click", loginYouTube);
els.ytLogout?.addEventListener("click", async () => {
  await api("/api/youtube/auth/logout", { method: "POST" });
  await refresh();
});

// --- Adaptive quality status ---
let adaptiveEnabledInitialized = false;

function renderAdaptiveStatus(state) {
  // One-time sync of the toggle to server state.
  if (!adaptiveEnabledInitialized && els.adaptiveEnabled) {
    els.adaptiveEnabled.checked = state.enabled;
    adaptiveEnabledInitialized = true;
  }

  if (state.isFallback && state.activePreset && state.originalPreset) {
    const orig = cachedPresets.find((p) => p.id === state.originalPreset) || { name: state.originalPreset };
    const active = cachedPresets.find((p) => p.id === state.activePreset) || { name: state.activePreset };
    if (els.adaptiveBanner) {
      els.adaptiveBanner.hidden = false;
      els.adaptiveBannerTitle.textContent = `Auto-reduced quality to ${active.name}`;
      els.adaptiveBannerDetail.textContent =
        `${state.reason || "Network conditions"} — original target was ${orig.name}. Will restore automatically when stable.`;
    }
  } else if (els.adaptiveBanner) {
    els.adaptiveBanner.hidden = true;
  }
}

els.adaptiveEnabled?.addEventListener("change", async (e) => {
  try {
    await api("/api/adaptive", {
      method: "POST",
      body: JSON.stringify({ enabled: e.target.checked }),
    });
    showNotice(e.target.checked ? "Auto-quality enabled" : "Auto-quality disabled");
  } catch (err) {
    els.lastMessage.textContent = err.message;
    e.target.checked = !e.target.checked;
  }
});

function renderYouTubeStatus(yt) {
  const authed = yt.authenticated;
  const configured = yt.configured;
  ytAuthed = authed;

  // Header account info
  if (authed && yt.channelName) {
    els.ytAccount.hidden = false;
    els.ytChannelName.textContent = yt.channelName;
  } else {
    els.ytAccount.hidden = true;
  }

  // Scheduled mode
  if (configured) {
    els.ytLoginPrompt.hidden = authed;
    els.scheduleUI.hidden = !authed;
  } else {
    els.ytLoginPrompt.querySelector("p").textContent =
      "YouTube integration is not configured. Set YOUTUBE_CLIENT_ID and YOUTUBE_CLIENT_SECRET environment variables to enable.";
    els.ytLoginPrompt.querySelector("button").hidden = true;
    els.ytLoginPrompt.hidden = false;
    els.scheduleUI.hidden = true;
  }

  // Go Live Now mode
  if (configured && authed) {
    els.nowYTLogin.hidden = true;
    els.nowUI.hidden = false;
  } else {
    els.nowYTLogin.hidden = false;
    els.nowUI.hidden = true;
    if (!configured) {
      els.nowYTLogin.querySelector("p").textContent =
        "YouTube not configured. Use Manual mode or set up YouTube credentials.";
      els.nowYTLogin.querySelector("button").hidden = true;
    }
  }
}

// --- Go Live Now ---
document.querySelector("#now-go-live")?.addEventListener("click", async () => {
  try {
    const title = document.querySelector("#now-title").value.trim() || "Live Stream";
    const privacy = document.querySelector("#now-privacy").value;
    els.lastMessage.textContent = "Creating YouTube broadcast and going live...";
    await saveConfig();
    const result = await api("/api/youtube/go-live-now", {
      method: "POST",
      body: JSON.stringify({ title, privacy }),
    });
    showNotice("Broadcast created! Stream is starting...");
    await refresh();
  } catch (error) {
    els.lastMessage.textContent = error.message;
  }
});

// --- Events ---
function renderEvents(events) {
  if (!els.eventsList) return;
  if (!events || events.length === 0) {
    els.eventsList.innerHTML = '<p class="hint">No upcoming events.</p>';
    return;
  }
  els.eventsList.innerHTML = "";
  for (const event of events.slice(0, 5)) {
    const div = document.createElement("div");
    div.className = "event-card";
    const when = new Date(event.startTime);
    const dateStr = when.toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric" });
    const timeStr = when.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
    const statusBadge = event.broadcastId
      ? '<span class="badge badge-ready">Ready</span>'
      : '<span class="badge badge-pending">Pending</span>';
    div.innerHTML = `
      <div class="event-info">
        <strong>${event.name}</strong>
        <span>${dateStr} at ${timeStr}</span>
      </div>
      <div class="event-meta">${statusBadge}</div>
    `;
    els.eventsList.appendChild(div);
  }
}

// --- Schedules ---
async function loadSchedules() {
  try {
    const schedules = await api("/api/schedules");
    renderSchedules(schedules);
  } catch (_) {}
}

function renderSchedules(schedules) {
  if (!els.schedulesList) return;
  if (!schedules || schedules.length === 0) {
    els.schedulesList.innerHTML = '<p class="hint">No recurring schedules.</p>';
    return;
  }
  els.schedulesList.innerHTML = "";
  for (const sched of schedules) {
    const div = document.createElement("div");
    div.className = "sched-card";
    const days = sched.days.map((d) => d.charAt(0).toUpperCase() + d.slice(0, 3)).join(", ");
    div.innerHTML = `
      <div class="sched-info">
        <strong>${sched.name}</strong>
        <span>${days} at ${sched.time} (${sched.timezone.split("/").pop().replace("_", " ")})</span>
      </div>
      <div class="sched-actions">
        <span class="badge ${sched.enabled ? "badge-ready" : "badge-pending"}">${sched.enabled ? "Active" : "Off"}</span>
        <button class="button button-sm button-danger" data-delete-schedule="${sched.id}">Delete</button>
      </div>
    `;
    els.schedulesList.appendChild(div);
  }
  // Wire delete buttons
  els.schedulesList.querySelectorAll("[data-delete-schedule]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      if (!confirm("Delete this schedule?")) return;
      await api(`/api/schedules/${btn.dataset.deleteSchedule}`, { method: "DELETE" });
      await loadSchedules();
    });
  });
}

// Schedule form
document.querySelector("#add-schedule-btn")?.addEventListener("click", () => {
  document.querySelector("#schedule-form").hidden = false;
});
document.querySelector("#sched-cancel")?.addEventListener("click", () => {
  document.querySelector("#schedule-form").hidden = true;
});

// Day picker
document.querySelectorAll("#sched-days .day-btn").forEach((btn) => {
  btn.addEventListener("click", () => btn.classList.toggle("selected"));
});

document.querySelector("#sched-save")?.addEventListener("click", async () => {
  const selectedDays = Array.from(document.querySelectorAll("#sched-days .day-btn.selected"))
    .map((b) => b.dataset.day);
  if (selectedDays.length === 0) {
    alert("Select at least one day.");
    return;
  }
  const sched = {
    name: document.querySelector("#sched-name").value.trim(),
    days: selectedDays,
    time: document.querySelector("#sched-time").value,
    timezone: document.querySelector("#sched-tz").value,
    durationMin: parseInt(document.querySelector("#sched-duration").value) || 120,
    presetId: selectedPreset,
    title: document.querySelector("#sched-title").value.trim() || document.querySelector("#sched-name").value.trim(),
    description: document.querySelector("#sched-desc").value.trim(),
    privacy: document.querySelector("#sched-privacy").value,
    enabled: true,
  };
  try {
    await api("/api/schedules", { method: "POST", body: JSON.stringify(sched) });
    document.querySelector("#schedule-form").hidden = true;
    showNotice("Schedule created.");
    // Clear form
    document.querySelector("#sched-name").value = "";
    document.querySelector("#sched-title").value = "";
    document.querySelector("#sched-desc").value = "";
    document.querySelectorAll("#sched-days .day-btn").forEach((b) => b.classList.remove("selected"));
    await loadSchedules();
    await refresh();
  } catch (error) {
    alert(error.message);
  }
});

// --- Overrides ---
async function loadOverrides() {
  try {
    const overrides = await api("/api/overrides");
    renderOverrides(overrides);
  } catch (_) {}
}

function renderOverrides(overrides) {
  if (!els.overridesList) return;
  if (!overrides || overrides.length === 0) {
    els.overridesList.innerHTML = '<p class="hint">No special events.</p>';
    return;
  }
  els.overridesList.innerHTML = "";
  for (const o of overrides) {
    const div = document.createElement("div");
    div.className = "sched-card";
    const when = new Date(o.startTime);
    const dateStr = when.toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric" });
    const timeStr = when.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
    div.innerHTML = `
      <div class="sched-info">
        <strong>${o.name}</strong>
        <span>${dateStr} at ${timeStr}</span>
      </div>
      <div class="sched-actions">
        <button class="button button-sm button-danger" data-delete-override="${o.id}">Delete</button>
      </div>
    `;
    els.overridesList.appendChild(div);
  }
  els.overridesList.querySelectorAll("[data-delete-override]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      if (!confirm("Delete this event?")) return;
      await api(`/api/overrides/${btn.dataset.deleteOverride}`, { method: "DELETE" });
      await loadOverrides();
    });
  });
}

// Override form
document.querySelector("#add-override-btn")?.addEventListener("click", () => {
  document.querySelector("#override-form").hidden = false;
});
document.querySelector("#ovr-cancel")?.addEventListener("click", () => {
  document.querySelector("#override-form").hidden = true;
});

document.querySelector("#ovr-save")?.addEventListener("click", async () => {
  const datetimeStr = document.querySelector("#ovr-datetime").value;
  if (!datetimeStr) {
    alert("Pick a date and time.");
    return;
  }
  // Parse the datetime-local as the selected timezone
  const tz = document.querySelector("#ovr-tz").value;
  // datetime-local returns YYYY-MM-DDTHH:MM as a wall-clock with no timezone.
  // Send it verbatim along with the selected timezone; the server converts
  // to UTC correctly (browser Date() would have used the browser's tz, which
  // is wrong when the user picks a different zone for the event).
  const override = {
    name: document.querySelector("#ovr-name").value.trim(),
    wallClock: datetimeStr,
    timezone: tz,
    durationMin: parseInt(document.querySelector("#ovr-duration").value) || 120,
    presetId: selectedPreset,
    title: document.querySelector("#ovr-title").value.trim() || document.querySelector("#ovr-name").value.trim(),
    description: document.querySelector("#ovr-desc").value.trim(),
    privacy: document.querySelector("#ovr-privacy").value,
  };
  try {
    await api("/api/overrides", { method: "POST", body: JSON.stringify(override) });
    document.querySelector("#override-form").hidden = true;
    showNotice("Special event created.");
    document.querySelector("#ovr-name").value = "";
    document.querySelector("#ovr-title").value = "";
    document.querySelector("#ovr-desc").value = "";
    document.querySelector("#ovr-datetime").value = "";
    await loadOverrides();
    await refresh();
  } catch (error) {
    alert(error.message);
  }
});

// --- Device discovery (smart unified picker) ---

const DEVICE_GROUPS = [
  { type: "camera",       label: "Cameras" },
  { type: "capture-card", label: "Capture cards (USB HDMI)" },
  { type: "screen",       label: "Screen capture" },
  { type: "sdi",          label: "SDI (Blackmagic DeckLink)" },
];

const AUDIO_GROUPS = [
  { type: "microphone",  label: "Microphones" },
  { type: "audio-input", label: "Audio inputs" },
];

async function scanDevices(forceRefresh) {
  try {
    const url = `/api/devices${forceRefresh ? "?refresh=1" : ""}`;
    const data = await api(url);
    lastDeviceScan = data;
    renderDevicePickers(data);
  } catch (_) {}
}

function renderDevicePickers(data) {
  // --- Video picker with grouped categories ---
  els.videoSource.innerHTML = "";

  // Always-available test pattern at the top.
  const testOpt = document.createElement("option");
  testOpt.value = "test-video::";
  testOpt.textContent = "Test pattern (no hardware)";
  els.videoSource.appendChild(testOpt);

  let videoCount = 0;
  for (const group of DEVICE_GROUPS) {
    const matches = (data.video || []).filter((d) => d.type === group.type);
    if (matches.length === 0) continue;
    const og = document.createElement("optgroup");
    og.label = group.label;
    for (const d of matches) {
      const opt = document.createElement("option");
      const kind = kindForDeviceType(d.type);
      opt.value = `${kind}:${d.backend}:${d.index}`;
      opt.textContent = d.backend === "decklink" ? d.name : `${d.name} [${d.index}]`;
      og.appendChild(opt);
      videoCount++;
    }
    els.videoSource.appendChild(og);
  }

  // Restore selection.
  const wantedVideo = pendingVideoValue || els.videoSource.value;
  if (wantedVideo && [...els.videoSource.options].some((o) => o.value === wantedVideo)) {
    els.videoSource.value = wantedVideo;
  }

  // --- Audio picker ---
  els.audioSource.innerHTML = "";
  const noneOpt = document.createElement("option");
  noneOpt.value = "";

  // Detect if currently selected video is SDI; if so, default to embedded SDI audio.
  const selectedKind = decodeSourceValue(els.videoSource.value).kind;
  noneOpt.textContent = selectedKind === "sdi" ? "Embedded SDI audio" : "None / silent";
  els.audioSource.appendChild(noneOpt);

  let audioCount = 0;
  for (const group of AUDIO_GROUPS) {
    const matches = (data.audio || []).filter((d) => d.type === group.type);
    if (matches.length === 0) continue;
    const og = document.createElement("optgroup");
    og.label = group.label;
    for (const d of matches) {
      const opt = document.createElement("option");
      opt.value = d.index;
      opt.textContent = `${d.name} [${d.index}]`;
      og.appendChild(opt);
      audioCount++;
    }
    els.audioSource.appendChild(og);
  }

  if (pendingAudioDevice && [...els.audioSource.options].some((o) => o.value === pendingAudioDevice)) {
    els.audioSource.value = pendingAudioDevice;
  }

  // Hide audio picker for SDI sources (audio is always embedded).
  if (els.audioSourceLabel) {
    els.audioSourceLabel.hidden = selectedKind === "sdi";
  }

  // Clear pending values once applied.
  if (els.videoSource.value === pendingVideoValue) pendingVideoValue = "";
  if (els.audioSource.value === pendingAudioDevice) pendingAudioDevice = "";

  // Status line.
  if (els.deviceStatus) {
    if (videoCount === 0) {
      els.deviceStatus.textContent = `No video devices detected. Connect a camera or capture card and click Refresh.`;
    } else {
      els.deviceStatus.textContent = `${videoCount} video, ${audioCount} audio detected. Auto-refreshes every 5s.`;
    }
  }
}

function applyPendingToDropdowns() {
  if (lastDeviceScan) renderDevicePickers(lastDeviceScan);
}

// Poll for devices every 5 seconds.
scanDevices();
setInterval(() => scanDevices(), 5000);

// Refresh button.
document.querySelector("#refresh-devices")?.addEventListener("click", () => scanDevices(true));

// --- Preview ---
let previewFailed = false;

function restartPreview() {
  if (!previewActive) return;
  previewFailed = false;
  els.previewImg.src = "";
  hidePreviewError();
  setTimeout(startPreview, 300);
}

async function saveAndRestartPreview() {
  if (!previewActive) return;
  previewFailed = false;
  els.previewImg.src = "";
  hidePreviewError();
  setTimeout(startPreview, 300);
}

els.previewToggle?.addEventListener("click", () => {
  previewActive = !previewActive;
  els.previewContainer.hidden = !previewActive;
  els.previewToggle.textContent = previewActive ? "Hide Preview" : "Show Preview";
  const refreshBtn = document.querySelector("#preview-refresh");
  if (refreshBtn) refreshBtn.hidden = !previewActive;
  if (previewActive) {
    previewFailed = false;
    hidePreviewError();
    startPreview();
  } else {
    els.previewImg.src = "";
    hidePreviewError();
  }
});

document.querySelector("#preview-refresh")?.addEventListener("click", () => {
  previewFailed = false;
  els.previewImg.src = "";
  hidePreviewError();
  startPreview();
});

function startPreview() {
  if (previewFailed) return;
  saveConfig().then(() => {
    const img = els.previewImg;
    img.style.display = "";
    let loaded = false;

    // Set a timeout — if no frame loads within 4 seconds, assume failure.
    clearTimeout(startPreview._timeout);
    startPreview._timeout = setTimeout(() => {
      if (!loaded && previewActive && !previewFailed) {
        previewFailed = true;
        showPreviewError("Could not open capture device. Check that camera permission is granted (System Settings > Privacy & Security > Camera), the device isn't in use by another app, and the correct device is selected.");
      }
    }, 4000);

    img.onload = () => { loaded = true; };
    img.onerror = () => {
      if (previewActive && !previewFailed) {
        previewFailed = true;
        clearTimeout(startPreview._timeout);
        showPreviewError("Could not open capture device. Check that camera permission is granted (System Settings > Privacy & Security > Camera), the device isn't in use by another app, and the correct device is selected.");
      }
    };

    img.src = "/api/preview?" + Date.now();
  });
}

function showPreviewError(msg) {
  let el = document.querySelector("#preview-error");
  if (!el) {
    el = document.createElement("p");
    el.id = "preview-error";
    el.className = "preview-error";
    els.previewContainer.appendChild(el);
  }
  el.textContent = msg;
  el.hidden = false;
  els.previewImg.style.display = "none";
}

function hidePreviewError() {
  const el = document.querySelector("#preview-error");
  if (el) el.hidden = true;
  els.previewImg.style.display = "";
}

// Restart preview when capture source settings change.
els.videoSource.addEventListener("change", () => {
  // Re-render audio dropdown so "Embedded SDI audio" label matches new kind.
  if (lastDeviceScan) renderDevicePickers(lastDeviceScan);
  saveAndRestartPreview();
});
els.audioSource.addEventListener("change", saveAndRestartPreview);

// --- Quick-fill destination presets ---
const DEST_PRESETS = {
  youtube: "rtmps://a.rtmps.youtube.com/live2",
  cloudflare: "rtmps://live.cloudflare.com:443/live/",
  twitch: "rtmp://live.twitch.tv/app",
};
document.querySelectorAll("[data-preset]").forEach((btn) => {
  btn.addEventListener("click", () => {
    const url = DEST_PRESETS[btn.dataset.preset];
    if (url) {
      els.ingestURL.value = url;
      els.streamName.focus();
      showNotice(`${btn.textContent} URL filled — paste your stream key.`);
    }
  });
});

// --- Init ---
refresh();
setInterval(refresh, 2000);

// Load schedules and overrides on init
setTimeout(() => {
  loadSchedules();
  loadOverrides();
}, 500);
