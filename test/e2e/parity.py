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
import os
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
    ensure_joined(b, channel)
    b.ask("JOIN " + channel, 2.0)
    echo = b.ask("PRIVMSG %s :parity-echo" % channel, 1.5)
    return {"joined": True, "echoed": says(echo, "parity-echo")}


def ensure_joined(b: Bouncer, channel: str) -> None:
    """Join the channel if we are not already in it.

    Cases in this file run against one long-lived bouncer and some of them
    perturb persistent state on purpose (chansaver parts the channel). Rather
    than depending on case order, every case that needs the channel asks for it.
    """
    lines = b.ask("JOIN " + channel, 1.5)
    # 366 is RPL_ENDOFNAMES, which a successful JOIN always produces.
    if not any(" 366 " in l for l in lines):
        b.drain(1.5)


def case_playback(b: Bouncer, channel: str, marker: str) -> dict:
    """A line buffered while we were detached must replay on PlayBuffer."""
    ensure_joined(b, channel)
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
    # Leave the fixture connected. The next case joins channels and expects them to
    # reach the IRCd; if this case returned while the upstream was still dialling, the
    # next one measures a dropped JOIN and reports a feature as broken. Cases must not
    # depend on each other's timing, so this one waits for its own aftermath.
    wait_connected(b)
    return {
        "disconnect_answered": says(dis, "disconnected"),
        "connect_answered": says(con, "connect"),
    }


def wait_connected(b: Bouncer, budget: float = 120.0) -> bool:
    """Wait until the bouncer reports its upstream is connected.

    Necessary before anything that depends on a line reaching the IRCd. A bouncer
    accepts client lines while its upstream is down and simply drops them, and the
    client cannot tell: its own line comes back from the replay buffer, so a naive
    check (does a 366 for this channel appear?) matches history and proves nothing.
    That is exactly how the chansaver control reported a join that never happened.
    """
    end = time.time() + budget
    while time.time() < end:
        lines = b.cmd("ListNetworks", seconds=2.0)
        text = " ".join(lines)
        # ZNC prints "Connected" in the Connected column; salotto prints "on irc".
        if re.search(r"\bConnected\b", text) or re.search(r"\bon irc\b", text, re.I):
            return True
        time.sleep(3.0)
    return False


def join_and_confirm(b: Bouncer, channel: str, budget: float = 90.0) -> bool:
    """Join a channel and wait for RPL_ENDOFNAMES (366), retrying.

    A bouncer's upstream can be down when the client speaks: the client line is then
    dropped, no JOIN reaches the IRCd, and any later assertion about "the channel we
    joined" is about a join that never happened. That is not a hypothetical — it is
    why the chansaver control failed on salotto while the module worked: the case
    sent JOIN during salotto's reconnect backoff.

    366 is the honest proof the join landed, because the server answers it and the
    bouncer can only relay what it received. Returns whether it landed.
    """
    # The upstream has to be up, or the join is dropped on the floor and the 366 we
    # might see is replayed history rather than a fresh answer.
    if not wait_connected(b):
        return False
    # Clear anything already buffered so a replayed 366 cannot be mistaken for the
    # answer to this join.
    b.drain(1.5)
    end = time.time() + budget
    while time.time() < end:
        b.send("JOIN " + channel)
        for line in b.drain(6.0):
            # A replay line carries a batch tag; a live answer does not.
            if "batch=" in line and "-replay" in line:
                continue
            if " 366 " in line and channel.lower() in line.lower():
                return True
    return False


def joined_channels(b: Bouncer) -> set[str]:
    """The channels the bouncer says it is joined to, from its own list command.

    This is the honest measurement for chansaver, and it is the same question on both
    sides. Watching the client stream does not work: a bouncer only delivers a window's
    lines live to a client attached to that window, so the JOIN answering its own
    rejoin is buffered rather than sent, and "I did not see a JOIN" says nothing about
    whether the rejoin happened. Reading the bouncer's list avoids the ambiguity
    entirely.
    """
    out: set[str] = set()
    for line in b.cmd("ListChans", seconds=2.5):
        # Both bouncers render a table; the name is a cell starting with #, and the
        # next cell is its status. A buffer size or mode column also starts with a
        # digit or +, so requiring the status word keeps the parse honest.
        cells = [c.strip().lstrip("@+") for c in re.split(r"[|]", line)]
        for k, cell in enumerate(cells):
            if not cell.startswith("#"):
                continue
            status = cells[k + 1] if k + 1 < len(cells) else ""
            if re.search(r"\b(Joined|Detached|Disabled)\b", status, re.I):
                out.add(cell.lower())
    return out


