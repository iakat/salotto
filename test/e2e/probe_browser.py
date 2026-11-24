"""End-to-end checks for the browser-only parts of the web front end.

These are the features that exist in the page rather than in the program: text selection, the
copy path, and link detection. They cannot be tested from the terminal side, and they cannot be
tested by reading the model, because the page is the thing that implements them.

Run from inside a browser session:

    from probe_browser import run
    run()      # returns a list of (name, ok, detail)

The checks dispatch real Chrome DevTools Protocol input events, not synthetic DOM events: a
dispatched mousemove does not move xterm's selection, because xterm reads coordinates from the
event target's own geometry and an untrusted event has no trusted coordinates. A check that
passes with synthetic events is testing the test.
"""
import json
import time


def run(cdp, js, host):
    """Run every check against a live host. cdp(method, **params) and js(expr) are injected by
    the caller so this file has no browser driver of its own - the one that exists is the one
    the operator is already using."""
    results = []

    def check(name, ok, detail=""):
        results.append((name, bool(ok), detail))
        return ok

    def mouse(kind, x, y, buttons=1):
        cdp("Input.dispatchMouseEvent", type=kind, x=x, y=y, button="left",
            buttons=buttons, clickCount=1)

    def navigate(url):
        cdp("Page.navigate", url=url)

    navigate("https://%s/?t=%d" % (host, int(time.time())))
    deadline = time.time() + 60
    while time.time() < deadline:
        if js("typeof window.salottoTerm") == "object":
            break
        time.sleep(1)
    check("the page exposes its terminal", js("typeof window.salottoTerm") == "object")

    check("there is a select toggle", js("!!document.getElementById('selectmode')") is True)

    # Selection needs the program's mouse reporting off, or every drag is a click.
    js("document.getElementById('selectmode').click()")
    time.sleep(0.4)
    protocol = js("window.salottoTerm._core.coreMouseService.activeProtocol")
    check("select mode turns mouse reporting off", protocol == "NONE", "protocol=%s" % protocol)

    geom = json.loads(js("""(function(){
      const t = window.salottoTerm;
      const r = document.querySelector('#term .xterm-screen').getBoundingClientRect();
      return JSON.stringify({x:r.left, y:r.top, cw:r.width/t.cols, ch:r.height/t.rows});
    })()"""))

    rows = json.loads(js("""JSON.stringify(Array.from(document.querySelectorAll('.xterm-rows > div'))
      .map(r => r.textContent.replace(/\\u00a0/g,' ')))"""))
    target = next((i for i, l in enumerate(rows) if len(l.strip(" │")) > 20), 2)
    y = int(geom["y"] + (target + 0.5) * geom["ch"])
    x0 = int(geom["x"] + geom["cw"] * 22)
    x1 = int(geom["x"] + geom["cw"] * min(100, max(30, len(rows[target]) - 2)))

    mouse("mousePressed", x0, y)
    for x in range(x0, x1, 20):
        mouse("mouseMoved", x, y)
    mouse("mouseReleased", x1, y, buttons=0)
    time.sleep(0.6)

    sel = js("window.salottoTerm.getSelection()") or ""
    check("a drag selects text", bool(sel.strip()), repr(sel[:40]))
    check("the selection is real text, not an empty string", len(sel.strip()) > 3,
          "%d chars" % len(sel.strip()))

    js("document.querySelector('#term .xterm-helper-textarea').dispatchEvent("
       "new KeyboardEvent('keydown', {key:'C', ctrlKey:true, shiftKey:true,"
       "bubbles:true, cancelable:true}))")
    check("Ctrl-Shift-C does not clear the selection",
          bool(js("window.salottoTerm.getSelection()")))

    # Links: put one in the buffer, then ask the providers the page installed.
    js("window.salottoTerm.paste('probe https://example.com/x and www.example.org/y')")
    time.sleep(0.2)
    js("window.salottoTerm.input('\\r', true)")
    time.sleep(2.5)
    found = json.loads(js("""(function(){
      var t = window.salottoTerm, out = [];
      var svc = t._core._linkProviderService;
      var provs = svc.linkProviders || [];
      for (var i = 0; i < provs.length; i++) {
        for (var y = 1; y <= t.rows; y++) {
          (function(y){
            provs[i].provideLinks(y, function(links){
              if (links) links.forEach(function(l){ out.push(l.text); });
            });
          })(y);
        }
      }
      return JSON.stringify(out);
    })()""") or "[]")
    check("a URL in the buffer is detected as a link",
          any("example.com" in f for f in found), str(found[:3]))
    check("a bare www. host is detected too",
          any("example.org" in f for f in found), str(found[:3]))

    js("document.getElementById('selectmode').click()")
    time.sleep(0.4)
    protocol = js("window.salottoTerm._core.coreMouseService.activeProtocol")
    check("turning select mode off restores mouse reporting",
          protocol not in (None, "NONE"), "protocol=%s" % protocol)

    return results
