// EasyStream — capture-source helpers. Builds device-listing options
// from /api/devices and round-trips the "kind:backend:device" form used
// by <select> values.
window.EasyStreamSources = (() => {
  const TYPE_LABELS = {
    camera:         "Cameras",
    "capture-card": "Capture cards",
    screen:         "Screen capture",
    sdi:            "SDI",
  };
  const TYPE_ORDER = ["camera", "capture-card", "screen", "sdi"];
  const AUDIO_LABELS = { microphone: "Microphones", "audio-input": "Audio inputs" };
  const AUDIO_ORDER = ["microphone", "audio-input"];

  function kindForDeviceType(type) {
    switch (type) {
      case "sdi": return "sdi";
      case "capture-card": return "hdmi";
      default: return "webcam";
    }
  }

  // Sentinel values used as <select> option values for non-device
  // sources. The actual configuration (URL, port, passphrase) lives
  // in separate fields; the sentinel just keeps the dropdown's
  // selected state stable across edits.
  const NETWORK_SENTINEL       = "network::";
  const SRT_LISTENER_SENTINEL  = "srt-listener::";

  function encodeSourceValue(input, devices) {
    if (!input || input.kind === "test-video") return "test-video::";
    if (input.kind === "network") return NETWORK_SENTINEL;
    if (input.kind === "srt-listener") return SRT_LISTENER_SENTINEL;
    const kind = input.kind || "webcam";
    const backend = input.backend || "avfoundation";
    let device = input.videoDevice || "";
    if (input.videoDeviceName && (devices?.video || []).length > 0) {
      const match = devices.video.find((d) => d.name === input.videoDeviceName && d.backend === backend);
      if (match) device = String(match.index);
    }
    return `${kind}:${backend}:${device}`;
  }

  function decodeSourceValue(value) {
    if (!value) return null;
    if (value === "test-video::") return { kind: "test-video", backend: "lavfi", videoDevice: "" };
    if (value === NETWORK_SENTINEL) return { kind: "network", backend: "", videoDevice: "" };
    if (value === SRT_LISTENER_SENTINEL) return { kind: "srt-listener", backend: "", videoDevice: "" };
    const [kind, backend, ...rest] = value.split(":");
    return { kind, backend, videoDevice: rest.join(":") };
  }

  function videoSourceOptions(devices) {
    const out = [
      { key: "group:test", value: "__group:test", label: "Test source", disabled: true },
      { key: "test-video", value: "test-video::", label: "  Test pattern (no hardware)", disabled: false },
      { key: "group:network", value: "__group:network", label: "Network stream", disabled: true },
      { key: "network", value: NETWORK_SENTINEL, label: "  Pull from URL (RTSP / SRT / UDP / HTTP)", disabled: false },
      { key: "srt-listener", value: SRT_LISTENER_SENTINEL, label: "  Receive SRT push (listen on a port)", disabled: false },
    ];
    for (const t of TYPE_ORDER) {
      const matches = (devices?.video || []).filter((d) => d.type === t);
      if (matches.length === 0) continue;
      out.push({ key: `group:${t}`, value: `__group:${t}`, label: TYPE_LABELS[t] || "Video", disabled: true });
      for (const d of matches) {
        const kind = kindForDeviceType(d.type);
        const label = d.backend === "decklink" ? d.name : `${d.name} [${d.index}]`;
        out.push({
          key: `${kind}:${d.backend}:${d.index}`,
          value: `${kind}:${d.backend}:${d.index}`,
          label: `  ${label}`,
          disabled: false,
        });
      }
    }
    return out;
  }

  function audioSourceOptions(devices, sdiSelected) {
    const out = [{
      key: "silent", value: "",
      label: sdiSelected ? "Embedded SDI audio" : "None / silent",
      disabled: false,
    }];
    for (const t of AUDIO_ORDER) {
      const matches = (devices?.audio || []).filter((d) => d.type === t);
      if (matches.length === 0) continue;
      out.push({ key: `group:${t}`, value: `__group:${t}`, label: AUDIO_LABELS[t] || "Audio", disabled: true });
      for (const d of matches) {
        out.push({ key: `${t}:${d.index}`, value: d.index, label: `  ${d.name} [${d.index}]`, disabled: false });
      }
    }
    return out;
  }

  return { kindForDeviceType, encodeSourceValue, decodeSourceValue, videoSourceOptions, audioSourceOptions };
})();