def case_chansaver(b: Bouncer, channel: str, observer: "ErgoObserver") -> dict:
    """Does a channel joined inside the session survive a reconnect, and does a
    channel parted inside the session stay gone?

    Both halves are needed, and the positive control comes first. chansaver's job is
    that the channel list follows our own JOIN and PART (chansaver.cpp:69-92); a case
    that only checks "a parted channel did not come back" passes for a chansaver that
    records nothing at all, which is how this case found a real bug.

    The verdict comes from the IRCd, not from either bouncer. The bouncers answer
    different questions through their list commands — ZNC lists channels in its config,
    salotto lists known windows — and neither reflects "am I actually in this channel
    now". The observer joins the same network as a third party and asks ergo who is in
    the channel: after a reconnect the bouncer must be there for the channel it kept
    and absent for the one it parted.
    """
    stamp = int(time.time()) % 100000
    keep = "%s-keep%d" % (channel, stamp)
    gone = "%s-gone%d" % (channel, stamp)
    bnick = b.nick

    # The observer joins both channels: NAMES only lists members to a client that is
    # itself in the channel.
    observer.join(keep)
    observer.join(gone)

    joined_keep = join_and_confirm(b, keep)
    joined_gone = join_and_confirm(b, gone)

    # Part the second one in-session: it must not come back.
    b.send("PART " + gone)
    b.drain(2.0)

    b.cmd("Disconnect", seconds=2.5)
    b.cmd("Connect", seconds=2.5)

    def present(ch: str) -> bool:
        # NAMES is answered by the IRCd, so this is about the wire, not about what
        # the bouncer believes. The bouncer's own nick may be e2e or e2e_ (the fixture
        # shares one IRCd between two bouncers), so any name starting with the base
        # nick counts.
        lines = observer.names(ch)
        if os.environ.get("CHANSAVER_DEBUG"):
            for line in lines:
                print("### names %s: %r" % (ch, line), file=sys.stderr)
        for line in lines:
            if " 353 " not in line:
                continue
            names = line.split(":", 1)[-1]
            for name in re.split(r"[\s]+", names):
                name = name.lstrip("@+%~&")
                if name and bnick and name.lower().startswith(bnick.lower()[:3]):
                    return True
        return False

    end = time.time() + 120.0
    kept = False
    while time.time() < end and not kept:
        kept = present(keep)
        if not kept:
            time.sleep(5.0)

    # Same patience for the negative: absence is only meaningful once the positive
    # has been observed on the same run.
    back = present(gone)

    if os.environ.get("CHANSAVER_DEBUG"):
        print("### keep=%s present=%s | gone=%s present=%s"
              % (keep, kept, gone, back), file=sys.stderr)

    # Tidy up so later cases start from the configured channel only.
    b.send("PART " + keep)
    b.drain(1.5)
    b.send("JOIN " + channel)
    b.drain(1.5)

    return {
        "joined_keep": joined_keep,
        "joined_gone": joined_gone,
        "kept_after_reconnect": kept,
        "parted_channel_came_back": back,
        "chansaver_ok": joined_keep and joined_gone and kept and not back,
    }


# ZNC's away reason format, from simple_away + CUtils::CTime: a C-locale %c plus
# an explicit " UTC" (simple_away.cpp:213-215, Utils.cpp:510-512).
ZNC_AWAY_RE = re.compile(r"Auto away at \w{3} \w{3} [ \d]\d [\d:]{8} \d{4} UTC")


