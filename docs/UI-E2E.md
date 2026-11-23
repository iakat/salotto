# The UI front ends, against a real bouncer

QUEUE.md §2.0 asks for one model and three renderers. The model and the renderers
were built and tested; what was missing was the wire between them and a bouncer,
which is why both front ends rendered a correct, empty screen. This file records
how that is proven, because "the page renders" is not proof that anything is
connected.

## What is deployed

`test/e2e/k8s/ui.yaml` runs both front ends as clients of the bouncer in the same
namespace (`hermes-generated-content`):

| workload | what it is |
| --- | --- |
| `salotto-e2e` | the bouncer daemon, Service `salotto-e2e:6667` |
| `salotto-e2e-ergo` | the real IRCd, Service `:6667` |
| `salotto-e2e-znc` | ZNC 1.10.2, the parity reference |
| `salotto-ui` | `salotto-web` on `:8080` and `salotto-ssh` on `:2222` |

One image, three binaries: all of them share `internal/ui`, so a rendering
difference between the front ends would be a real bug rather than a build
difference.

## The traps these probes are built around

**A UI can echo locally and send nothing.** "The line appears on the page" proves
nothing, because the local echo is drawn by the model. The only check that means
anything is whether the IRCd saw the line, so `probe_ui.py` watches ergo with a
third-party nick and looks for the PRIVMSG itself.

**A bouncer does not replay a client's own sends.** `PLAYBUFFER` is therefore the
wrong place to look for a line a front end just sent. Measured on both bouncers
with a positive control — a line received *from someone else* is replayed, an own
send is not — salotto behaves exactly like ZNC 1.10.2. An earlier version of the
probe reported this as a salotto bug; it was not.

**`ssh(1)` prints the banner before authentication.** A session that appears to
reach the TUI may only have received the banner, so ssh(1) is not a reliable
authentication test. A client library is: paramiko refused the server that had no
password handler and connected once one was installed, which is what settled
whether that handler was needed.

## Running them

Both probes run as pods in the namespace, because that is where the Services
resolve and where `pods/exec` and `pods/portforward` are not available to the
`hermes` ServiceAccount:

```sh
KUBECONFIG=/opt/data/home/.kubeconfig
K=/opt/data/home/.local/bin/kubectl -n hermes-generated-content

$K create cm probe-ui  --from-file=probe_ui.py=test/e2e/probe_ui.py
$K create cm probe-ssh --from-file=probe_ssh.py=test/e2e/probe_ssh.py
$K apply -f /tmp/probeui.yaml   # python:3.12-slim, runs probe_ui.py
$K apply -f /tmp/probessh.yaml  # python:3.12-slim, pip installs paramiko
$K logs ui-probe
$K logs ui-sshprobe
```

A pass looks like this:

```
observer joined #e2e
POST / -> 303
ergo saw the line: True
page shows the line: True
bouncer answered a command in the UI: True
RESULT: PASS

ssh session open
line from the bouncer reached the SSH screen: True
256-colour SGR present: True
typed line reached ergo: True
RESULT: PASS
```

The SSH probe checks both directions: a line put in the channel from ergo must
reach the session screen (bouncer → transport → model → terminal), and a line
typed into the session must reach ergo. It also asserts 256-colour SGR, which is
the classic daemon-has-no-TTY failure.

## Rebuilding after a change

The kaniko Job builds from the git branch, not the working tree, so **push before
rebuilding** or the image silently contains the previous commit:

```sh
sed 's/name: salotto-e2e-image-build/name: salotto-e2e-image-build-N/' \
  test/e2e/k8s/salotto-build.yaml > /tmp/b.yaml
$K apply -f /tmp/b.yaml
$K delete pod -l app=salotto-ui --wait=false   # :e2e is a stable tag + Always pull
```
- **A front end must notice its bouncer dying.** The web driver holds one
  transport for the process, so when the bouncer was redeployed underneath it the
  page kept rendering, the input box kept accepting text and POST kept answering
  303, while every line went nowhere: healthy-looking and completely dead. There
  is no in-process reconnect, deliberately — the model is a view of a bouncer that
  keeps the history, so coming back is a fresh dial that replays the buffer — so
  the driver exits non-zero and lets the supervisor make that dial.
  `TestWebExitsWhenTheBouncerGoesAway` builds the real binary, points it at a
  listener that accepts and hangs up, and requires a non-zero exit within twenty
  seconds. The SSH front end never had this bug because it dials per session.
- **A password must be offered, not merely accepted.** `probe_ssh.py` reported
  "No authentication methods available" against a working server: the handler
  accepts any password when none is configured, but a client that offers none has
  no method in common and is refused before the handler runs.
