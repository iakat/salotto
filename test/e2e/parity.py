#!/usr/bin/env python3
"""Feature-parity harness: drive ZNC and salotto identically, diff what comes back.

The question is not "does salotto do what I decided it should do", it is "does
salotto do what ZNC does". So every claim about ZNC has to be settled by asking
ZNC: the same scenario runs against both bouncers and the observable outcome is
compared.

The one asymmetry that is real, not a bug: bouncer commands are a *dialect*. ZNC
takes them as `PRIVMSG *status :DisableChan #chan`; salotto takes the bare verb.
cmd() hides that difference so the scenarios stay identical, and responses are
normalised to their text so the comparison is about behaviour, not about which
prefix carried it.

Usage:
    python3 parity.py                       # in-cluster defaults
    python3 parity.py --json /tmp/report.json
Exit code 1 when a behaviour differs, so it can gate CI.
"""

from __future__ import annotations

import argparse
import json
import re
import socket
import sys
import time

# ---------------------------------------------------------------- transport


class Bouncer:
    """One bouncer connection: send lines, collect what comes back."""

    def __init__(self, host: str, port: int, nick: str, dialect: str, password: str | None = None):
        self.dialect = dialect  # "znc" | "salotto"
        self.nick = nick
        self.sock = socket.create_connection((host, port), timeout=15)
        self.sock.settimeout(0.3)
        self.buf = b""
        self.pending: list[str] = []
        if password:
            self.send("PASS " + password)
        self.send("CAP LS 302")
        self.send("NICK " + nick)
        self.send("USER %s 0 * :%s client" % (nick, nick))
        self.send("CAP END")

    # -- raw
    def send(self, line: str) -> None:
        self.sock.sendall(line.encode() + b"\r\n")

    def _pump(self) -> None:
        try:
            chunk = self.sock.recv(65536)
        except socket.timeout:
            return
        except OSError:
            return
        if not chunk:
            return
        self.buf += chunk
        while b"\n" in self.buf:
            raw, self.buf = self.buf.split(b"\n", 1)
            self.pending.append(raw.rstrip(b"\r").decode("utf-8", "replace"))

    def drain(self, seconds: float = 1.0) -> list[str]:
        end = time.time() + seconds
        while time.time() < end:
            self._pump()
        out, self.pending = self.pending, []
        return out

    def ask(self, line: str, seconds: float = 1.2) -> list[str]:
        self.send(line)
        return self.drain(seconds)

    # -- dialect
    def cmd(self, name: str, *args: str, seconds: float = 1.5) -> list[str]:
        """Send a bouncer command in this bouncer's dialect."""
        tail = " ".join([name] + [a for a in args])
        if self.dialect == "znc":
            return self.ask("PRIVMSG *status :" + tail, seconds)
        return self.ask(tail, seconds)

    def wait_for(self, needle: str, seconds: float = 10.0) -> bool:
        end = time.time() + seconds
        while time.time() < end:
            self._pump()
            for i, l in enumerate(self.pending):
                if needle in l:
                    del self.pending[: i + 1]
                    return True
        return False

    def close(self) -> None:
        try:
            self.send("QUIT :parity harness")
        except OSError:
            pass
        try:
            self.sock.close()
        except OSError:
            pass


# ---------------------------------------------------------------- scrubbing

# Identity, version, timestamps and sequence numbers are legitimately different.
# None of them is a parity question.
SCRUBS = [
    (re.compile(r":\S*salotto\S*"), ":bouncer"),
    (re.compile(r":irc\.znc\.in"), ":bouncer"),
    (re.compile(r"salotto-0\.1"), "VERSION"),
    (re.compile(r"ZNC \d+\.\d+\.\d+[^ ]*"), "VERSION"),
    (re.compile(r"\b20\d\d-\d\d-\d\dT[\d:.]+Z?\b"), "TIME"),
    (re.compile(r"@time=\S+"), "@time=TIME"),
    (re.compile(r"GAP \S+ \S+ \d+ \d+"), "GAP"),
]


def scrub(line: str) -> str:
    for pat, repl in SCRUBS:
        line = pat.sub(repl, line)
    return line.strip()


def numeric(line: str) -> str | None:
    m = re.match(r"^(?:@\S+ )?:\S+ (\d{3}) ", line)
    return m.group(1) if m else None


def texts(lines: list[str]) -> list[str]:
    """The human-readable payload of each line: trailing param, or the rest."""
    out = []
    for line in lines:
        if " PRIVMSG " in line or " NOTICE " in line:
            i = line.rfind(" :")
            if i >= 0:
                out.append(scrub(line[i + 2:]))
            continue
        m = re.match(r"^(?:@\S+ )?:\S+ (\d{3}) \S+ (.*)$", line)
        if m:
            rest = m.group(2)
            i = rest.find(" :")
            out.append(scrub(rest[i + 2:] if i >= 0 else rest))
    return out


def says(lines: list[str], needle: str) -> bool:
    """Does any payload of lines contain needle (case-insensitive)?"""
    needle = needle.lower()
    joined = " ".join(texts(lines)).lower()
    return needle in joined