class ErgoObserver:
    """A plain IRC client on ergo, used to see what the bouncer does on the wire.

    Some behaviour only exists upstream: simple_away's whole observable effect is
    an `AWAY :reason` that ergo relays to the channels we share. Asking the
    bouncer about it would be asking the defendant for a reference, so the away
    case watches ergo instead.
    """

    def __init__(self, host: str, port: int, nick: str, channel: str):
        self.sock = socket.create_connection((host, port), timeout=15)
        self.sock.settimeout(0.3)
        self.buf = b""
        self.pending: list[str] = []
        # away-notify is not optional here: a plain AWAY is never relayed to
        # other clients, so without this capability the observer would see
        # nothing, both sides would "agree" on nothing, and the case would pass
        # for the wrong reason.
        self.send("CAP REQ :away-notify")
        self.send("CAP END")
        self.send("NICK " + nick)
        self.send("USER %s 0 * :%s observer" % (nick, nick))
        self.drain(1.5)
        self.send("JOIN " + channel)
        self.drain(1.5)

    def send(self, line: str) -> None:
        self.sock.sendall(line.encode() + b"\r\n")

    def drain(self, seconds: float = 1.0) -> list[str]:
        end = time.time() + seconds
        while time.time() < end:
            try:
                chunk = self.sock.recv(65536)
            except (socket.timeout, OSError):
                continue
            if not chunk:
                break
            self.buf += chunk
            while b"\n" in self.buf:
                raw, self.buf = self.buf.split(b"\n", 1)
                self.pending.append(raw.rstrip(b"\r").decode("utf-8", "replace"))
        out, self.pending = self.pending, []
        return out

    def join(self, channel: str) -> None:
        """Join a channel so NAMES on it answers. ergo replies 353 with the member
        list only to a client in the channel; outside it, NAMES returns an empty 366,
        which would read as "the bouncer is not there" no matter what it did.
        """
        self.send("JOIN " + channel)
        self.drain(1.5)

    def names(self, channel: str) -> list[str]:
        """Who ergo says is in a channel. The bouncer's own list cannot answer this:
        ZNC lists channels in its config and salotto lists known windows, so the two
        commands are different questions. The IRCd is the only witness both sides
        answer identically.
        """
        self.send("NAMES " + channel)
        out: list[str] = []
        end = time.time() + 4.0
        while time.time() < end:
            for line in self.drain(0.4):
                out.append(line)
                if " 366 " in line:  # RPL_ENDOFNAMES
                    return out
        return out

    def whois(self, target: str) -> list[str]:
        """Ask ergo about a nick and return what came back for it."""
        self.send("WHOIS " + target)
        out: list[str] = []
        end = time.time() + 4.0
        while time.time() < end:
            for line in self.drain(0.4):
                out.append(line)
                if " 318 " in line:  # RPL_ENDOFWHOIS
                    return out
        return out

    def close(self) -> None:
        try:
            self.send("QUIT :done")
            self.sock.close()
        except OSError:
            pass


def run_away(host: str, port: int, nick: str, dialect: str, password: str | None,
             channel: str, ergo_host: str, ergo_port: int) -> dict:
    """Detach, wait, and ask ergo whether the bouncer is away.

    Two earlier versions of this case were wrong in instructive ways. The first
    watched the channel for an AWAY line without negotiating away-notify, and
    a plain AWAY is never relayed to other clients, so it saw nothing and both
    sides "agreed" on nothing. The second negotiated the capability but the
    probe still saw nothing, because away-notify only relays to clients that
    share a channel and neither bouncer was actually in #e2e — the observer sat
    there alone.

    So the measurement is WHOIS, which needs no shared channel: ergo answers
    301 RPL_AWAY with the reason for anyone who asks, which is the same thing a
    human sees in /whois. The probe also re-joins the channel first, so the case
    repairs the fixture state that the chansaver case perturbs.
    """
    obs = ErgoObserver(ergo_host, ergo_port, "obs" + ("z" if dialect == "znc" else "s"), channel)
    try:
        b = Bouncer(host, port, nick, dialect, password)
        b.drain(2.0)
        # Make sure the bouncer is in the channel: the chansaver case parts it on
        # purpose, and a bouncer that is not in the channel makes the away
        # invisible to channel-based checks.
        b.send("JOIN " + channel)
        b.drain(2.0)
        # The upstream identity may be the alt nick when both bouncers share the
        # ergo: whoever dialled second gets renamed. Ask about both.
        candidates = [nick, nick + "_"]
        b.close()          # the last client leaving is what arms simple_away
        obs.drain(12.0)

        for candidate in candidates:
            lines = obs.whois(candidate)
            away = [l for l in lines if " 301 " in l]
            if away:
                reason = away[0].split(" :", 1)[-1]
                return {
                    "away": True,
                    "reason_has_znc_format": bool(ZNC_AWAY_RE.match(reason)),
                }
        return {"away": False, "reason_has_znc_format": False}
    finally:
        obs.close()


