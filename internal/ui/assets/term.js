(function () {
  "use strict";

  // The browser half of the terminal front end: an emulator, a socket, and nothing
  // else. Every keystroke goes to the PTY as bytes; every byte the program writes goes
  // to the emulator. There is deliberately no logic here that knows what a channel, a
  // nick or an input line is - the program on the other end of the socket owns all of
  // that, because it drew them.
  //
  // This is the arrangement ttyd and GoTTY use, and it is the arrangement that cannot
  // drift from the SSH and terminal front ends: they run the same program. A JSON frame
  // protocol with parallel fields for windows and nicks was a second renderer, and it
  // produced a doubled nicklist and a doubled input line before it was deleted.

  var termEl = document.getElementById("term");
  var selectBtn = document.getElementById("selectmode");
  var banner = document.getElementById("banner");

  var term = new Terminal({
    // The program sends 256-colour SGR, which is what lipgloss is pinned to; truecolor
    // is set anyway because the emulator handles both and TERM advertises it.
    convertEol: false,
    cursorBlink: true,
    // Scrollback is the emulator's, not the program's: a browser user scrolls with the
    // wheel, and making the program implement that would be a terminal inside a
    // terminal.
    scrollback: 5000,
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
    fontSize: 14,
    theme: {
      background: "#12121a",
      foreground: "#e6e6f0",
      cursor: "#e6e6f0",
      selectionBackground: "#3a3a55"
    },
    // Let the program receive Alt as a prefix (bubbletea binds alt keys), and treat
    // Option as Meta on macOS so those binds work there too.
    macOptionIsMeta: true,
    allowProposedApi: true
  });

  function fit() {
    // xterm has no built-in fit: measure the cell and set the size. The server learns
    // it via the resize message, which becomes TIOCSWINSZ on the PTY, which is how
    // bubbletea gets a WindowSizeMsg. No custom resize protocol.
    var rect = termEl.getBoundingClientRect();
    var dims = term._core._renderService.dimensions;
    var cw = dims.css.cell.width, ch = dims.css.cell.height;
    if (!cw || !ch) return;
    var cols = Math.max(20, Math.floor(rect.width / cw));
    var rows = Math.max(5, Math.floor(rect.height / ch));
    if (cols === term.cols && rows === term.rows) return;
    term.resize(cols, rows);
    send({ type: "resize", cols: cols, rows: rows });
  }

  term.open(termEl);

  // Exposed for the end-to-end probe and for anyone debugging a live session: the browser
  // half is otherwise closed over, and a terminal you cannot inspect is a terminal whose
  // selection and link behaviour can only be tested by hand.
  window.salottoTerm = term;

  // Selection mode. The program has asked for mouse reporting because it needs clicks for the
  // window list, and a terminal that reports the mouse does not select: every press belongs to
  // the program. Shift+drag still selects, the way it does in every terminal - but a phone has
  // no Shift, and "you cannot copy this on mobile" is not a thing to leave as a known
  // limitation when one button fixes it.
  //
  // xterm has no runtime switch for mouse reporting, but it does have the escape hatch: hold
  // Shift and it selects instead of reporting. So this mode makes every mouse event look
  // shifted, which is the same code path a real terminal takes and cannot drift from it.
  var selecting = false;

  function setSelectMode(on) {
    selecting = on;
    if (selectBtn) {
      selectBtn.textContent = on ? "\u2713 text" : "\u2197 select";
      selectBtn.title = on ? "back to clicking" : "select text to copy";
      selectBtn.classList.toggle("on", on);
    }
    if (!on) term.clearSelection();
  }

  ["mousedown", "mousemove", "mouseup", "click", "dblclick"].forEach(function (name) {
    // Capture before xterm's own listener so the patched event is the one it acts on.
    termEl.addEventListener(name, function (ev) {
      if (!selecting) return;
      if (!("shiftKey" in ev)) return;
      try {
        Object.defineProperty(ev, "shiftKey", { value: true, configurable: true });
      } catch (e) {}
    }, true);
  });

  if (selectBtn) {
    selectBtn.addEventListener("click", function () { setSelectMode(!selecting); });
  }

  // Selection. The program has asked for mouse reporting (it needs clicks for the window
  // list), and a terminal that reports the mouse does not select text: every press is the
  // program's. xterm keeps the escape hatch real terminals use - hold Shift - and this page
  // adds the two things a browser user expects on top of it: a copy that works, and a way to
  // select without knowing about Shift.
  //
  // Double-click selects a word and triple-click a line, which xterm implements itself once
  // the selection is allowed; drag under Shift selects a range.
  term.attachCustomKeyEventHandler(function (ev) {
    // Cmd+C on macOS, Ctrl+Shift+C elsewhere. Plain Ctrl-C is SIGINT and belongs to the
    // program: taking it for copy would kill the client.
    var copyKey = (ev.metaKey && !ev.ctrlKey) || (ev.ctrlKey && ev.shiftKey);
    if (ev.type === "keydown" && copyKey && (ev.key === "c" || ev.key === "C")) {
      var sel = term.getSelection();
      if (sel) {
        copyText(sel);
        term.clearSelection();
        return false;
      }
    }
    return true;
  });

  function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).catch(fallbackCopy.bind(null, text));
      return;
    }
    fallbackCopy(text);
  }

  function fallbackCopy(text) {
    // clipboard.writeText needs a secure context and permission; a plain https page usually
    // has it, a self-signed one sometimes does not. A hidden textarea and execCommand still
    // works there, and a copy button that silently does nothing is worse than a deprecated
    // API.
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand("copy"); } catch (e) {}
    document.body.removeChild(ta);
  }

  // Links. The program prints URLs in chat; xterm does not know they are links. This is the
  // same hover-with-underline, click-to-open behaviour every browser terminal has, and it is
  // read-only: it never touches the socket, so a link can never be mistaken for input.
  term.registerLinkProvider({
    provideLinks: function (y, callback) {
      var line = term.buffer.active.getLine(y - 1);
      if (!line) { callback(undefined); return; }
      var text = line.translateToString(true);
      var re = /\b(?:https?:\/\/|www\.)[^\s<>"']+/gi;
      var links = [];
      var m;
      while ((m = re.exec(text)) !== null) {
        var raw = m[0].replace(/[.,;:!?)\]]+$/, "");
        var start = m.index + 1;
        var end = start + raw.length;
        // A link wraps: xterm wants the range clipped to this row, and the hover underline
        // stops at the edge the way it should.
        if (start > term.cols) continue;
        links.push({
          range: { start: { x: start, y: y }, end: { x: Math.min(end, term.cols + 1), y: y } },
          text: raw,
          activate: function (ev, uri) {
            var url = /^https?:\/\//i.test(uri) ? uri : "https://" + uri;
            // Cmd/Ctrl-click or a plain click both open, because on a phone there is no
            // modifier to hold.
            window.open(url, "_blank", "noopener,noreferrer");
          }
        });
      }
      callback(links.length ? links : undefined);
    }
  });

  var ws = null;
  var backoff = 300;

  function send(msg) {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(msg));
      return true;
    }
    return false;
  }

  function setBanner(text) {
    if (!banner) return;
    banner.textContent = text || "";
    banner.className = text ? "on" : "";
  }

  function connect() {
    var proto = location.protocol === "https:" ? "wss:" : "ws:";
    var url = proto + "//" + location.host + "/ws?cols=" + term.cols + "&rows=" + term.rows;
    ws = new WebSocket(url);
    ws.binaryType = "arraybuffer";

    ws.onopen = function () {
      backoff = 300;
      setBanner("");
      // The program may have started before we knew the real size; say it again once
      // the socket is up.
      send({ type: "resize", cols: term.cols, rows: term.rows });
      term.focus();
    };

    ws.onmessage = function (ev) {
      var msg;
      try { msg = JSON.parse(ev.data); } catch (e) { return; }
      switch (msg.type) {
      case "output":
        // base64 from Go's []byte encoding, decoded straight into the emulator: the
        // bytes are the rendering, escape sequences and all.
        term.write(base64ToBytes(msg.data), function () {});
        break;
      case "exit":
        term.write("\r\n\x1b[31m[session ended]\x1b[0m\r\n");
        break;
      case "error":
        setBanner(msg.text || "session error");
        break;
      }
    };

    ws.onclose = function () {
      // The program exited: there is nothing to reconnect to, and pretending otherwise
      // shows a dead terminal that accepts typing. Say so and stop.
      setBanner("disconnected");
    };

    ws.onerror = function () {
      setBanner("connection lost, retrying");
      try { ws.close(); } catch (e) {}
    };
  }

  function base64ToBytes(b64) {
    if (!b64) return new Uint8Array(0);
    var bin = atob(b64);
    var out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }

  // Keystrokes are the terminal's input. xterm produces exactly what a real terminal
  // would for a key, including arrows, function keys, Alt combinations and bracketed
  // paste, so the program needs no key mapping of its own.
  term.onData(function (data) {
    send({ type: "input", data: btoa(data) });
  });

  // Binary input (bracketed paste, some IMEs) is separate from text input.
  term.onBinary(function (data) {
    var bytes = new Uint8Array(data.length);
    for (var i = 0; i < data.length; i++) bytes[i] = data.charCodeAt(i) & 0xff;
    send({ type: "input", data: bytesToBase64(bytes) });
  });

  function bytesToBase64(bytes) {
    var bin = "";
    for (var i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
    return btoa(bin);
  }

  // Clicks and drags are input too: xterm encodes them as mouse escape sequences when
  // the program has asked for mouse reporting, which bubbletea does. That is why the
  // sidebar is clickable without a single line of click handling here.
  term.onResize(function () { fit(); });

  window.addEventListener("resize", fit);
  window.addEventListener("orientationchange", function () { setTimeout(fit, 200); });
  if (window.visualViewport) {
    // The on-screen keyboard changes the visual viewport without a window resize on
    // iOS, and a terminal that does not react ends up with the input line behind the
    // keyboard.
    window.visualViewport.addEventListener("resize", fit);
  }

  connect();
  setTimeout(fit, 0);
  setTimeout(fit, 250);
})();
