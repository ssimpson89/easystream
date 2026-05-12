// EasyStream — pure formatting helpers. No Alpine, no DOM, no state.
// Attached to window.EasyStreamFormat for use from app.js.
//
// All functions return strings safe to put into x-text bindings.
window.EasyStreamFormat = (() => {
  function fmtTime(d) {
    return d.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
  }

  function formatEventWhen(d) {
    const now = new Date();
    const sameDay = d.toDateString() === now.toDateString();
    const tomorrow = new Date(now.getTime() + 86400000);
    const isTomorrow = d.toDateString() === tomorrow.toDateString();
    const timeStr = fmtTime(d);
    if (sameDay) return `Today at ${timeStr}`;
    if (isTomorrow) return `Tomorrow at ${timeStr}`;
    const dateStr = d.toLocaleDateString(undefined, { weekday: "long", month: "short", day: "numeric" });
    return `${dateStr} at ${timeStr}`;
  }

  function formatDateTime(d) {
    const dateStr = d.toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric" });
    const timeStr = d.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
    return `${dateStr} at ${timeStr}`;
  }

  function formatDays(days) {
    return (days || []).map((d) => d.charAt(0).toUpperCase() + d.slice(1, 3)).join(", ");
  }

  function shortTZ(tz) {
    return (tz || "").split("/").pop().replace("_", " ");
  }

  function platformFromURL(url) {
    if (!url) return null;
    const u = url.toLowerCase();
    if (u.includes("youtube.com")) return "YouTube";
    if (u.includes("cloudflare.com")) return "Cloudflare";
    if (u.includes("twitch.tv")) return "Twitch";
    if (u.includes("facebook.com") || u.includes("fb.com")) return "Facebook";
    return null;
  }

  // redactUrl scrubs userinfo and known secret query params from a
  // URL string before rendering it in the UI. Mirrors the server's
  // RedactURLCredentials so what the operator sees on the dashboard
  // never includes a camera password or SRT passphrase.
  function redactUrl(url) {
    if (!url) return url;
    try {
      const u = new URL(url);
      if (u.username || u.password) {
        u.username = "•••";
        u.password = "•••";
      }
      const secretKeys = ["passphrase", "password", "token", "key", "secret"];
      let changed = false;
      for (const k of [...u.searchParams.keys()]) {
        if (secretKeys.includes(k.toLowerCase())) {
          u.searchParams.set(k, "•••");
          changed = true;
        }
      }
      if (u.username || changed) return u.toString();
      return url;
    } catch (_) {
      // Not a parseable URL — return as-is rather than mangle it.
      return url;
    }
  }

  function presetTitle(p) {
    if (!p) return "-";
    const fps = p.fps === 60 ? "60" : "";
    return `${p.name} · ${p.height}p${fps} · ${p.videoKbps / 1000} Mbps`;
  }

  // Relative-time formatter for "starts in 1h 15m" style countdowns.
  // Returns "" when the event is in the past.
  function relativeUntil(d) {
    const diff = d.getTime() - Date.now();
    if (diff <= 0) return "";
    const totalMin = Math.round(diff / 60000);
    if (totalMin < 60) return `in ${totalMin} min`;
    const hours = Math.floor(totalMin / 60);
    const mins = totalMin % 60;
    if (mins === 0) return `in ${hours} hr`;
    return `in ${hours} hr ${mins} min`;
  }

  // Live-uptime formatter: "0:23:14" tabular.
  function elapsedSince(start) {
    if (!start) return "0:00:00";
    const sec = Math.max(0, Math.floor((Date.now() - start.getTime()) / 1000));
    const h = Math.floor(sec / 3600);
    const m = Math.floor((sec % 3600) / 60);
    const s = sec % 60;
    return `${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
  }

  return { fmtTime, formatEventWhen, formatDateTime, formatDays, shortTZ, platformFromURL, redactUrl, presetTitle, relativeUntil, elapsedSince };
})();
