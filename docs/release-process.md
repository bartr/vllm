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
curl -s -X POST -g "http://$PROM/api/v1/admin/tsdb/delete_series?match[]={node=\"rtx\"}"
curl -s -X POST    "http://$PROM/api/v1/admin/tsdb/clean_tombstones"
```

Add additional `match[]=...` selectors for any other smoke-only labels
(experimental node names, throwaway tenants, etc.) introduced this
cycle. Prometheus must have `enableAdminAPI: true` set on its CR — this
is already the case on z01.

### 1. Push the dev branch and open a PR

```sh
cd ~/vllm
git checkout bartr
git push origin bartr
gh pr create --base main --head bartr --title "Release X.Y.Z" --body-file -
```

The PR body should describe what is shipping (feature blocks, new
metrics, breaking changes, ops-relevant config). It is the input for
the annotated tag in step 3 — you can copy from the PR body when
writing the tag message.

**Solo-dev note.** cLLM currently has one engineer. Self-approval is
fine and the PR does not need a second reviewer. The PR exists to
produce **repo evidence** (a permanent, addressable record of what
shipped, why, and what the diff looked like) and to keep the workflow
identical to how FDEs work on multi-engineer teams. When a second
engineer joins cLLM, the workflow does not change — only the
approval policy does.

Do not skip this step "because it is just me." The PR is the artifact,
not the gate.

### 2. Merge the PR — rebase by default, merge commit only if rebase fails

**Default: rebase merge.**

```sh
gh pr merge --rebase --delete-branch
```

This replays every commit from `bartr` onto `main` and fast-forwards.
Result: linear history, **every individual commit retained**, no merge
commit. This is the cLLM default.

**Fallback: merge commit, only when rebase cannot succeed cleanly.**

```sh
gh pr merge --merge --delete-branch
```

Use `--merge` only when GitHub refuses to rebase (conflicts that need
human resolution, or a long-lived branch where the merge commit itself
is a meaningful waypoint). The merge commit also retains every
individual commit — it just adds the diamond shape to the graph.

**Never `--squash`.** Squash collapses the entire branch into one
commit and destroys the per-commit history we need for storytelling,
forensics, and AI-assisted reconstruction.

The release commit on `main` is whatever the tip of `bartr` already
is (rebase) or the merge commit (`--merge`). There is no separate
"Version X.Y.Z" commit — the **annotated tag** (step 3) carries the
release notes.

#### Why never squash, and why prefer rebase

We used to squash-merge with the message `Version X.Y.Z`. It produced
a clean linear `main` history — one commit per release — and that felt
tidy at the time.

The cost only became visible later, when telling the cLLM story:
**every release collapsed dozens of real commits into one.** The HVE
narrative — "99 commits in the first 5 days," the day-by-day evolution
of admission, the DSL, the MCP server — was largely invisible on
`main`. The history we needed for evidence had been thrown away.

Rebase merge keeps the full commit graph **and** keeps `main` linear.
Merge commits keep the full graph but add diamond shapes per PR.
Both retain history; rebase just looks cleaner in `git log` and
`git bisect`. Squash retains nothing and is forbidden.

The annotated tag still gives us a clean release-notes view; the
per-commit detail is preserved underneath. **Storytelling and
forensics both want the full history. Releases want a single
authoritative anchor.** The tag is that anchor; the commits are the
story.

### 3. Tag the release with annotated notes

```sh
git tag -a X.Y.Z -m "Version X.Y.Z: <one-line headline>

<optional bulleted body — feature blocks, new metrics, breaking changes,
backward-compat contracts, ops-relevant config additions>"
```

The tag's body should be self-contained: `git show X.Y.Z` is the release
notes for anyone trying to understand what shipped.

### 4. Pull `main` and push the tag

After `gh pr merge` the remote `main` is already updated. Sync local
`main` and push the tag:

```sh
git checkout main
git pull --ff-only
git push origin X.Y.Z
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

### 8. Update repo memory

Repo memory (`/memories/repo/*.md`) is the agent's durable record of
verified codebase facts and conventions. Anything new this cycle that
future sessions should not have to re-derive belongs there. Audit and
update before declaring the release done:

* New invariants, conventions, or hard-won debugging lessons (mirror or
  link to the matching addition in `.github/instructions/cllm.instructions.md`).
* Build / deploy / smoke commands whose flags or defaults changed.
* Newly-stable node IDs, class names, metric labels, or DSL directives
  that other notes will reference by name.
* Removals: delete or correct any entry that this release made wrong
  (e.g., a renamed metric, a retired endpoint, a flipped default).

Keep entries terse — one fact per bullet, no walls of prose. The goal
is a high-signal lookup table, not a changelog. The release notes on
the annotated tag remain the changelog.

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
git push origin hotfix-X.Y.(Z+1)
gh pr create --base main --head hotfix-X.Y.(Z+1) --title "Hotfix X.Y.(Z+1)" --body "<fix summary>"
gh pr merge --rebase --delete-branch     # rebase by default; --merge only if rebase fails. Never --squash.
git checkout main && git pull --ff-only
git tag -a X.Y.(Z+1) -m "Version X.Y.(Z+1): <fix summary>"
git push origin X.Y.(Z+1)
# Then forward-merge main back into bartr to pick up the fix:
git checkout bartr && git merge main && git push
```

Same merge-strategy rule as the main release path: rebase by default,
`--merge` only when rebase cannot succeed cleanly, **never** `--squash`.

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
