# Release Process

This document is the canonical procedure for cutting a new cLLM release on
the development server. Flux is intentionally **not** used on dev — every
deployment goes through the explicit `make deploy` path so a release is a
deliberate, reviewable action rather than a side-effect of a `main` push.

## Versioning rules

* `cllm/internal/buildinfo/version.go` is the **definitive** source of
  truth. Every other version reference exists only to keep build artifacts
  and manifests in sync with it.
* Tags are short SemVer strings: `0.8.0`, `0.9.0`, … (no `v` prefix; the
  existing tag history is the authority).
* `main` always points at the most recent release commit and tag.
* `bartr` carries the next dev version. After each release `bartr` is
  reset to the new `main` and the version is bumped one minor.
* Bump `MINOR` for additive feature blocks (e.g., new admission axes,
  new metrics, new DSL directives). Bump `PATCH` for fix-only releases.
  Bump `MAJOR` only on incompatible client changes (the `ask` CLI is the
  only client; OpenAI subset shape changes count).

## Version touchpoints

When bumping, every one of these must change in lockstep:

| File | Why |
|---|---|
| `cllm/internal/buildinfo/version.go` | Compiled into the binary; surfaced at `/version`. |
| `cllm/Makefile` | `IMAGE ?= cllm:X.Y.Z`; controls what `make build`, `make deploy` produce and import into k3s. |
| `clusters/z01/cllm/deployment.yaml` | Pod `image: cllm:X.Y.Z`. |
| `cllm/README.md` | Four `cllm:X.Y.Z` references in the build/run sections. |

A `grep -rn 'cllm:X\.Y\.Z\|var Version' cllm clusters` after the bump
should return only the new version.

## Release procedure

### 0. Pre-flight: smoke test must pass

A release **never** proceeds before the smoke test is green against the
currently deployed image. The smoke test exercises every admission path
(baseline, KV ladder, kv_oversize rejection, KV-disabled fall-through,
no-kv kill-switch, first-wins precedence, the three KV profiles, and
unpinned least-loaded routing) in a single pass.

```sh
# From a clean workspace, against the currently deployed pod:
go test ./...                    # 1. unit + integration tests green
ask --files scripts/smoke-test.yaml --bench 1   # 2. live smoke green
curl -s http://localhost:8088/version           # 3. confirm what we tested
```

If any of those three steps fails, **stop**. Fix the failure on `bartr`,
re-deploy, re-smoke. Do not tag a release on top of a known regression.

The smoke fixture intentionally drives `node=kv-node` and other
ephemeral targets that should not bleed into long-lived dashboards.
Tombstone any test-only series before continuing so the post-release
dashboards are clean:

```sh
PROM=$(kubectl -n monitoring get svc prometheus -o jsonpath='{.spec.clusterIP}'):9090
curl -s -X POST -g "http://$PROM/api/v1/admin/tsdb/delete_series?match[]={node=\"kv-node\"}"
curl -s -X POST    "http://$PROM/api/v1/admin/tsdb/clean_tombstones"
```

Add additional `match[]=...` selectors for any other smoke-only labels
(experimental node names, throwaway tenants, etc.) introduced this
cycle. Prometheus must have `enableAdminAPI: true` set on its CR — this
is already the case on z01.

### 1. Push the dev branch

```sh
cd ~/vllm
git checkout bartr
git push origin bartr
```

### 2. Squash-merge `bartr` → `main`

A squash merge keeps `main` history linear with one commit per release.
The commit message is always `Version X.Y.Z` (no body — the annotated
tag carries the release notes).

```sh
git checkout main
git pull --ff-only
git merge --squash bartr
git commit -m "Version X.Y.Z"
```

### 3. Tag the release with annotated notes

```sh
git tag -a X.Y.Z -m "Version X.Y.Z: <one-line headline>

<optional bulleted body — feature blocks, new metrics, breaking changes,
backward-compat contracts, ops-relevant config additions>"
```

The tag's body should be self-contained: `git show X.Y.Z` is the release
notes for anyone trying to understand what shipped.

### 4. Push `main` and the tag

```sh
git push origin main X.Y.Z
```

### 5. Reset `bartr` to the new `main` and bump to next dev version

```sh
git checkout -B bartr main
# Bump every version touchpoint (see table above) to X.(Y+1).0
sed -i 's/X\.Y\.Z/X.(Y+1).0/g' cllm/Makefile clusters/z01/cllm/deployment.yaml cllm/README.md
# Edit cllm/internal/buildinfo/version.go by hand or with sed.
go test ./...
git commit -am "bump dev version to X.(Y+1).0"
git push --force-with-lease origin bartr
```

`--force-with-lease` is required because `bartr` was just rewound to
`main`. It is safe because no one else commits to `bartr`.

### 6. Build, deploy, and verify the new version on the cluster

```sh
cd ~/vllm/cllm
make deploy                                      # build + import + rollout
kubectl -n cllm rollout status deployment/cllm
curl -s http://localhost:8088/version            # must show X.(Y+1).0
make install                                     # rebuild the local ask CLI
ask --version                                    # must show X.(Y+1).0
```

### 7. Re-run the smoke test against the freshly deployed dev version

```sh
ask --files scripts/smoke-test.yaml --bench 1
```

This is the same smoke test from step 0, but now validating the
post-bump dev branch on the cluster. If it fails, the bump introduced a
regression — fix on `bartr` before doing any further work.

When it passes, repeat the dashboard tombstone from step 0 — the
post-bump smoke also writes `kv-node` (and any other smoke-only) series.

```sh
PROM=$(kubectl -n monitoring get svc prometheus -o jsonpath='{.spec.clusterIP}'):9090
curl -s -X POST -g "http://$PROM/api/v1/admin/tsdb/delete_series?match[]={node=\"kv-node\"}"
curl -s -X POST    "http://$PROM/api/v1/admin/tsdb/clean_tombstones"
```

## What if the cluster is on a different version than `main`?

The release tag is the authoritative answer to "what is in production".
If `curl /version` disagrees with the latest tag on `main`, run
`make deploy` from `main` to reconcile. Never edit `clusters/z01/cllm/
deployment.yaml` to point at an untagged version on `main`.

`bartr` may legitimately deploy a higher dev version than `main`'s tag —
that is the whole point of the dev branch. The cluster reflects whatever
was last `make deploy`'d.

## Hotfix releases

For an urgent fix that does not warrant the full `bartr` train:

```sh
git checkout -b hotfix-X.Y.(Z+1) X.Y.Z
# … make minimal change, bump version touchpoints …
go test ./... && ask --files scripts/smoke-test.yaml --bench 1
git checkout main
git merge --squash hotfix-X.Y.(Z+1)
git commit -m "Version X.Y.(Z+1)"
git tag -a X.Y.(Z+1) -m "Version X.Y.(Z+1): <fix summary>"
git push origin main X.Y.(Z+1)
# Then forward-merge main back into bartr to pick up the fix:
git checkout bartr && git merge main && git push
```

## Why no Flux on dev

Flux is excellent for stamping out clusters where the desired state is a
declarative Git ref. On dev that produces two failure modes we have no
need for:

1. A `make deploy` from `bartr` would be silently overwritten on the
   next reconcile.
2. A bad commit on `main` would auto-deploy without any human in the
   loop, breaking smoke before anyone can intervene.

Production clusters can adopt Flux against `main` once we have a
green-build gate. Until then, dev keeps the deploy step explicit.