def run(host: str, port: int, nick: str, dialect: str, password: str | None, channel: str, only: set[str] | None = None, observer: "ErgoObserver | None" = None) -> dict:
    b = Bouncer(host, port, nick, dialect, password)
    out: dict[str, dict] = {}

    def want(case: str) -> bool:
        return only is None or case in only

    try:
        if want("registration"):
            out["registration"] = case_registration(b)
        if want("join"):
            out["join"] = case_join_echo(b, channel)
        if want("playback"):
            out["playback"] = case_playback(b, channel, "parity-play")
        if want("listchans"):
            out["listchans"] = case_listchans(b, channel)
        if want("setbuffer"):
            out["setbuffer"] = case_setbuffer(b, channel)
        if want("disable"):
            out["disable"] = case_disable(b, channel)
        if want("clear"):
            out["clear"] = case_clear(b, channel, "parity-clear")
        if want("fingerprints"):
            out["fingerprints"] = case_fingerprints(b)
        if want("listnetworks"):
            out["listnetworks"] = case_listnetworks(b)
        if want("listservers"):
            out["listservers"] = case_listservers(b)
        if want("listclients"):
            out["listclients"] = case_listclients(b)
        if want("traffic"):
            out["traffic"] = case_traffic(b)
        if want("connect_disconnect"):
            out["connect_disconnect"] = case_connect_disconnect(b)
        if want("chansaver"):
            if observer is None:
                out["chansaver"] = {"error": "no observer: cannot read the IRCd, so "
                                    "the case would compare two silences"}
            else:
                out["chansaver"] = case_chansaver(b, channel, observer)
    finally:
        b.close()
    return out


# Some predicates must be TRUE on both sides. Parity alone cannot catch a case
# that fails identically on both bouncers: `{"x": false} == {"x": false}` is a
# matching pair and the harness would call it agreement. Every key listed here is
# asserted true on each side before the comparison runs, and a false one is a real
# failure even when both sides agree. This exists because `chansaver` reported
# `rejoined_after_part: false` on both sides while the feature was working — the
# negative half is correct, but only a positive control can prove it.
MUST_BE_TRUE: dict[str, dict[str, bool]] = {
    "chansaver": {"joined_keep": True, "joined_gone": True, "kept_after_reconnect": True},
    "join": {"joined": True},
    "playback": {"played": True},
    "registration": {"welcome": True},
    "clear": {"cleared": True},
    "setbuffer": {"says_set": True},
}

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
    ap.add_argument("--ergo", default="salotto-e2e-ergo:6667")
    ap.add_argument("--json", default="")
    # One case at a time, for debugging a single scenario without paying for the
    # whole suite: `--only chansaver`.
    ap.add_argument("--only", default="")
    args = ap.parse_args()
    only = {c.strip() for c in args.only.split(",") if c.strip()} or None

    def split(addr: str) -> tuple[str, int]:
        host, _, port = addr.rpartition(":")
        return host, int(port)

    zh, zp = split(args.znc)
    sh, sp = split(args.salotto)

    eh, ep = split(args.ergo)

    # One observer per bouncer, on the same IRCd: it is the only witness that answers
    # "is the bouncer in this channel" identically for both.
    def make_observer(nick: str) -> "ErgoObserver | None":
        try:
            return ErgoObserver(eh, ep, nick, args.channel)
        except OSError as exc:
            print("observer unavailable: %s" % exc, file=sys.stderr)
            return None

    zobs = make_observer("zobs")
    znc = run(zh, zp, args.nick, "znc", args.znc_pass, args.channel, only, zobs)
    sobs = make_observer("sobs")
    sal = run(sh, sp, args.nick, "salotto", None, args.channel, only, sobs)
    for o in (zobs, sobs):
        if o is not None:
            o.close()
    # The away case runs after the others and one bouncer at a time: both are
    # attached to the same ergo under nick e2e/e2e_, and an away probe needs the
    # channel quiet.
    if only is None or "away" in only:
        znc["away"] = run_away(zh, zp, args.nick, "znc", args.znc_pass, args.channel, eh, ep)
        sal["away"] = run_away(sh, sp, args.nick, "salotto", None, args.channel, eh, ep)

    # A predicate that must hold, checked per side *before* the comparison: two
    # identical failures are not parity, they are a broken fixture.
    unmet = []
    for case, fields in MUST_BE_TRUE.items():
        if only is not None and case not in only:
            continue
        for field, want in fields.items():
            for side, got in (("znc", znc), ("salotto", sal)):
                value = (got.get(case) or {}).get(field)
                if value != want:
                    unmet.append({"case": case, "field": field, "side": side,
                                  "got": value, "want": want})
    if unmet:
        for u in unmet:
            print("UNMET: %(side)s %(case)s.%(field)s = %(got)r, want %(want)r" % u,
                  file=sys.stderr)

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
        "unmet": unmet,
        "znc": znc,
        "salotto": sal,
    }
    if args.json:
        with open(args.json, "w") as fh:
            json.dump(report, fh, indent=2, sort_keys=True)
    print(json.dumps(report, indent=2, sort_keys=True))
    return 1 if (diffs or unmet) else 0


if __name__ == "__main__":
    sys.exit(main())
