#!/usr/bin/env python3
"""Prove uploads work over the public HTTPS host, end to end.

The claim under test is not "the endpoint answers" but "a file that leaves this process comes back
byte for byte, with its type taken from its contents". So the probe:

  1. POSTs a real PNG (with a deliberate lie about its type in the filename)
  2. reads the JSON back and checks the id, the sniffed type and the size
  3. GETs the URL it was given and compares the bytes
  4. reads the headers a browser acts on: type, disposition, nosniff, cache
  5. uploads an oversized body and asserts the refusal is 413, not a silent truncation
  6. uploads an expired-by-policy file - not possible without waiting, so instead it checks that
     the served file has an expiry at all, which is the property that makes it go away
  7. deletes it and asserts the URL is then 404

Anything that could pass because both sides saw nothing is a failure here: the bytes are compared,
not the status code alone.
"""
import hashlib
import io
import json
import sys
import urllib.error
import urllib.request

HOST = "https://alice.ch-test.katia.sh"
BASE = HOST + "/uploads"

# A minimal but genuinely valid PNG: signature, IHDR, IDAT, IEND. Small enough to paste, real
# enough that a browser renders it, and not a shape the server could guess from the extension.
PNG = bytes.fromhex(
    "89504e470d0a1a0a"
    "0000000d49484452000000010000000108060000001f15c489"
    "0000000a49444154789c6360000002000100ffff03000006000557bfabd4"
    "0000000049454e44ae426082"
)


def post(name, body, content_type):
    boundary = "----salottoprobe"
    buf = io.BytesIO()
    buf.write(("--" + boundary + "\r\n").encode())
    buf.write(
        ('Content-Disposition: form-data; name="file"; filename="%s"\r\n' % name).encode()
    )
    if content_type:
        buf.write(("Content-Type: " + content_type + "\r\n").encode())
    buf.write(b"\r\n")
    buf.write(body)
    buf.write(("\r\n--" + boundary + "--\r\n").encode())
    req = urllib.request.Request(
        BASE + "/",
        data=buf.getvalue(),
        headers={"Content-Type": "multipart/form-data; boundary=" + boundary},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(errors="replace")


def main():
    ok = True

    def check(label, cond, detail=""):
        nonlocal ok
        print(("PASS  " if cond else "FAIL  ") + label + (("  " + detail) if detail else ""))
        if not cond:
            ok = False

    # 1. A real PNG claimed as plain text. The claim is wrong on purpose: an endpoint that
    #    believed it would store the file as text/plain and serve it as a download.
    status, meta = post("probe.png", PNG, "text/plain")
    check("upload accepted", status == 201, "status=%s body=%s" % (status, str(meta)[:120]))
    if status != 201:
        return 1
    check("size reported", meta.get("size") == len(PNG), "%s vs %s" % (meta.get("size"), len(PNG)))
    check("type sniffed from bytes", meta.get("mime") == "image/png", "mime=%s" % meta.get("mime"))

    # 2. Fetch it back and compare the bytes.
    with urllib.request.urlopen(meta["url"], timeout=30) as r:
        back = r.read()
        ctype = r.headers.get("Content-Type")
        disp = r.headers.get("Content-Disposition", "")
        nosniff = r.headers.get("X-Content-Type-Options")
        cache = r.headers.get("Cache-Control", "")
    check("bytes identical", hashlib.sha256(back).hexdigest() == hashlib.sha256(PNG).hexdigest(),
          "%d in, %d out" % (len(PNG), len(back)))
    check("served as png", ctype == "image/png", "content-type=%s" % ctype)
    check("nosniff present", nosniff == "nosniff", "x-content-type-options=%s" % nosniff)
    # The claim disagreed with the bytes, so this downloads even though a PNG is harmless.
    check("contradiction forces download", disp.startswith("attachment"), "disposition=%s" % disp)
    check("cached as immutable", "immutable" in cache, "cache-control=%s" % cache)

    # 3. The same bytes with an honest claim: now it renders inline, which is the other half.
    status2, meta2 = post("honest.png", PNG, "image/png")
    check("honest upload accepted", status2 == 201, "status=%s" % status2)
    if status2 == 201:
        with urllib.request.urlopen(meta2["url"], timeout=30) as r:
            disp2 = r.headers.get("Content-Disposition", "")
        check("honest file renders inline", disp2.startswith("inline"), "disposition=%s" % disp2)

    # 4. An oversized body is refused, not truncated into a smaller file.
    #
    # Two acceptable shapes, and they are the two honest ones: a 413 when the server can answer
    # before the body arrives (which is what a browser gets, because it declares its length), or a
    # closed connection when it cannot. What must never happen is a 201 - a truncated file that the
    # client believes was uploaded.
    try:
        status3, body3 = post("huge.bin", b"x" * (26 * 1024 * 1024), "application/octet-stream")
        ok3 = status3 == 413
        detail3 = "status=%s" % status3
    except (urllib.error.URLError, ConnectionError, OSError) as e:
        # urllib sends the whole body, so it is the chunked-ish client here: the server refuses
        # mid-stream and the socket closes under it. That is a refusal, and the file is not stored.
        ok3 = True
        detail3 = "connection closed mid-body (%s) - the refusal arrived before the body finished" % type(e).__name__
    check("oversized upload refused", ok3, detail3)
    # And nothing of it was stored.
    with urllib.request.urlopen(BASE + "/?list=1", timeout=30) as r:
        listing0 = json.loads(r.read())
    check("no oversized file stored",
          all(f["size"] < 26 * 1024 * 1024 for f in listing0.get("files", [])),
          "largest stored: %s" % max([f["size"] for f in listing0.get("files", [])] or [0]))

    # 5. The expiry is reported: a file that expires is the difference between a share and
    #    permanent hosting.
    check("expiry reported", bool(meta.get("expires")), "expires=%s" % meta.get("expires"))

    # 6. Delete, then the URL is gone.
    req = urllib.request.Request(BASE + "/" + meta["id"], method="DELETE")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            dstatus = r.status
    except urllib.error.HTTPError as e:
        dstatus = e.code
    check("delete accepted", dstatus == 204, "status=%s" % dstatus)
    try:
        with urllib.request.urlopen(meta["url"], timeout=30) as r:
            check("deleted file is gone", False, "still served with %s" % r.status)
    except urllib.error.HTTPError as e:
        check("deleted file is gone", e.code == 404, "status=%s" % e.code)

    # 7. The list is this user's own and no longer contains the deleted file.
    with urllib.request.urlopen(BASE + "/?list=1", timeout=30) as r:
        listing = json.loads(r.read())
    ids = [f["id"] for f in listing.get("files", [])]
    check("deleted file not listed", meta["id"] not in ids, "%d files, %d bytes used" % (len(ids), listing.get("used", -1)))
    check("the honest file is listed", meta2["id"] in ids, "")

    print("\n" + ("ALL PASS" if ok else "FAILURES ABOVE"))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
