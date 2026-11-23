// salotto's browser client. No framework and no build step on purpose: this is
// served by the same binary that renders, and a toolchain in the serving path is
// a toolchain that has to be present at 3 a.m.
//
// The division of labour is the design from QUEUE.md §2.0. The server renders with
// the same View() the TUI and the SSH session use; this file converts that ANSI to
// HTML and adds the things a rendered string cannot carry (a sidebar, live typing,
// window switching). It never decides what a message looks like — it only decides
// where the server's own rendering goes.
(function () {
  "use strict";

  var termEl = document.getElementById("term");
  var listEl = document.getElementById("windows");
  var formEl = document.getElementById("input");
  var inputEl = document.getElementById("line");
  var bannerEl = document.getElementById("banner");
  var statusEl = document.getElementById("status");
  var searchEl = document.getElementById("search");
  var resultsEl = document.getElementById("results");
  var nicksEl = document.getElementById("nicks");
  var topicEl = document.getElementById("topic");
  var previewsEl = document.getElementById("previews");

  // -- ANSI → HTML ----------------------------------------------------------
  // Only the sequences the server actually emits: SGR colour (8-bit, which the
  // renderer is restricted to) and reset. Escaping happens first so a message
  // containing "<script>" is text, not markup — the server escapes when it embeds
  // the HTML, but this path is the one a user's own message travels.
  var FG = ["#000", "#800", "#080", "#880", "#008", "#808", "#088", "#ccc",
            "#888", "#f00", "#0f0", "#ff0", "#00f", "#f0f", "#0ff", "#fff"];

  // esc is used for anything that goes into markup as text. Window names come
  // from the wire (a channel name is user input), so they are escaped rather than
  // trusted.
  function esc(v) {
    return String(v).replace(/[&<>"]/g, function (c) {
      return c === "&" ? "&amp;" : c === "<" ? "&lt;" : c === ">" ? "&gt;" : "&quot;";
    });
  }

  function xterm256(n) {
    if (n < 16) return FG[n];
    if (n >= 232) {
      var v = 8 + (n - 232) * 10;
      return "rgb(" + v + "," + v + "," + v + ")";
    }
    n -= 16;
    var r = Math.floor(n / 36), g = Math.floor((n % 36) / 6), b = n % 6;
    function c(x) { return x === 0 ? 0 : 55 + x * 40; }
    return "rgb(" + c(r) + "," + c(g) + "," + c(b) + ")";
  }

  function ansiToHTML(s) {
    var out = "";
    var open = 0;
    s = s.replace(/[&<>]/g, function (c) {
      return c === "&" ? "&amp;" : c === "<" ? "&lt;" : "&gt;";
    });
    var re = /\x1b\[([0-9;]*)m/g;
    var last = 0, m;
    while ((m = re.exec(s)) !== null) {
      out += s.slice(last, m.index);
      last = re.lastIndex;
      var codes = m[1] === "" ? [0] : m[1].split(";").map(Number);
      for (var i = 0; i < codes.length; i++) {
        var c = codes[i];
        if (c === 0) {
          while (open > 0) { out += "</span>"; open--; }
        } else if (c === 1) {
          out += '<span style="font-weight:bold">'; open++;
        } else if (c === 3) {
          out += '<span style="font-style:italic">'; open++;
        } else if (c >= 30 && c <= 37) {
          out += '<span style="color:' + FG[c - 30] + '">'; open++;
        } else if (c >= 90 && c <= 97) {
          out += '<span style="color:' + FG[c - 90 + 8] + '">'; open++;
        } else if (c === 39) {
          // default foreground: close and reopen is not worth tracking precisely,
          // and a stray span is invisible. Close one.
          if (open > 0) { out += "</span>"; open--; }
        } else if (c === 38 || c === 48) {
          var isFg = c === 38;
          if (codes[i + 1] === 5) {
            var col = xterm256(codes[i + 2]);
            out += '<span style="' + (isFg ? "color:" : "background:") + col + '">';
            open++;
            i += 2;
          } else if (codes[i + 1] === 2) {
            var rgb = "rgb(" + codes[i + 2] + "," + codes[i + 3] + "," + codes[i + 4] + ")";
            out += '<span style="' + (isFg ? "color:" : "background:") + rgb + '">';
            open++;
            i += 4;
          }
        }
      }
    }
    out += s.slice(last);
    while (open > 0) { out += "</span>"; open--; }
    return out;
  }

  // -- state ---------------------------------------------------------------
  var ws = null;
  var retry = 0;
  var active = "status";
  // The render is a full screen. Redrawing it replaces the whole <pre>, which is
  // why there is no incremental line logic here to get wrong.
  var lastRender = "";
  // The last frame the server sent, for the keybinds: it already carries the window
  // list and which one is active, so no keybind needs to read the DOM.
  var lastFrame = null;

  function draw(f) {
    // Remembered so the keybinds can cycle windows without parsing the DOM: the
    // frame already carries the list and which one is active.
    lastFrame = f;
    // The model owns the buffer; this field only shows it. Setting .value on every
    // frame would fight the caret, so it is set only when it actually differs.
    if (inputEl && typeof f.input === "string" && inputEl.value !== f.input) {
      inputEl.value = f.input;
    }
    // Body, not render: the server sends the conversation without the TUI's frame,
    // because this page already has a sidebar, a header and an input row. Drawing
    // the framed render here is what produced a box inside a box.
    var bodyKey = Array.isArray(f.body) ? f.body.join("\n") : null;
    if (bodyKey !== null && bodyKey !== lastRender) {
      lastRender = bodyKey;
      termEl.innerHTML = bodyKey.split("\n").map(ansiToHTML).join("\n");
      // Keep the newest line visible unless the user scrolled up to read.
      var nearBottom = termEl.scrollTop + termEl.clientHeight >= termEl.scrollHeight - 40;
      if (nearBottom) termEl.scrollTop = termEl.scrollHeight;
    }

    if (Array.isArray(f.windows)) {
      active = f.active || active;
      var mt = document.getElementById("mobile-title");
      if (mt) mt.textContent = active;
      var unread = f.unread || {};
      var mentions = f.mentions || {};
      var html = "";
      for (var i = 0; i < f.windows.length; i++) {
        var name = f.windows[i];
        var cls = "win";
        if (name === active) cls += " active";
        // A mention is not just "something new": it is something for you, so it
        // gets its own class and the count is not drawn. Showing both would be
        // two competing signals for the same glance.
        if (mentions[name]) cls += " mention";
        var label = esc(name);
        // "status" is the bouncer's own window and is always present; a badge on
        // it would be noise, since every NOTICE lands there.
        var count = (f.nickcounts || {})[name] || 0;
        var badge = "";
        if (count > 0) badge = '<span class="count-nicks">' + count + "</span>";
        if (name !== "status") {
          if (mentions[name]) badge = '<span class="badge">@</span>';
          else if (unread[name] > 0) badge = '<span class="badge count">' + unread[name] + "</span>";
        }
        html += '<div class="' + cls + '" data-window="' + esc(name) + '">' +
          '<span class="name">' + label + "</span>" + badge + "</div>";
      }
      listEl.innerHTML = html;
    }

    // Link previews. They are cards under the header rather than inline: an inline
    // card would reflow the terminal rendering the server produced, which is the one
    // thing this client promises not to do.
    renderPreviews(f.previews);

    // The topic is a header, not a message: one line per window, replaced when it
    // changes. The server sends the string; the client decides where it goes.
    if (topicEl) {
      // Mode string and topic share one header line: "+nt | the topic". Modes
      // lead because they are short and survive truncation.
      var bits = [];
      if (f.modes && f.modes !== "+") bits.push(f.modes);
      if (f.topic) bits.push(f.topic);
      if (bits.length) {
        topicEl.textContent = bits.join(" | ");
        topicEl.hidden = false;
      } else {
        topicEl.textContent = "";
        topicEl.hidden = true;
      }
    }

    // The nick panel is the browser's version of the TUI's nick column: same data,
    // same order (ops first, then alphabetical), drawn where a browser expects it.
    if (nicksEl) {
      var list = f.nicks || [];
      if (!list.length) {
        nicksEl.innerHTML = "";
        nicksEl.hidden = true;
      } else {
        var nh = '<h2>' + list.length + " here</h2>";
        for (var j = 0; j < list.length; j++) {
          var n = list[j];
          var cls = n.prefix ? "nick op" : "nick";
          nh += '<div class="' + cls + '">' + esc(n.prefix || "") + esc(n.name) + "</div>";
        }
        nicksEl.innerHTML = nh;
        nicksEl.hidden = false;
      }
    }

    // The connected flag is the transport's report. A UI that shows green while
    // the bouncer is gone is the specific failure this project treats as a bug, so
    // the banner says it plainly instead of being silently optimistic.
    if (f.connected === false) {
      bannerEl.textContent = "disconnected — reconnecting";
      bannerEl.hidden = false;
    } else {
      bannerEl.hidden = true;
    }
    if (typeof f.status === "string" && f.status !== "") {
      statusEl.textContent = f.status;
    }
  }

  function connect() {
    var proto = location.protocol === "https:" ? "wss:" : "ws:";
    ws = new WebSocket(proto + "//" + location.host + "/ws");

    ws.onopen = function () {
      lastSize = "";
      // First frame is already the right size: report before the server renders,
      // not after, so the conversation does not appear as a small block and then
      // jump to fill the page.
      reportSize();
      retry = 0;
      bannerEl.hidden = true;
    };
    ws.onmessage = function (ev) {
      var f;
      try { f = JSON.parse(ev.data); } catch (e) { return; }
      if (f.type === "state") draw(f);
      else if (f.type === "results") renderResults(f);
    };
    ws.onclose = function () {
      bannerEl.textContent = "disconnected — reconnecting";
      bannerEl.hidden = false;
      // Backoff, capped: a browser hammering a restarting front end is worse than
      // a browser that waits two seconds.
      var wait = Math.min(1000 * Math.pow(1.6, retry), 8000);
      retry++;
      setTimeout(connect, wait);
    };
    ws.onerror = function () { try { ws.close(); } catch (e) {} };
  }


  // ---- theme and keybinds -------------------------------------------------
  //
  // Both are client-side settings, so both live in localStorage: they are
  // preferences about this browser, not state the bouncer should hold. Server-side
  // settings would mean a per-device session table for something that has no
  // business being shared between devices.
  var SETTINGS_KEY = "salotto.settings";
  var defaults = { theme: "dark", density: "normal" };
  var settings = defaults;

  function loadSettings() {
    try {
      var raw = localStorage.getItem(SETTINGS_KEY);
      if (raw) {
        var parsed = JSON.parse(raw);
        settings = Object.assign({}, defaults, parsed);
      }
    } catch (e) {
      // A corrupt or blocked localStorage must not break the client: the defaults
      // are a working UI, and preferences are not worth a broken page.
      settings = defaults;
    }
    applyTheme();
  }

  function saveSettings() {
    try {
      localStorage.setItem(SETTINGS_KEY, JSON.stringify(settings));
    } catch (e) {}
  }

  function applyTheme() {
    // The light theme is a class on <body>, because the palette is a set of CSS
    // variables and swapping them is the whole theme.
    document.body.classList.toggle("light", settings.theme === "light");
    document.body.classList.toggle("compact", settings.density === "compact");
  }

  function toggleTheme() {
    settings.theme = settings.theme === "light" ? "dark" : "light";
    applyTheme();
    saveSettings();
    setHint("theme: " + settings.theme);
  }

  var hintTimer = null;
  function setHint(text) {
    var el = document.getElementById("hint");
    if (!el) return;
    el.textContent = text;
    el.hidden = !text;
    if (hintTimer) clearTimeout(hintTimer);
    if (text) hintTimer = setTimeout(function () { el.hidden = true; }, 1800);
  }

  // Keybinds. Alt is the modifier rather than Ctrl because Ctrl+digit and Ctrl+M
  // are taken by the browser and the terminal respectively, and a bind that the
  // browser eats is worse than no bind.
  function onKey(e) {
    if (e.altKey && !e.ctrlKey && !e.metaKey) {
      switch (e.key) {
      case "t": e.preventDefault(); toggleTheme(); return;
      case "k": e.preventDefault(); focusSearch(); return;
      case "j": e.preventDefault(); cycleWindow(1); return;
      case "i": e.preventDefault(); cycleWindow(-1); return;
      case "c": e.preventDefault(); clearSearch(); return;
      }
    }
    if (e.key === "Escape") { clearSearch(); }
  }

  function focusSearch() {
    var q = document.getElementById("q");
    if (q) { q.focus(); q.select(); }
  }

  function clearSearch() {
    var q = document.getElementById("q");
    var res = document.getElementById("results");
    if (q) q.value = "";
    if (res) { res.hidden = true; res.textContent = ""; }
    var input = document.getElementById("line");
    if (input) input.focus();
  }

  function cycleWindow(delta) {
    if (!lastFrame || !lastFrame.windows || lastFrame.windows.length < 2) return;
    var ws = lastFrame.windows;
    var i = ws.indexOf(lastFrame.active);
    if (i < 0) return;
    var next = ws[(i + delta + ws.length) % ws.length];
    send({ type: "select", active: next });
  }


  // ---- viewport reporting -------------------------------------------------
  //
  // The server renders a fixed width and a fixed number of lines, so it has to be
  // told the size. Without this the page asks for the terminal driver's default and
  // every phone gets a window that wraps.
  var lastSize = "";

  function reportSize() {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    // Measure the terminal element, not the window: the sidebar and the input take
    // space, and asking for the window width produces lines that wrap anyway.
    var rect = termEl.getBoundingClientRect();
    var charW = 8.4;   // ui-monospace at 14px; close enough, and measured below
    var charH = 18.9;  // 14px * 1.35 line-height
    var probe = document.createElement("span");
    probe.textContent = "0000000000";
    probe.style.font = getComputedStyle(termEl).font;
    probe.style.position = "absolute";
    probe.style.visibility = "hidden";
    document.body.appendChild(probe);
    var w = probe.getBoundingClientRect().width / 10;
    document.body.removeChild(probe);
    if (w > 0) charW = w;
    var cols = Math.max(20, Math.floor((rect.width - 24) / charW));
    var rows = Math.max(6, Math.floor((rect.height - 8) / charH));
    var key = cols + "x" + rows;
    if (key === lastSize) return;
    lastSize = key;
    ws.send(JSON.stringify({ type: "resize", cols: cols, rows: rows }));
  }


  // ---- mobile drawers -----------------------------------------------------
  //
  // On a phone the sidebars become drawers. They cannot simply be hidden: a phone
  // with no window list cannot change channel, which is the one thing a chat client
  // must always allow.
  function bindDrawer(buttonId, bodyClass) {
    var btn = document.getElementById(buttonId);
    if (!btn) return;
    btn.addEventListener("click", function () {
      var on = document.body.classList.toggle(bodyClass);
      btn.classList.toggle("active", on);
    });
  }

  function closeDrawers() {
    document.body.classList.remove("show-windows", "show-nicks");
    var a = document.getElementById("show-windows");
    var b = document.getElementById("show-nicks");
    if (a) a.classList.remove("active");
    if (b) b.classList.remove("active");
  }

  function renderPreviews(list) {
    if (!previewsEl) return;
    previewsEl.textContent = "";
    if (!list || !list.length) {
      previewsEl.hidden = true;
      return;
    }
    var shown = 0;
    for (var i = 0; i < list.length; i++) {
      var p = list[i];
      if (!p || p.kind !== "link" || !p.title) continue;
      var a = document.createElement("a");
      a.className = "card";
      a.href = p.url;
      // noreferrer keeps the reader's origin off the third party's logs, and
      // noopener stops the opened page from touching this window. Both are the same
      // promise this client makes by fetching previews server-side at all.
      a.rel = "noreferrer noopener";
      a.target = "_blank";
      var site = document.createElement("span");
      site.className = "site";
      site.textContent = p.site || "";
      var title = document.createElement("span");
      title.className = "title";
      // textContent, never innerHTML: a title is a stranger's string.
      title.textContent = p.title;
      a.appendChild(site);
      a.appendChild(title);
      previewsEl.appendChild(a);
      shown++;
    }
    previewsEl.hidden = shown === 0;
  }

  function send(text) {
    if (!text) return;
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: "send", text: text }));
    } else {
      // Degraded path: the plain form POST still works with no JavaScript and no
      // socket, so this is a fallback rather than an error page.
      formEl.submit();
    }
  }

  // Every keystroke goes to the model. It is the same path a TUI keypress takes, so
  // the buffer, history and completion cannot diverge between the two front ends.
  //
  // The caret is put back afterwards because the server re-renders the input line:
  // a client that only appends its own text would be a second buffer.
  if (inputEl) {
    inputEl.addEventListener("keydown", function (e) {
      if (e.key === "Enter") {
        e.preventDefault();
        send({ type: "key", key: "enter" });
        return;
      }
      if (e.key === "ArrowUp") {
        e.preventDefault();
        send({ type: "key", key: "up" });
        return;
      }
      if (e.key === "ArrowDown") {
        e.preventDefault();
        send({ type: "key", key: "down" });
        return;
      }
      if (e.key === "Backspace") {
        e.preventDefault();
        send({ type: "key", key: "backspace" });
        return;
      }
      if (e.key === "Escape") {
        e.preventDefault();
        send({ type: "key", key: "esc" });
        return;
      }
      // Printable characters only: modifiers and control keys are the browser's.
      if (e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
        e.preventDefault();
        send({ type: "key", runes: e.key });
      }
    });

    // Paste arrives as one event, not a keystroke per character.
    inputEl.addEventListener("paste", function (e) {
      var text = (e.clipboardData || window.clipboardData).getData("text");
      if (!text) return;
      e.preventDefault();
      send({ type: "key", runes: text });
    });
  }

  formEl.addEventListener("submit", function (e) {
    e.preventDefault();
    // The input field is a VIEW of the model's buffer, not the buffer itself. Send
    // the Enter and let the model decide what the line means: that is what gives the
    // browser the same command mapping, the same /commands and the same local echo
    // as the TUI, instead of a second implementation that drifts.
    send({ type: "key", key: "enter" });
    inputEl.value = "";
  });

  listEl.addEventListener("click", function (e) {
    // closest(), not e.target: the row is built from spans, so clicking the window
    // NAME lands on the inner <span> which has no data-window. Reading the target
    // directly meant only the empty space beside the text worked, which is exactly
    // as broken as it sounds and invisible to every test that dispatches the click
    // on the row itself.
    var row = e.target && e.target.closest ? e.target.closest("[data-window]") : null;
    if (!row) return;
    var name = row.getAttribute("data-window");
    if (!name) return;
    closeDrawers();
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: "select", active: name }));
    } else {
      // No socket: a form POST with only a window switches it server-side.
      var f = document.createElement("form");
      f.method = "POST";
      f.action = "/";
      var i = document.createElement("input");
      i.type = "hidden"; i.name = "window"; i.value = name;
      f.appendChild(i);
      document.body.appendChild(f);
      f.submit();
    }
  });

  // Alt-binds for the things the TUI has keys for, plus the theme. Registered once.
  document.addEventListener("keydown", onKey);

  // Ctrl-N / Ctrl-P cycle windows, matching the TUI, because a browser user who
  // learned the keys in the terminal should not have to relearn them here.
  document.addEventListener("keydown", function (e) {
    if (!e.ctrlKey) return;
    if (e.key !== "n" && e.key !== "p") return;
    e.preventDefault();
    var wins = listEl.querySelectorAll(".win");
    if (wins.length < 2) return;
    var idx = 0;
    for (var i = 0; i < wins.length; i++) {
      if (wins[i].getAttribute("data-window") === active) { idx = i; break; }
    }
    idx = (idx + (e.key === "n" ? 1 : -1) + wins.length) % wins.length;
    var name = wins[idx].getAttribute("data-window");
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: "select", active: name }));
    }
  });

  // -- search ---------------------------------------------------------------
  // The query goes to the server, which owns the scrollback. Searching in the
  // browser would search only what has been rendered into the page, which is a
  // different and much smaller set than what the bouncer holds.
  function renderResults(f) {
    if (!resultsEl) return;
    var hits = f.results || [];
    if (!hits.length) {
      resultsEl.hidden = true;
      resultsEl.innerHTML = "";
      return;
    }
    var html = '<div class="head">' + hits.length + ' match' +
      (hits.length === 1 ? "" : "es") + ' for ' + esc(f.query) + "</div>";
    for (var i = 0; i < hits.length; i++) {
      var h = hits[i];
      html += '<div class="hit" data-window="' + esc(h.window) + '">' +
        '<span class="hl">' + esc(h.window) + "</span> " +
        '<span class="hn">&lt;' + esc(h.nick || "") + "&gt;</span> " +
        esc(h.text) + "</div>";
    }
    resultsEl.innerHTML = html;
    resultsEl.hidden = false;
  }

  if (searchEl) {
    searchEl.addEventListener("keydown", function (e) {
      if (e.key === "Escape") {
        searchEl.value = "";
        renderResults({ results: [] });
        return;
      }
      if (e.key !== "Enter") return;
      var q = searchEl.value.trim();
      if (!q) { renderResults({ results: [] }); return; }
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: "search", query: q }));
      }
    });
  }

  if (resultsEl) {
    // Clicking a hit jumps to the window it came from, which is the part The
    // Lounge's search cannot do and the reason ours is worth having.
    resultsEl.addEventListener("click", function (e) {
      var el = e.target;
      while (el && el !== resultsEl && !el.getAttribute("data-window")) el = el.parentNode;
      if (!el || el === resultsEl) return;
      var name = el.getAttribute("data-window");
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: "select", active: name }));
      }
    });
  }

  connect();
  bindDrawer("show-windows", "show-windows");
  bindDrawer("show-nicks", "show-nicks");
  // Tapping the conversation closes a drawer. Otherwise the only way out is the
  // button that opened it, which is a trap on a small screen.
  if (termEl) termEl.addEventListener("click", closeDrawers);

  var themeBtn = document.getElementById("theme");
  if (themeBtn) themeBtn.addEventListener("click", toggleTheme);

  // A resize is not a keypress and not a click, so it needs its own listener. Both
  // directions are covered: the window (a phone rotating) and the element (the
  // sidebar collapsing at a breakpoint).
  if (window.ResizeObserver && termEl) {
    var ro = new ResizeObserver(function () { reportSize(); });
    ro.observe(termEl);
  }
  window.addEventListener("resize", reportSize);
  window.addEventListener("orientationchange", function () { setTimeout(reportSize, 250); });

  loadSettings();

})();