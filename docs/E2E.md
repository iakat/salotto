# Long-lived test deployment

Three workloads in the `hermes-generated-content` namespace, all reachable
inside the cluster. This is the environment the parity harness drives; it is
meant to keep running, not to be torn down after a run.

| what | service | image |
|---|---|---|
| ircd | `salotto-e2e-ergo:6667` | `ghcr.io/ergochat/ergo:stable` |
| ZNC 1.10.2 | `salotto-e2e-znc:6667` | `docker.io/library/znc:1.10.2` |
| salotto | `salotto-e2e:6667` | `10.43.135.163:5000/salotto/salottod:e2e` |

```
kubectl -n hermes-generated-content apply -f test/e2e/k8s/ergo.yaml
kubectl -n hermes-generated-content apply -f test/e2e/k8s/znc.yaml
kubectl -n hermes-generated-content apply -f test/e2e/k8s/salotto.yaml
```

ZNC is unmodified upstream; there is no ZNC patch in this repository. Its
`znc.conf` is rendered at container start from `$ZNC_PASS`, so no password is in
git.

## Rebuilding the salotto image

The image is built in-cluster by kaniko, straight from GitHub `dev`, so a pushed
commit is the only input:

```
kubectl -n hermes-generated-content delete job salotto-e2e-image-build
kubectl -n hermes-generated-content apply -f test/e2e/k8s/salotto-build.yaml
kubectl -n hermes-generated-content logs job/salotto-e2e-image-build
kubectl -n hermes-generated-content delete deploy salotto-e2e
kubectl -n hermes-generated-content apply -f test/e2e/k8s/salotto.yaml
```

The tag is stable (`:e2e`) and the Deployment sets `imagePullPolicy: Always`,
because a `:e2e` tag that never moves lets the node serve the previous binary
from its cache and quietly invalidate the run.

## Running the parity harness

The salotto image is distroless, so the harness runs from a ConfigMap in a
throwaway python pod instead of `kubectl exec`:

```
kubectl -n hermes-generated-content create configmap salotto-e2e-parity \
  --from-file=parity.py=test/e2e/parity.py
kubectl -n hermes-generated-content run parity-run --restart=Never \
  --image=python:3.12-slim --overrides='{"spec":{"containers":[{"name":"parity",
  "image":"python:3.12-slim","command":["python3","/e2e/parity.py"],
  "volumeMounts":[{"name":"p","mountPath":"/e2e"}]}],
  "volumes":[{"name":"p","configMap":{"name":"salotto-e2e-parity"}}]}}'
kubectl -n hermes-generated-content logs parity-run
```

Exit code 1 means at least one case differs. A case that differs on purpose must
be listed in `KNOWN_DEVIATIONS` in `parity.py` with the reason and the ZNC source
line, so "we deviate" is a claim with a citation rather than a shrug.

## Test fixtures that must stay in step

Both bouncers run under the same identity (`e2e`), because ZNC answers `464
Invalid Password` to a client whose nick is not the user's nick. The upstreams
are separate: ZNC has its own network to ergo, salotto another. When one bouncer
connects to ergo first it takes the nick, so the other lands on `e2e_`; that is a
fixture artefact and the harness normalises it.

ZNC's config has several sharp edges worth recording, because each one cost a
debugging round:

- `Pass` is split on `#`, so a plaintext password is written `plain#<password>`.
- The config belongs at `/znc-data/configs/znc.conf`, not `/znc-data/znc.conf`.
- The binary is `/opt/znc/bin/znc` and needs `--allow-root`.
- `Version = 1.10.2` is required, and a `<Listener>` is required to listen.
- `Server = host port` uses a space; `+6667` in that field means SSL, not a port
  offset (`CServer::Parse`).

## A case that passes for the wrong reason

Three times now a case has been green because both bouncers did nothing, which is
not parity. Each one is now guarded, and the guards are the reason to trust the
rest:

- **`MUST_BE_TRUE`** in `parity.py` lists predicates that have to be *true* on both
  sides before the comparison runs. `{"x": false} == {"x": false}` is a matching
  pair, and the harness used to call that agreement. Without this table, `chansaver`
  reported `kept_after_reconnect: false` on both sides and was counted as `same`.
- **chansaver's positive control.** Its only original assertion was that a parted
  channel does *not* come back after a reconnect — which is exactly what a dead
  chansaver produces. It now joins a throwaway channel first and requires that to
  survive the reconnect. That control is what exposed a real bug: salotto's
  `networkAdapter` did not implement `Nick()`, so the facade could not tell which
  lines were ours, so the chansaver recorded nothing and no channel was ever
  re-joined. ZNC passed the control, salotto failed it.
- **Every window is scanned, with a budget that outlasts the slow bouncer.** A
  rejoin can happen inside the `Connect` command's own drain; watching only later
  output reports a working rejoin as a failure, and a 20 s window measured the
  harness's patience rather than ZNC's reconnect backoff.

The rule that follows: when a differential case fails identically on both sides,
suspect the measurement first — the fixtures are two different programs, so
agreeing on nothing is usually the harness.
