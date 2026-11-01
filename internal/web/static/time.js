// Timestamps in the browser's locale and time zone. The server renders
// <time datetime="RFC 3339" data-ts="KEY">fallback text</time>; the fallback is in the server's
// zone and is what shows without JavaScript. Here the same instant is formatted with
// Intl.DateTimeFormat. KEY picks the fields (same as the fallback's, see ui/stamp.go). An
// element that carries a title gets the full date, time and zone name as its title.
// No inline script: CSP forbids it.
(function () {
  "use strict";
  var D = { year: "numeric", month: "2-digit", day: "2-digit" };
  var HM = { hour: "2-digit", minute: "2-digit" };
  var HMS = { hour: "2-digit", minute: "2-digit", second: "2-digit" };
  var SPECS = {
    "datetime-s": Object.assign({}, D, HMS),
    datetime: Object.assign({}, D, HM),
    "time-s": HMS,
    time: HM,
    // a log column: 24-hour like the device's own console, and no AM/PM to widen the fixed column
    "clock-24": Object.assign({ hourCycle: "h23" }, HMS),
    short: Object.assign({ year: "numeric", month: "short", day: "numeric" }, HM),
  };
  var FULL = Object.assign({}, D, HMS, { timeZoneName: "short" });
  var cache = {};
  function fmt(key, spec) {
    if (!cache[key]) cache[key] = new Intl.DateTimeFormat(undefined, spec);
    return cache[key];
  }
  // store: a storage table's "last backup" cell: this year without the year, older with only the day
  function storeText(d) {
    var thisYear = d.getFullYear() === new Date().getFullYear();
    return fmt(thisYear ? "store-now" : "store-old", thisYear
      ? Object.assign({ day: "numeric", month: "short" }, HM)
      : { day: "numeric", month: "short", year: "numeric" }).format(d);
  }
  // day: "Today 09:41", "Yesterday 21:10", else "19 Aug, 20:02" ("19 Aug 2025, 20:02" in another
  // year). Today and Yesterday are the calendar days of the browser's zone, now; the same rule as
  // ui.StampDayText, which is the server's fallback.
  function dayText(d) {
    var now = new Date();
    var days = Math.round((new Date(now.getFullYear(), now.getMonth(), now.getDate()) -
      new Date(d.getFullYear(), d.getMonth(), d.getDate())) / 86400000);
    if (days === 0) return "Today " + fmt("time", HM).format(d);
    if (days === 1) return "Yesterday " + fmt("time", HM).format(d);
    var sameYear = d.getFullYear() === now.getFullYear();
    return fmt(sameYear ? "day-year" : "day-old", sameYear
      ? Object.assign({ day: "numeric", month: "short" }, HM)
      : Object.assign({ day: "numeric", month: "short", year: "numeric" }, HM)).format(d);
  }
  // clock-day: today's instant is the bare clock time, any other day is dayText (#422). The server's
  // fallback is ui.StampClockOrDayText.
  function clockOrDayText(d) {
    var now = new Date();
    var same = d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate();
    return same ? fmt("time", HM).format(d) : dayText(d);
  }
  // textOf is the text for a format key and an instant ("" when the key is unknown). "title"
  // leaves the text alone, so it is "" here and handled by the caller.
  function textOf(key, d) {
    if (key === "store") return storeText(d);
    if (key === "day") return dayText(d);
    if (key === "clock-day") return clockOrDayText(d);
    return SPECS[key] ? fmt(key, SPECS[key]).format(d) : "";
  }
  function paint(root) {
    (root || document).querySelectorAll("time[data-ts]").forEach(function (el) {
      var key = el.getAttribute("data-ts");
      var d = new Date(el.getAttribute("datetime"));
      if (isNaN(d.getTime())) return; // keep the server's text
      try {
        if (key !== "title") {
          var text = textOf(key, d);
          if (!text) return;
          el.textContent = text;
        }
        if (el.hasAttribute("title")) el.title = fmt("full", FULL).format(d);
      } catch (err) {
        // an unsupported locale or zone: the server's text stays
      }
    });
  }
  // formatFor is the same text for code that builds a sentence (dialog.js); "" when unknown.
  window.tasmofleetFormatTime = function (iso, key) {
    var d = new Date(iso);
    try {
      return isNaN(d.getTime()) ? "" : textOf(key, d);
    } catch (err) {
      return "";
    }
  };
  window.tasmofleetPaintTimes = paint;
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", function () { paint(); });
  } else {
    paint();
  }
  document.body.addEventListener("htmx:afterSettle", function () { paint(); });
  // "Today" turns into "Yesterday" at the browser's midnight, and a hidden tab's timers sleep:
  // repaint at the next midnight and when the tab comes back.
  function untilMidnight() {
    var n = new Date();
    return new Date(n.getFullYear(), n.getMonth(), n.getDate() + 1, 0, 0, 1) - n;
  }
  function paintAtMidnight() {
    setTimeout(function () { paint(); paintAtMidnight(); }, Math.max(untilMidnight(), 1000));
  }
  paintAtMidnight();
  document.addEventListener("visibilitychange", function () { if (!document.hidden) paint(); });
  // timers also pause while the machine sleeps over midnight
  window.addEventListener("focus", function () { paint(); });
  window.addEventListener("pageshow", function () { paint(); });
})();
