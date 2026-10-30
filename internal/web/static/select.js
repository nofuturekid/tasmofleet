// Device picker: "select all" checkboxes (with the indeterminate state), the
// quick selectors and the bulk bar. Without this file the same buttons are
// plain submit buttons that reload the page with the selection made on the
// server, so nothing depends on it. No inline script: CSP forbids that.
//
// The device list renders every device twice, as a table row and as a phone
// card (CSS shows one); both boxes carry the device id as their value. A
// device counts once, and ticking one box ticks its twin, so the selection
// survives a switch between the two layouts (and the server counts a posted
// id once).
(function () {
  "use strict";

  function picker(el) {
    return (
      el.closest("form") ||
      document.getElementById(el.getAttribute("data-picker") || "")
    );
  }
  function allBoxes(form) {
    return Array.prototype.slice.call(
      form.querySelectorAll(
        'input[type="checkbox"][name="id"]:not(:disabled), input[type="checkbox"][name="device"]:not(:disabled)',
      ),
    );
  }
  // one box per device: the first (the table row's; a card's twin follows it)
  function boxes(form) {
    var seen = {};
    return allBoxes(form).filter(function (b) {
      var k = b.name + "=" + b.value;
      if (seen[k]) {
        return false;
      }
      seen[k] = true;
      return true;
    });
  }
  // tick every box of the same device as b
  function twin(form, b) {
    allBoxes(form).forEach(function (x) {
      if (x !== b && x.name === b.name && x.value === b.value) {
        x.checked = b.checked;
      }
    });
  }
  function setAll(form, checked) {
    allBoxes(form).forEach(function (b) {
      b.checked = checked;
    });
  }
  function sync(form) {
    var list = boxes(form);
    var n = list.filter(function (b) {
      return b.checked;
    }).length;
    form.querySelectorAll("[data-select-all]").forEach(function (all) {
      all.checked = list.length > 0 && n === list.length;
      all.indeterminate = n > 0 && n < list.length;
    });
    document.querySelectorAll("[data-select-count]").forEach(function (c) {
      c.textContent = n + " of " + list.length + " selected";
    });
    form.querySelectorAll("[data-bar-count]").forEach(function (c) {
      c.textContent = String(n);
    });
    form.querySelectorAll("[data-bar-label]").forEach(function (c) {
      c.textContent = n === 1 ? "1 device selected" : n + " devices selected";
    });
    form.querySelectorAll("[data-select-n]").forEach(function (c) {
      c.textContent = n + " selected";
    });
    // the plan of a new update: devices, the different firmware files, the estimated time
    form.querySelectorAll("[data-plan-devices]").forEach(function (el) {
      el.textContent = String(n);
    });
    form.querySelectorAll("[data-plan-files]").forEach(function (el) {
      var files = {};
      list.forEach(function (b) {
        var r = b.checked ? rowOf(b) : null;
        var f = r ? r.getAttribute("data-file") : "";
        if (f) {
          files[f] = true;
        }
      });
      el.textContent = String(Object.keys(files).length);
    });
    form.querySelectorAll("[data-plan-est]").forEach(function (el) {
      var par = parseInt(el.getAttribute("data-parallel") || "1", 10) || 1;
      var per = parseInt(el.getAttribute("data-per-device") || "60", 10) || 60;
      el.textContent =
        n === 0
          ? "\u2013"
          : Math.round((Math.ceil(n / par) * per) / 60) + " min";
    });
    form.querySelectorAll("[data-plan-backup]").forEach(function (el) {
      var box = form.querySelector('input[name="backup"]');
      el.hidden = !!box && !box.checked;
    });
    // the experimental upload over MQTT can only be asked for together with push where possible
    form.querySelectorAll("[data-mqtt-upload]").forEach(function (el) {
      var sel = form.querySelector('select[name="delivery"]');
      var on = !!sel && sel.value === "push";
      el.hidden = !on;
      var box = el.querySelector("input[type=checkbox]");
      if (box && !on) {
        box.checked = false;
      }
    });
    // push where possible uploads to ESP32 only: say which selected devices download instead
    form.querySelectorAll("[data-push-hint]").forEach(function (el) {
      var sel = form.querySelector('select[name="delivery"]');
      var bad = 0;
      if (sel && sel.value === "push") {
        list.forEach(function (b) {
          var r = b.checked ? rowOf(b) : null;
          var f = r ? r.getAttribute("data-family") : "esp32";
          if (!f || f === "esp8266") {
            bad++;
          }
        });
      }
      el.hidden = bad === 0;
      el.textContent =
        bad === 0
          ? ""
          : "Push upload is for ESP32 devices only. " +
            (bad === 1
              ? "1 selected device is an ESP8266 or of unknown type and downloads its file instead."
              : bad +
                " selected devices are ESP8266 or of unknown type and download their files instead.");
    });
    // buttons that act on the selection stay off until something is selected
    form.querySelectorAll("[data-needs-selection]").forEach(function (b) {
      b.disabled = n === 0;
      b.title = n === 0 ? "Select devices first" : "";
    });
    // the bulk action bar shows as soon as something is selected
    form.querySelectorAll("[data-bulkbar]").forEach(function (b) {
      b.hidden = n === 0;
    });
    // Actions that need a live device: the server skips selected devices that do not answer (the
    // scope page lists them). Say how many here, and switch the actions off when none answers.
    var down = list.filter(function (b) {
      return b.checked && !answers(rowOf(b));
    }).length;
    form.querySelectorAll("[data-bar-note]").forEach(function (c) {
      c.hidden = down === 0;
      c.textContent =
        down === 0
          ? ""
          : down === n
            ? n === 1
              ? "it is not online"
              : "none of them is online"
            : down + " not online, will be skipped";
    });
    form
      .querySelectorAll("[data-bulkbar] [data-needs-device]")
      .forEach(function (b) {
        var none = n > 0 && down === n;
        b.disabled = none;
        // the reason is the visible note of the bar, not a tooltip (a button like "Send command"
        // is self-explanatory and takes none)
        if (none) {
          b.setAttribute("aria-describedby", "bulk-note");
        } else {
          b.removeAttribute("aria-describedby");
        }
      });
    // an action on exactly two rows (compare two backups) shows only then
    form.querySelectorAll("[data-needs-pair]").forEach(function (b) {
      b.hidden = n !== 2;
    });
  }
  // answers says whether the device of a row answers now (the health badge carries its state);
  // a row without a badge is assumed to.
  function answers(row) {
    var s = row && row.querySelector("[data-state]");
    var st = s ? s.getAttribute("data-state") : "online";
    return st === "online" || st === "stale";
  }
  // The row menus of the device list: Back up and Restart need a live device.
  function markDown() {
    document.querySelectorAll("[data-needs-device]").forEach(function (el) {
      if (!el.closest("[data-bulkbar]")) {
        var row = el.closest("tr, li.dcard");
        var off = !!row && !answers(row);
        if (off) {
          el.setAttribute("aria-disabled", "true");
          el.setAttribute("tabindex", "-1");
        } else {
          el.removeAttribute("aria-disabled");
          el.removeAttribute("tabindex");
        }
        // the reason is the row's own status badge beside the menu, not a tooltip
      }
    });
  }
  document.addEventListener(
    "click",
    function (e) {
      var t = e.target instanceof Element ? e.target : null;
      if (t && t.closest('[data-needs-device][aria-disabled="true"]')) {
        e.preventDefault();
        e.stopPropagation();
      }
    },
    true,
  );
  function syncAll() {
    markDown();
    document.querySelectorAll("[data-plan-devices]").forEach(function (p) {
      var f = picker(p);
      if (f) {
        sync(f);
      }
    });
    document.querySelectorAll("[data-select-all]").forEach(function (a) {
      a.hidden = false; // only useful with this script
      var row = a.closest("[data-select-all-row]");
      if (row) {
        row.hidden = false;
      }
      var f = picker(a);
      if (f) {
        sync(f);
      }
    });
  }
  // rowOf is the row of a box: a table row, or the label of a picker list
  function rowOf(b) {
    return b.closest("tr") || b.closest("[data-family]");
  }
  // wants says whether the quick selector picks the row.
  function wants(mode, row) {
    if (mode === "all") {
      return true;
    }
    if (mode === "none") {
      return false;
    }
    if (mode === "unused") {
      return !!row && row.getAttribute("data-unused") === "1";
    }
    if (mode === "update") {
      return !!row && row.getAttribute("data-update") === "1";
    }
    if (mode.indexOf("family:") === 0) {
      return !!row && row.getAttribute("data-family") === mode.slice(7);
    }
    return false;
  }

  document.addEventListener("change", function (e) {
    var t = e.target;
    if (
      (t instanceof HTMLSelectElement && t.name === "delivery") ||
      (t instanceof HTMLInputElement && t.name === "backup")
    ) {
      var df = picker(t);
      if (df) {
        sync(df);
      }
      return;
    }
    if (!(t instanceof HTMLInputElement)) {
      return;
    }
    var form = picker(t);
    if (!form) {
      return;
    }
    if (t.hasAttribute("data-select-all")) {
      setAll(form, t.checked);
    } else if (t.name === "id" || t.name === "device") {
      twin(form, t);
    }
    if (
      t.hasAttribute("data-select-all") ||
      t.name === "id" ||
      t.name === "device"
    ) {
      sync(form);
    }
  });

  document.addEventListener("click", function (e) {
    var btn =
      e.target instanceof Element ? e.target.closest("[data-pick]") : null;
    if (!btn) {
      return;
    }
    var form = picker(btn);
    if (!form) {
      return;
    }
    e.preventDefault(); // handled here; without the script the submit button does it on the server
    var menu = btn.closest("details");
    if (menu) {
      menu.open = false; // a quick selection menu closes once it has picked
    }
    var mode = btn.getAttribute("data-pick");
    boxes(form).forEach(function (b) {
      b.checked = wants(mode, rowOf(b));
      twin(form, b);
    });
    sync(form);
  });

  document.addEventListener("DOMContentLoaded", syncAll);
  // the browser restores checkbox states after back/forward, without a change event
  window.addEventListener("pageshow", syncAll);
  document.addEventListener("htmx:afterSettle", syncAll);

  // The device list reloads itself on server events. A reload would close the row menu or
  // dialog the user is in, so it waits until they are done. A selection alone does not block
  // it: the checked ids are put back after the swap (rows that are gone drop out) and the
  // htmx:afterSettle handler above recomputes the bulk bar.
  var deferred = false;
  var kept = null;
  function open(t) {
    return !!(
      t.querySelector("details[open]") ||
      document.querySelector("dialog[open]:not([data-static])")
    );
  }
  function flush() {
    var t = document.getElementById("devtable");
    if (!deferred || !t || open(t)) {
      return;
    }
    deferred = false;
    if (window.htmx) {
      window.htmx.ajax("GET", t.getAttribute("hx-get"), {
        target: t,
        swap: "innerHTML",
      });
    }
  }
  document.addEventListener("htmx:beforeSwap", function (e) {
    var t = e.detail && e.detail.target;
    if (!t || t.id !== "devtable") {
      return;
    }
    if (open(t)) {
      e.detail.shouldSwap = false;
      deferred = true;
      return;
    }
    kept = Array.prototype.map.call(
      t.querySelectorAll('input[name="id"]:checked'),
      function (b) {
        return b.value;
      },
    );
  });
  document.addEventListener("htmx:afterSwap", function (e) {
    var t = e.detail && e.detail.target;
    if (!t || t.id !== "devtable" || !kept) {
      return;
    }
    var ids = kept;
    kept = null;
    t.querySelectorAll('input[name="id"]').forEach(function (b) {
      b.checked = ids.indexOf(b.value) >= 0;
    });
  });
  // The network scan streams its results into #scan-live while the reader ticks rows (#144): the
  // ticked ids are put back after each swap, and htmx:afterSettle recomputes the bulk bar. The last
  // event replaces the whole #scan (outerHTML): its old element is detached after the swap and its
  // htmx:sseMessage no longer reaches the document, so htmx:afterSwap of the new one restores as well.
  var keptScan = null;
  function restoreScan() {
    var live = document.getElementById("scan-live");
    if (!keptScan || !live) {
      return;
    }
    var ids = keptScan;
    keptScan = null;
    live.querySelectorAll('input[name="id"]').forEach(function (b) {
      b.checked = ids.indexOf(b.value) >= 0;
    });
  }
  document.addEventListener("htmx:sseBeforeMessage", function (e) {
    var t = e.target;
    if (!t || (t.id !== "scan-live" && t.id !== "scan")) {
      return;
    }
    var live = document.getElementById("scan-live");
    keptScan = live
      ? Array.prototype.map.call(
          live.querySelectorAll('input[name="id"]:checked'),
          function (b) {
            return b.value;
          },
        )
      : null;
  });
  document.addEventListener("htmx:sseMessage", restoreScan);
  document.addEventListener("htmx:afterSwap", restoreScan);
  // toggle and close do not bubble: listen in the capture phase; wait a tick for the state to settle
  ["toggle", "close"].forEach(function (n) {
    document.addEventListener(
      n,
      function () {
        setTimeout(flush, 0);
      },
      true,
    );
  });
})();
