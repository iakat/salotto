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

  function draw(f) {
    if (typeof f.render === "string" && f.render !== lastRender) {
      lastRender = f.render;
      termEl.innerHTML = ansiToHTML(f.render);
      // Keep the newest line visible unless the user scrolled up to read.
      var nearBottom = termEl.scrollTop + termEl.clientHeight >= termEl.scrollHeight - 40;
      if (nearBottom) termEl.scrollTop = termEl.scrollHeight;
    }

    if (Array.isArray(f.windows)) {
      active = f.active || active;
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

  formEl.addEventListener("submit", function (e) {
    e.preventDefault();
    var text = inputEl.value;
    inputEl.value = "";
    send(text);
  });

  listEl.addEventListener("click", function (e) {
    var t = e.target;
    if (!t || !t.getAttribute) return;
    var name = t.getAttribute("data-window");
    if (!name) return;
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
})();
