// --- Element references ---
const els = {
  state: document.querySelector("#state"),
  title: document.querySelector("#status-title"),
  bitrate: document.querySelector("#bitrate"),
  frames: document.querySelector("#frames"),
  restarts: document.querySelector("#restarts"),
  speed: document.querySelector("#speed"),
  lastMessage: document.querySelector("#last-message"),
  presets: document.querySelector("#presets"),
  start: document.querySelector("#start"),
  stop: document.querySelector("#stop"),
  save: document.querySelector("#save-config"),
  saveNotice: document.querySelector("#save-notice"),
  ingestURL: document.querySelector("#ingest-url"),
  streamName: document.querySelector("#stream-name"),
  inputKind: document.querySelector("#input-kind"),
  inputBackend: document.querySelector("#input-backend"),
  videoDevice: document.querySelector("#video-device"),
  audioDevice: document.querySelector("#audio-device"),
  captureDetails: document.querySelector("#capture-details"),
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
  // Preview
  previewToggle: document.querySelector("#preview-toggle"),
  previewContainer: document.querySelector("#preview-container"),
  previewImg: document.querySelector("#preview-img"),
};

let selectedPreset = "recommended";
let lastDeviceScan = null;
let pendingVideoDevice = "";
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

  updateButtonStates(stream.state);
}

function loadConfigIntoForm(config, presets) {
  selectedPreset = config.preset.id;
  els.ingestURL.value = config.ingestUrl || "";
  els.streamName.placeholder = config.hasStreamKey
    ? "Stream key is set (enter a new one to replace)"
    : "Paste your stream key here";
  els.inputKind.value = config.input.kind || "test-video";
  els.inputBackend.value = config.input.backend || "avfoundation";
  // Store pending device selections — will be applied when device dropdown populates.
  pendingVideoDevice = config.input.videoDevice || "";
  pendingAudioDevice = config.input.audioDevice || "";
  els.videoDevice.value = pendingVideoDevice;
  els.audioDevice.value = pendingAudioDevice;
  // Output mode
  if (els.outputMode) {
    els.outputMode.value = config.outputMode || "rtmp";
    updateOutputModeVisibility();
  }
  if (config.hlsUrl && els.hlsUrl) {
    els.hlsUrl.textContent = config.hlsUrl;
  }
  updateCaptureVisibility();
  renderPresets(presets);
}

function setPlatformDefault(platform) {
  // If the backend is still the default lavfi (test video), set platform default
  // so switching away from test-video picks the right one.
  if (els.inputKind.value === "test-video") {
    els.inputBackend.value = platform;
  }
}

