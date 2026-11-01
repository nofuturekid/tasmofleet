// Marks the fields of a form[data-track-changes] that differ from the value the
// device reported (the hidden orig.<name> field next to each), and counts them.
// Without this file the form works the same; nothing is marked.
(function () {
  "use strict";

  function fields(form) {
    return Array.prototype.slice
      .call(form.querySelectorAll("input[name], select[name]"))
      .filter(function (f) {
        return (
          f.name.indexOf("orig.") !== 0 &&
          f.type !== "hidden" &&
          f.type !== "checkbox" &&
          form.elements["orig." + f.name]
        );
      });
  }
  function sync(form) {
    var n = 0;
    fields(form).forEach(function (f) {
      var changed =
        f.type === "password"
          ? f.value !== ""
          : f.value !== form.elements["orig." + f.name].value;
      var label = f.closest("label") || f.closest(".kv-row");
      if (label) {
        label.classList.toggle("is-changed", changed);
      }
      if (changed) {
        n++;
      }
    });
    form.querySelectorAll("[data-changed-count]").forEach(function (c) {
      c.textContent =
        n === 0
          ? "No changes yet"
          : n + (n === 1 ? " setting changed" : " settings changed");
    });
  }
  function all() {
    document.querySelectorAll("form[data-track-changes]").forEach(sync);
  }
  document.addEventListener("input", function (e) {
    var form =
      e.target instanceof Element
        ? e.target.closest("form[data-track-changes]")
        : null;
    if (form) {
      sync(form);
    }
  });
  document.addEventListener("change", function (e) {
    var form =
      e.target instanceof Element
        ? e.target.closest("form[data-track-changes]")
        : null;
    if (form) {
      sync(form);
    }
  });
  // Typed values survive a step-up. A form[data-keep-draft] saves its plain
  // fields to sessionStorage when it is submitted (passwords are never kept)
  // and puts them back when the page is opened with ?restore=1, the address the
  // step-up returns to. Any other visit drops the draft. Without storage the
  // form works as before, minus the restore.
  function draftKey(form) {
    return "tf-draft:" + form.getAttribute("data-keep-draft");
  }
  function draftForms() {
    return document.querySelectorAll("form[data-keep-draft]");
  }
  function draftRestore() {
    var want = /[?&]restore=1(&|$)/.test(location.search);
    draftForms().forEach(function (form) {
      try {
        var raw = sessionStorage.getItem(draftKey(form));
        sessionStorage.removeItem(draftKey(form));
        if (!want || !raw) {
          return;
        }
        var d = JSON.parse(raw);
        Object.keys(d).forEach(function (name) {
          var f = form.elements[name];
          if (!f || f.type === "password" || f.type === "hidden") {
            return;
          }
          if (f.type === "checkbox") {
            f.checked = d[name] === "1";
          } else {
            f.value = d[name];
          }
        });
      } catch (e) {
        /* storage unavailable or bad draft: nothing to restore */
      }
    });
  }
  document.addEventListener("submit", function (e) {
    var form = e.target;
    if (
      !(form instanceof HTMLFormElement) ||
      !form.hasAttribute("data-keep-draft")
    ) {
      return;
    }
    try {
      var d = {};
      Array.prototype.forEach.call(form.elements, function (f) {
        if (
          !f.name ||
          f.type === "password" ||
          f.type === "hidden" ||
          f.type === "submit" ||
          f.type === "button" ||
          (f.type === "radio" && !f.checked)
        ) {
          return;
        }
        d[f.name] = f.type === "checkbox" ? (f.checked ? "1" : "0") : f.value;
      });
      sessionStorage.setItem(draftKey(form), JSON.stringify(d));
    } catch (err) {
      /* storage unavailable: no restore */
    }
  });
  // The device list's pill row scrolls sideways on a narrow screen, and every filter choice loads the
  // page again, which would put the row back to its start and move every pill (#116). The scroll
  // position is kept across that one reload: saved when a pill or a select in the row is used, restored
  // (once) on the next page.
  var chipsKey = "tf-chips-scroll";
  function saveChipsScroll(e) {
    var row = e.target instanceof Element && e.target.closest(".dv-chips");
    if (row) {
      try {
        sessionStorage.setItem(chipsKey, String(row.scrollLeft));
      } catch (err) {
        /* storage unavailable: the row starts at its beginning */
      }
    }
  }
  document.addEventListener("click", saveChipsScroll);
  document.addEventListener("change", saveChipsScroll);
  function restoreChipsScroll() {
    var row = document.querySelector(".dv-chips");
    try {
      var x = sessionStorage.getItem(chipsKey);
      sessionStorage.removeItem(chipsKey);
      if (row && x) {
        row.scrollLeft = Number(x);
      }
    } catch (err) {
      /* storage unavailable */
    }
    // An active pill the row hides (a dashboard or Backups tile opens the list filtered; on a phone
    // the row is wider than the screen) is scrolled into view, so the filter shows. Only the row
    // scrolls, never the page (#381).
    var on = row && row.querySelectorAll('.dv-chip[aria-current="true"]');
    if (on && on.length) {
      var chip = on[on.length - 1].getBoundingClientRect();
      var box = row.getBoundingClientRect();
      if (chip.right > box.right) {
        row.scrollLeft += chip.right - box.right;
      } else if (chip.left < box.left) {
        row.scrollLeft -= box.left - chip.left;
      }
    }
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", restoreChipsScroll);
  } else {
    restoreChipsScroll();
  }
  // A select with data-autosubmit applies its filter at once (the device list's Chip, Tag and
  // Sort); without this file the form's Filter button (noscript) submits it.
  document.addEventListener("change", function (e) {
    var t = e.target;
    if (
      t instanceof HTMLSelectElement &&
      t.hasAttribute("data-autosubmit") &&
      t.form
    ) {
      t.form.requestSubmit();
    }
  });
  // "Reset to default on save" (clear.KEY) wins over the field's own value: the
  // server refuses a setting that is set and cleared at once, and a text field
  // always sends its value. Without this file the server refuses such a save.
  document.addEventListener("submit", function (e) {
    var form = e.target;
    if (!(form instanceof HTMLFormElement)) {
      return;
    }
    form
      .querySelectorAll('input[type="checkbox"][name^="clear."]:checked')
      .forEach(function (box) {
        var key = box.name.slice("clear.".length);
        form
          .querySelectorAll('[name="v.' + key + '"], [name="b.' + key + '"]')
          .forEach(function (f) {
            f.disabled = true;
          });
      });
  });
  // A filter form (form[data-autosubmit]: the audit filter, the backup comparison's pickers)
  // applies a changed select, checkbox or radio at once; its submit button
  // (data-autosubmit-hide) is for browsers without this file. Text fields wait for the submit.
  document.addEventListener("change", function (e) {
    var t = e.target;
    var choice =
      t instanceof HTMLSelectElement ||
      (t instanceof HTMLInputElement &&
        (t.type === "checkbox" || t.type === "radio"));
    if (!choice || t.hasAttribute("data-autosubmit")) {
      return; // a select with its own data-autosubmit: the rule above submits it
    }
    var form = t.closest("form[data-autosubmit]");
    if (form) {
      form.requestSubmit();
    }
  });
  document.addEventListener("DOMContentLoaded", function () {
    document
      .querySelectorAll("form[data-autosubmit] [data-autosubmit-hide]")
      .forEach(function (b) {
        b.hidden = true;
      });
  });
  document.addEventListener("DOMContentLoaded", draftRestore);
  document.addEventListener("DOMContentLoaded", all);
  window.addEventListener("pageshow", all);
})();

// The import drop zone (Add devices, Import from TasmoAdmin): files dropped on a
// [data-dropzone] go into its file input, whose change uploads them (htmx,
// hx-trigger="change"). Without this file the "Choose files" button does the same.
(function () {
  "use strict";

  function zone(e) {
    return e.target instanceof Element
      ? e.target.closest("[data-dropzone]")
      : null;
  }
  document.addEventListener("dragover", function (e) {
    var z = zone(e);
    if (z) {
      e.preventDefault();
      z.classList.add("is-over");
    }
  });
  document.addEventListener("dragleave", function (e) {
    var z = zone(e);
    if (z && !z.contains(e.relatedTarget)) {
      z.classList.remove("is-over");
    }
  });
  document.addEventListener("drop", function (e) {
    var z = zone(e);
    if (!z || !e.dataTransfer) {
      return;
    }
    e.preventDefault();
    z.classList.remove("is-over");
    var input = z.querySelector('input[type="file"]');
    if (input && e.dataTransfer.files.length > 0) {
      input.files = e.dataTransfer.files;
      input.dispatchEvent(new Event("change", { bubbles: true }));
    }
  });
})();
