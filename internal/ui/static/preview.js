// EasyStream — WebRTC preview controller.
//
// Encapsulates the PeerConnection lifecycle and the audio-meter graph
// behind a small imperative API. The app component owns *when* to start
// or stop the preview; this module owns *how*. State changes are pushed
// back via callbacks so Alpine reactivity remains the source of truth
// for what the user sees.
//
// Usage:
//   const ctrl = window.EasyStreamPreview.create({
//     videoEl: <video>,
//     onAudioLevel: (level, peak, text, active) => { ... },
//     onError: (msg) => { ... },
//   });
//   ctrl.start();   // creates PC, posts offer to /api/preview/webrtc/offer
//   ctrl.stop();    // closes PC, tears down meter
//   ctrl.refresh(); // stop + start
window.EasyStreamPreview = (() => {
  function create({ videoEl, onAudioLevel, onError }) {
    let pc = null;
    let starting = false;
    let connectTimeout = null;

    // Audio meter — Web Audio analyser fed off the incoming audio track.
    // We also poll getStats() at 500 ms as a fallback when the AudioContext
    // is suspended (browsers gate it on user interaction).
    let audioCtx = null;
    let analyser = null;
    let analyserData = null;
    let analyserSrc = null;
    let analyserGain = null;
    let statsInterval = null;
    let raf = null;
    let lastEnergy = null;
    let lastDuration = null;
    let peakHold = 0;
    let smoothedLevel = 0;
    let fallbackLevel = null;

    function emitAudio(level, peak, text, active) {
      if (onAudioLevel) onAudioLevel(level, peak, text, active);
    }

    function stop() {
      clearTimeout(connectTimeout);
      stopMeter();
      if (pc) {
        try { pc.close(); } catch (_) {}
        pc = null;
      }
      if (videoEl) videoEl.srcObject = null;
    }

    function stopMeter() {
      if (statsInterval) clearInterval(statsInterval);
      if (raf) cancelAnimationFrame(raf);
      statsInterval = null;
      raf = null;
      try { analyserSrc?.disconnect(); } catch (_) {}
      try { analyser?.disconnect(); } catch (_) {}
      try { analyserGain?.disconnect(); } catch (_) {}
      if (audioCtx && audioCtx.state !== "closed") audioCtx.close().catch(() => {});
      audioCtx = analyser = analyserData = analyserSrc = analyserGain = null;
      lastEnergy = lastDuration = null;
      peakHold = 0;
      smoothedLevel = 0;
      fallbackLevel = null;
      emitAudio(0, 0, "No audio", false);
    }

    function attachAudioTrack(track) {
      stopAnalyserGraph();
      const Ctor = window.AudioContext || window.webkitAudioContext;
      if (!Ctor || !track) return;
      try {
        audioCtx = new Ctor();
        analyserSrc = audioCtx.createMediaStreamSource(new MediaStream([track]));
        analyser = audioCtx.createAnalyser();
        analyserGain = audioCtx.createGain();
        analyser.fftSize = 512;
        analyser.smoothingTimeConstant = 0.25;
        analyserGain.gain.value = 0; // silent — graph stays active without speaker output
        analyserSrc.connect(analyser);
        analyser.connect(analyserGain);
        analyserGain.connect(audioCtx.destination);
        analyserData = new Uint8Array(analyser.fftSize);
        resumeAudio();
      } catch (_) {
        stopAnalyserGraph();
      }
    }

    function stopAnalyserGraph() {
      try { analyserSrc?.disconnect(); } catch (_) {}
      try { analyser?.disconnect(); } catch (_) {}
      try { analyserGain?.disconnect(); } catch (_) {}
      if (audioCtx && audioCtx.state !== "closed") audioCtx.close().catch(() => {});
      audioCtx = analyser = analyserData = analyserSrc = analyserGain = null;
    }

    function resumeAudio() {
      if (audioCtx?.state === "suspended") audioCtx.resume().catch(() => {});
    }

    function readAnalyserLevel() {
      if (!analyser || !analyserData) return null;
      if (audioCtx?.state === "suspended") {
        resumeAudio();
        return null;
      }
      analyser.getByteTimeDomainData(analyserData);
      let max = 0;
      for (const sample of analyserData) {
        const val = Math.abs(sample - 128);
        if (val > max) max = val;
      }
      return max / 128;
    }

    function renderMeter() {
      let raw = readAnalyserLevel();
      if (raw == null) raw = fallbackLevel;
      if (raw == null) {
        smoothedLevel = Math.max(0, smoothedLevel - 0.05);
        peakHold = Math.max(0, peakHold - 0.01);
        emitAudio(smoothedLevel, peakHold, "Waiting", false);
        return;
      }
      const clamped = Math.min(1, Math.max(0, raw));
      const db = clamped > 0 ? 20 * Math.log10(clamped) : -60;
      const display = Math.min(1, Math.max(0, (db + 60) / 60));
      if (display > smoothedLevel) smoothedLevel = display;
      else smoothedLevel = Math.max(0, smoothedLevel - 0.04);
      if (display > peakHold) peakHold = display;
      else peakHold = Math.max(0, peakHold - 0.005);
      const text = clamped > 0 ? `${Math.round(Math.max(-60, db))} dB` : "-∞ dB";
      emitAudio(smoothedLevel, peakHold, text, clamped > 0.001);
    }

    function startMeter(currentPC) {
      stopMeter();
      // getStats fallback.
      statsInterval = setInterval(async () => {
        if (!currentPC || currentPC !== pc) return;
        try {
          const stats = await currentPC.getStats();
          if (currentPC !== pc) return;
          let level = null;
          stats.forEach((report) => {
            if (report.type !== "inbound-rtp") return;
            if (report.kind !== "audio" && report.mediaType !== "audio") return;
            if (typeof report.audioLevel === "number") { level = report.audioLevel; return; }
            if (typeof report.totalAudioEnergy !== "number" || typeof report.totalSamplesDuration !== "number") return;
            if (lastEnergy == null) {
              lastEnergy = report.totalAudioEnergy;
              lastDuration = report.totalSamplesDuration;
              return;
            }
            const dE = report.totalAudioEnergy - lastEnergy;
            const dT = report.totalSamplesDuration - lastDuration;
            lastEnergy = report.totalAudioEnergy;
            lastDuration = report.totalSamplesDuration;
            if (dE >= 0 && dT > 0) level = Math.sqrt(dE / dT);
          });
          if (level != null) fallbackLevel = level;
        } catch (_) {}
      }, 500);

      // RAF render loop. Pauses automatically when the tab is hidden.
      const render = () => {
        if (currentPC !== pc) return;
        raf = requestAnimationFrame(render);
        renderMeter();
      };
      raf = requestAnimationFrame(render);
    }

    async function start() {
      if (starting) return;
      starting = true;
      try {
        stop();
        const newPC = new RTCPeerConnection();
        pc = newPC;
        newPC.addTransceiver("video", { direction: "recvonly" });
        newPC.addTransceiver("audio", { direction: "recvonly" });
        startMeter(newPC);

        newPC.ontrack = (e) => {
          const stream = e.streams && e.streams[0] ? e.streams[0] : new MediaStream([e.track]);
          if (e.track.kind === "video" && videoEl) videoEl.srcObject = stream;
          if (e.track.kind === "audio") attachAudioTrack(e.track);
        };

        let connected = false;
        connectTimeout = setTimeout(() => {
          if (!connected && pc === newPC) {
            if (onError) onError("Could not connect to preview. Click Refresh to try again.");
            stop();
          }
        }, 10000);

        newPC.oniceconnectionstatechange = () => {
          if (newPC.iceConnectionState === "connected" || newPC.iceConnectionState === "completed") {
            connected = true;
            clearTimeout(connectTimeout);
          } else if (newPC.iceConnectionState === "failed" || newPC.iceConnectionState === "closed") {
            if (pc === newPC) {
              if (onError) onError("Preview disconnected. Click Refresh.");
              stop();
            }
          }
        };

        const offer = await newPC.createOffer();
        await newPC.setLocalDescription(offer);
        const resp = await fetch("/api/preview/webrtc/offer", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(newPC.localDescription),
        });
        if (!resp.ok) throw new Error((await resp.text()) || `HTTP ${resp.status}`);
        const answer = await resp.json();
        await newPC.setRemoteDescription(answer);
      } catch (e) {
        if (onError) onError(`Preview connection failed: ${e.message}`);
        stop();
      } finally {
        starting = false;
      }
    }

    function refresh() {
      stop();
      // Defer start one tick so any in-flight RAF/interval observes the cleared pc first.
      setTimeout(start, 0);
    }

    return { start, stop, refresh, resumeAudio, isRunning: () => pc !== null };
  }

  return { create };
})();