# ---------------------------------------------------------------- scenarios


def case_registration(b: Bouncer) -> dict:
    """The handshake: 001, 005, and which MOTD policy answered."""
    lines = b.drain(4.0)
    codes = [c for c in (numeric(l) for l in lines) if c]
    motd = [c for c in codes if c in ("375", "372", "376", "422")]
    return {
        "welcome": "001" in codes,
        "isupport": "005" in codes,
        # 375/372/376 = a MOTD block, 422 = none.
        "motd": "block" if motd[:1] == ["375"] else ("missing" if motd[:1] == ["422"] else "other"),
    }


def case_join_echo(b: Bouncer, channel: str) -> dict:
    b.ask("JOIN " + channel, 2.0)
    echo = b.ask("PRIVMSG %s :parity-echo" % channel, 1.5)
    return {"joined": True, "echoed": says(echo, "parity-echo")}


def case_playback(b: Bouncer, channel: str, marker: str) -> dict:
    """A line buffered while we were detached must replay on PlayBuffer."""
    b.cmd("Detach", channel, seconds=1.2)
    b.send("PRIVMSG %s :%s" % (channel, marker))
    b.drain(1.2)
    play = b.cmd("PlayBuffer", channel, seconds=2.5)
    b.cmd("Attach", channel, seconds=1.2)
    return {"played": says(play, marker)}


def case_listchans(b: Bouncer, channel: str) -> dict:
    got = b.cmd("ListChans", seconds=2.0)
    text = " ".join(texts(got)).lower()
    return {
        "answered": bool(got),
        "names_channel": channel.lower() in text,
    }


def case_setbuffer(b: Bouncer, channel: str) -> dict:
    """SetBuffer answers with ZNC's exact sentence and a real size.

    The wording is the parity question: "Size of every buffer was set to N
    lines" is what a client or script greps for. That the size took effect is
    checked by the winring test on our side, because ZNC exposes no way to read
    a buffer's capacity back over the wire.
    """
    got = b.cmd("SetBuffer", channel, "50", seconds=2.0)
    return {
        "answered": bool(got),
        "says_set": says(got, "size of every buffer was set to 50"),
    }


def case_disable(b: Bouncer, channel: str) -> dict:
    """DisableChan must answer and EnableChan must answer; neither may error."""
    dis = b.cmd("DisableChan", channel, seconds=1.5)
    en = b.cmd("EnableChan", channel, seconds=1.5)
    return {
        "disable_answered": bool(dis),
        "enable_answered": bool(en),
        "mentions_channel": says(dis, channel),
    }


def case_clear(b: Bouncer, channel: str, marker: str) -> dict:
    """ClearBuffer then PlayBuffer: the cleared line must not come back."""
    b.send("PRIVMSG %s :%s" % (channel, marker))
    b.drain(1.0)
    printed = b.cmd("ClearBuffer", channel, seconds=1.5)
    play = b.cmd("PlayBuffer", channel, seconds=2.5)
    return {
        "clear_answered": bool(printed),
        "cleared": not says(play, marker),
    }


def case_fingerprints(b: Bouncer) -> dict:
    fp = ("E5:BE:6C:C0:E6:F1:06:3A:69:04:51:9F:E6:5C:D7:46:"
          "D8:AF:9D:E5:C8:D2:C6:20:17:E3:1F:A3:A7:3E:3C:EE")
    empty = b.cmd("ListTrustedServerFingerprints", seconds=1.5)
    bad = b.cmd("AddTrustedServerFingerprint", "not-a-fingerprint", seconds=1.5)
    add = b.cmd("AddTrustedServerFingerprint", fp, seconds=1.5)
    listed = b.cmd("ListTrustedServerFingerprints", seconds=1.5)
    b.cmd("DelTrustedServerFingerprint", fp, seconds=1.5)
    after = b.cmd("ListTrustedServerFingerprints", seconds=1.5)
    return {
        "empty_says_none": says(empty, "no fingerprints added"),
        "bad_rejected": says(bad, "not a sha-256 fingerprint"),
        "add_answered": bool(add),
        "listed": says(listed, fp.lower()),
        "empty_after_del": says(after, "no fingerprints added"),
    }


def _rows(lines: list[str]) -> list[list[str]]:
    """Data rows of a ZNC grid table: the '|' lines minus the header row."""
    rows = []
    for line in texts(lines):
        if not line.startswith("|"):
            continue
        cells = [x.strip() for x in line.strip("|").split("|")]
        rows.append(cells)
    return rows[1:] if rows else []


def case_listnetworks(b: Bouncer) -> dict:
    got = b.cmd("ListNetworks", seconds=2.0)
    rows = _rows(got)
    # The network name and the "is it on IRC" answer are the parity question;
    # the address and our nick mask legitimately differ between fixtures.
    return {
        "answered": bool(rows),
        "on_irc": any(r[-4].lower() == "yes" for r in rows) if rows else False,
        "name_listed": any(r and r[0] for r in rows),
    }


