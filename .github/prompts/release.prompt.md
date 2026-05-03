---
mode: agent
description: Cut a new cllm release. Walks the version-touchpoint bump, smoke gate, fast-forward merge, annotated tag, push, post-release bartr reset + dev bump, redeploy, and post-deploy verification. Use when the user says "release", "cut version", "tag X.Y.Z", or "bump to X.Y.Z".
---

# Release a new cllm version

Canonical doc: `docs/release-process.md`. This prompt is the executable wrapper. **Do not deviate from the order of steps.**

## Inputs (ask the user up-front if missing)
1. Target release version `X.Y.Z` (will be tagged on `main`).
2. Next dev version `X.(Y+1).0` (or `X.Y.(Z+1)` for hotfix). Default: minor bump.
3. One-line release headline + optional bulleted body for the annotated tag.

## Pre-flight (BLOCKING — abort if any step fails)
Run from `/home/bartr/vllm/cllm` against the currently deployed pod:
```sh
cd /home/bartr/vllm/cllm && go test ./...
~/go/bin/ask --files /home/bartr/vllm/scripts/smoke-test.yaml --bench 1
curl -fs http://localhost:8088/version
```
All three must succeed. If any fail: STOP, surface the failure, do not tag.

## Version touchpoints (all four must move in lockstep to `X.Y.Z`)
1. `cllm/internal/buildinfo/version.go` — `var Version = "X.Y.Z"`.
2. `cllm/Makefile` — `IMAGE ?= cllm:X.Y.Z`.
3. `clusters/z01/cllm/deployment.yaml` — pod `image: cllm:X.Y.Z`.
4. `cllm/README.md` — 4 occurrences in build/run sections.

Use `multi_replace_string_in_file` for a single batch.

Verify: `grep -rn 'cllm:X\.Y\.Z\|var Version' cllm clusters` returns ONLY the new version.

## Release sequence
```sh
# 1. Push dev
git checkout bartr && git push origin bartr

# 2. FF-merge bartr -> main (preserves full commit history; tag carries release notes)
git checkout main && git pull --ff-only
git merge --ff-only bartr
# If --ff-only refuses, rebase bartr onto main and retry. Do NOT fall back to squash.

# 3. Annotated tag — body is the release notes. WRITE TAG MESSAGE TO A FILE FIRST.
#    Inline -m with multi-line in zsh leaks \u00a7 escapes; always use -F.
cat > /tmp/tag-msg <<'EOF'
Version X.Y.Z: <headline>

- bullet
- bullet
EOF
git tag -a X.Y.Z -F /tmp/tag-msg

# 4. Push main + tag
git push origin main X.Y.Z

# 5. Reset bartr -> main, bump every touchpoint to X.(Y+1).0
git checkout -B bartr main
# (apply 4 edits via multi_replace_string_in_file)
go test ./...
git commit -am "bump dev version to X.(Y+1).0"
git push --force-with-lease origin bartr
```

## Deploy + verify (BLOCKING)
```sh
cd /home/bartr/vllm/cllm && make deploy
kubectl -n cllm rollout status deployment/cllm
# RELEASE-PROCESS TRAP: make deploy does NOT kubectl apply -k.
# If the manifest's image tag changed, you MUST also run:
kubectl apply -k /home/bartr/vllm/clusters/z01/cllm/
curl -fs http://localhost:8088/version    # must show X.(Y+1).0
make install
~/go/bin/ask --version                    # must show X.(Y+1).0
~/go/bin/ask --files /home/bartr/vllm/scripts/smoke-test.yaml --bench 1
```

## Post-release housekeeping
- Update `/memories/repo/cllm.md` "Latest release" + "Last verified" lines to the new shas/versions.
- `cd /home/bartr/vllm/cllm && make clean-images` to drop the previous tag from k3s ctr + docker.

## Output format
After completion, report:
- New tag (with `git show X.Y.Z` summary).
- `/version` returned vs. expected.
- Smoke test result (pass / which fixture failed).
- Files modified (touchpoints + any drift fixes).