function updateButtonStates(state) {
  const isActive = ["starting", "running", "degraded", "restarting"].includes(state);
  const isStopping = state === "stopping";
  els.start.disabled = isActive || isStopping;
  els.stop.disabled = !isActive;
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
function updateCaptureVisibility() {
  const kind = els.inputKind.value;
  const isTest = kind === "test-video";
  els.captureDetails.hidden = isTest;

  if (!isTest) {
    // Show/hide the backend dropdown based on input kind.
    // SDI always uses decklink. Webcam/HDMI use the platform backend.
    const backendLabel = els.inputBackend.closest("label");
    if (kind === "sdi") {
      // SDI = DeckLink, no choice needed.
      backendLabel.hidden = true;
      els.inputBackend.value = "decklink";
    } else if (kind === "hdmi") {
      // HDMI could be DeckLink or platform USB capture — show dropdown
      // but filter to relevant options.
      backendLabel.hidden = false;
    } else {
      // Webcam — show platform backends, hide decklink.
      backendLabel.hidden = false;
      if (els.inputBackend.value === "decklink") {
        els.inputBackend.value = lastDeviceScan?.backend || "avfoundation";
      }
    }
  }
}

// --- Presets ---
function renderPresets(presets) {
  els.presets.innerHTML = "";
  for (const preset of presets) {
    const button = document.createElement("button");
    button.className = `preset ${preset.id === selectedPreset ? "active" : ""}`;
    const fpsLabel = preset.fps === 60 ? "60" : "";
    button.innerHTML = `
      <strong>${preset.name} &middot; ${preset.height}p${fpsLabel} &middot; ${preset.videoKbps / 1000} Mbps</strong>
      <span>${preset.description} Upload: ${preset.uploadTarget}.</span>
    `;
    button.addEventListener("click", () => {
      selectedPreset = preset.id;
      renderPresets(presets);
      showNotice("Preset selected — click Save Settings to apply.");
    });
    els.presets.appendChild(button);
  }
}

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
  const payload = {
    presetId: selectedPreset,
    ingestUrl: els.ingestURL.value.trim(),
    outputMode: els.outputMode?.value || "rtmp",
    input: {
      kind: els.inputKind.value,
      backend: els.inputKind.value === "test-video" ? "lavfi" : els.inputBackend.value,
      videoDevice: els.videoDevice.value.trim(),
      audioDevice: els.audioDevice.value.trim(),
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
  els.inputKind.value = result.input.kind || "test-video";
  if (result.input.kind !== "test-video") {
    els.inputBackend.value = result.input.backend || "avfoundation";
  }
  // Set pending so next device scan picks up the values.
  pendingVideoDevice = result.input.videoDevice || "";
  pendingAudioDevice = result.input.audioDevice || "";
  els.videoDevice.value = pendingVideoDevice;
  els.audioDevice.value = pendingAudioDevice;
  if (els.outputMode) els.outputMode.value = result.outputMode || "rtmp";
  if (result.hlsUrl && els.hlsUrl) els.hlsUrl.textContent = result.hlsUrl;
  updateOutputModeVisibility();
  updateCaptureVisibility();
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

els.save.addEventListener("click", async () => {
  try {
    els.save.disabled = true;
    await saveConfig();
    showNotice("Settings saved.");
    await refresh();
  } catch (error) {
    els.lastMessage.textContent = error.message;
  } finally {
    els.save.disabled = false;
  }
});

// --- Destination mode tabs ---
document.querySelectorAll("#dest-tabs .tab").forEach((tab) => {
  tab.addEventListener("click", () => {
    document.querySelectorAll("#dest-tabs .tab").forEach((t) => t.classList.remove("active"));
    tab.classList.add("active");
    currentDestMode = tab.dataset.mode;
    document.querySelectorAll(".mode-content").forEach((el) => (el.hidden = true));
    document.querySelector(`#mode-${currentDestMode}`).hidden = false;
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

function renderYouTubeStatus(yt) {
  const authed = yt.authenticated;
  const configured = yt.configured;

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
  // datetime-local gives us YYYY-MM-DDTHH:MM — we need to interpret in the selected tz
  // Since we can't easily do timezone conversion in vanilla JS, send the raw value and timezone
  // and let the server handle it. For now, treat as UTC offset approximation.
  const dt = new Date(datetimeStr);

  const override = {
    name: document.querySelector("#ovr-name").value.trim(),
    startTime: dt.toISOString(),
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

// --- Device discovery ---

// Map input kind to the correct backend.
function backendForKind(kind) {
  switch (kind) {
    case "sdi": return "decklink";
    case "test-video": return "lavfi";
    default: return null; // use platform default
  }
}

async function scanDevices(forceRefresh) {
  try {
    const backend = els.inputBackend.value;
    const url = `/api/devices?backend=${backend}${forceRefresh ? "&refresh=1" : ""}`;
    const data = await api(url);
    lastDeviceScan = data;

    // Populate video device dropdown.
    const currentVideo = pendingVideoDevice || els.videoDevice.value;
    els.videoDevice.innerHTML = "";
    if (data.video && data.video.length > 0) {
      for (const d of data.video) {
        const opt = document.createElement("option");
        opt.value = d.index;
        opt.textContent = d.backend === "decklink" ? d.name : `[${d.index}] ${d.name}`;
        els.videoDevice.appendChild(opt);
      }
      if (currentVideo && [...els.videoDevice.options].some((o) => o.value === currentVideo)) {
        els.videoDevice.value = currentVideo;
      }
    } else {
      const opt = document.createElement("option");
      opt.value = "";
      opt.textContent = "No devices found";
      els.videoDevice.appendChild(opt);
    }

    // Populate audio device dropdown.
    const currentAudio = pendingAudioDevice || els.audioDevice.value;
    els.audioDevice.innerHTML = "";
    const noneOpt = document.createElement("option");
    noneOpt.value = "";
    noneOpt.textContent = backend === "decklink" ? "Embedded (SDI audio)" : "None (use default)";
    els.audioDevice.appendChild(noneOpt);
    if (data.audio && data.audio.length > 0) {
      for (const d of data.audio) {
        const opt = document.createElement("option");
        opt.value = d.index;
        opt.textContent = d.backend === "decklink" ? d.name : `[${d.index}] ${d.name}`;
        els.audioDevice.appendChild(opt);
      }
      if (currentAudio && [...els.audioDevice.options].some((o) => o.value === currentAudio)) {
        els.audioDevice.value = currentAudio;
      }
    }

    pendingVideoDevice = "";
    pendingAudioDevice = "";

    const vidCount = data.video?.length || 0;
    const audCount = data.audio?.length || 0;
    if (els.deviceStatus) {
      if (vidCount === 0 && audCount === 0) {
        els.deviceStatus.textContent = `No ${backend} devices found. Connect a device and click Refresh.`;
      } else {
        els.deviceStatus.textContent = `${vidCount} video, ${audCount} audio (${backend}). Devices auto-refresh every 5s.`;
      }
    }
  } catch (_) {}
}

// syncBackendToKind is now handled inside updateCaptureVisibility.
function syncBackendToKind() {}

// Poll for devices every 5 seconds.
scanDevices();
setInterval(() => scanDevices(), 5000);

// Refresh button.
document.querySelector("#refresh-devices")?.addEventListener("click", () => scanDevices(true));

// When backend changes, rescan devices immediately.
els.inputBackend.addEventListener("change", () => {
  scanDevices(true);
  saveAndRestartPreview();
});

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
els.inputKind.addEventListener("change", () => {
  updateCaptureVisibility();
  syncBackendToKind();
  scanDevices(true);
  saveAndRestartPreview();
});
els.videoDevice.addEventListener("change", saveAndRestartPreview);
els.audioDevice.addEventListener("change", saveAndRestartPreview);

// --- Init ---
refresh();
setInterval(refresh, 2000);

// Load schedules and overrides on init
setTimeout(() => {
  loadSchedules();
  loadOverrides();
}, 500);