def case_listservers(b: Bouncer) -> dict:
    got = b.cmd("ListServers", seconds=2.0)
    rows = _rows(got)
    text = " ".join(texts(got)).lower()
    return {
        "answered": bool(rows),
        # ZNC marks the current server with a trailing '*'.
        "marks_current": any(r and r[0].endswith("*") for r in rows),
        # A password is shown as ****** or as nothing, never in clear.
        "hides_password": "******" in text or "password" in text,
    }


def case_listclients(b: Bouncer) -> dict:
    got = b.cmd("ListClients", seconds=2.0)
    rows = _rows(got)
    return {
        "answered": bool(rows),
        # Exactly one client is attached: this harness connection.
        "one_client": len(rows) == 1,
    }


def case_traffic(b: Bouncer) -> dict:
    got = b.cmd("Traffic", seconds=2.0)
    text = " ".join(texts(got)).lower()
    return {
        "answered": "in" in text and "out" in text,
        "has_total": "<total>" in text,
    }


def case_connect_disconnect(b: Bouncer) -> dict:
    """Disconnect then Connect: both must answer with ZNC's wording."""
    dis = b.cmd("Disconnect", seconds=2.0)
    # ZNC stops the upstream then reconnects; give it a moment either way.
    con = b.cmd("Connect", seconds=2.5)
    return {
        "disconnect_answered": says(dis, "disconnected"),
        "connect_answered": says(con, "connect"),
    }


def run(host: str, port: int, nick: str, dialect: str, password: str | None, channel: str) -> dict:
    b = Bouncer(host, port, nick, dialect, password)
    out: dict[str, dict] = {}
    try:
        out["registration"] = case_registration(b)
        out["join"] = case_join_echo(b, channel)
        out["playback"] = case_playback(b, channel, "parity-play")
        out["listchans"] = case_listchans(b, channel)
        out["setbuffer"] = case_setbuffer(b, channel)
        out["disable"] = case_disable(b, channel)
        out["clear"] = case_clear(b, channel, "parity-clear")
        out["fingerprints"] = case_fingerprints(b)
        out["listnetworks"] = case_listnetworks(b)
        out["listservers"] = case_listservers(b)
        out["listclients"] = case_listclients(b)
        out["traffic"] = case_traffic(b)
    finally:
        b.close()
    return out


# KNOWN_DEVIATIONS are cases where the two bouncers are supposed to differ. A
# deviation is a claim someone can check, so each one carries the reason and the
# ZNC source line that makes the difference deliberate rather than a bug. Any
# difference NOT listed here fails the run.
KNOWN_DEVIATIONS = {
    "fingerprints": (
        "salotto validates a fingerprint on Add and normalises on Del; ZNC does "
        "neither (IRCNetwork.h AddTrustedFingerprint only escapes the string, "
        "ClientCommand.cpp:918-943 accepts anything non-empty). A bad fingerprint "
        "in ZNC is accepted and then silently never matches, and Del erases the "
        "unescaped form so a listed fingerprint cannot be removed. Verified by "
        "driving both: ZNC answers Add with 'Done.' for 'not-a-fingerprint' and "
        "keeps listing it after Del. salotto refuses the add with "
        "'not a SHA-256 fingerprint' and does remove what it listed."
    ),
}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--znc", default="salotto-e2e-znc:6667")
    ap.add_argument("--salotto", default="salotto-e2e:6667")
    ap.add_argument("--znc-pass", default="e2e-client-password")
    # ZNC refuses a client whose nick is not the user's nick (464 Invalid
    # Password), so both run under the same identity: the same test, twice.
    ap.add_argument("--nick", default="e2e")
    ap.add_argument("--channel", default="#e2e")
    ap.add_argument("--json", default="")
    args = ap.parse_args()

    def split(addr: str) -> tuple[str, int]:
        host, _, port = addr.rpartition(":")
        return host, int(port)

    zh, zp = split(args.znc)
    sh, sp = split(args.salotto)

    znc = run(zh, zp, args.nick, "znc", args.znc_pass, args.channel)
    sal = run(sh, sp, args.nick, "salotto", None, args.channel)

    diffs, same, deviations = [], [], []
    for case in sorted(set(znc) | set(sal)):
        z, s = znc.get(case), sal.get(case)
        if z == s:
            same.append(case)
        elif case in KNOWN_DEVIATIONS:
            deviations.append(
                {"case": case, "znc": z, "salotto": s,
                 "why": KNOWN_DEVIATIONS[case]}
            )
        else:
            diffs.append({"case": case, "znc": z, "salotto": s})

    report = {
        "same": same,
        "deviations": deviations,
        "different": diffs,
        "znc": znc,
        "salotto": sal,
    }
    if args.json:
        with open(args.json, "w") as fh:
            json.dump(report, fh, indent=2, sort_keys=True)
    print(json.dumps(report, indent=2, sort_keys=True))
    return 1 if diffs else 0


if __name__ == "__main__":
    sys.exit(main())
