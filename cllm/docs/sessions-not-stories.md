# Sessions, not Stories — Notes for Future Follow-up

> Saved from a working conversation while preparing the HVE presentation, May 2026.
> This is exploratory thinking, not a finished position. Pick it up when the talk lands and there is appetite to formalize.

## The shift in primitive

Old unit of planning: **a story** — sized in points, scoped to fit a sprint, accepted via a demo.
HVE unit of planning: **a session** — sized in *focus minutes*, scoped to "what one engineer + Copilot can ship before context drift," accepted via tests + commit + (often) a release.

A session has properties a story doesn't:

- **It is bounded by cognitive context, not calendar time.** A 90-minute session with a clean repo and a sharp goal beats two days of fragmented work. The constraint is *attention*, not *capacity*.
- **It produces a coherent artifact.** Code + tests + design notes + a release commit, ideally. Sessions that don't produce all four are a smell.
- **It is replayable.** The chat transcript, repo memory, and git history are the audit trail. Stories were never replayable; sessions are.
- **It compounds.** What repo memory captured this session makes the next session faster. Stories don't compound; backlogs just get longer.

## What this changes for each role

### Developers

- Plan in sessions, not tickets. A "release-able session" becomes the natural commit unit.
- The dot-release-per-session rhythm is the pattern. Codify it: every session ends with green tests, FF-merge, tag, repo-memory update.
- Context hygiene becomes a first-class skill: prepping the session (repo memory, design doc, scope) is now part of the work, not overhead.

### PMs

- Stop estimating in points. Estimate in **sessions to first demo** and **sessions to ship**. Way more honest.
- The PM's job shifts from "decompose into stories" to "frame the session" — goals, constraints, exit criteria, what *not* to do. That is a higher-leverage skill, and a great fit for HVE.
- Roadmaps become "this quarter, we expect ~N session-equivalents of capacity per engineer" — testable against actuals and self-correcting.

### Planning

- Velocity becomes **sessions-per-week to value** — a number you can actually feel.
- Risk shifts from "did we estimate right?" to "did we frame right?" Mis-framed sessions waste a session; mis-estimated stories used to waste a sprint. Smaller blast radius, faster correction.
- Cross-team dependencies get easier: "I need 1 session of your time on X" is much clearer than "a 5-point story."

## What's tricky

- **Sessions are personal.** My session ≠ a junior engineer's session. The unit does not natively normalize across people. (Stories had the same problem; we just pretended they didn't.)
- **Big things still take many sessions.** We need a "campaign" or "arc" concept above sessions — basically what an epic was, but composed of sessions and explicit memory checkpoints between them.
- **Ceremonies need to change.** Daily standup → weekly session retro. Sprint planning → arc framing. Demos → tag-driven release notes.
- **Reporting up.** Leadership wants quarter-level numbers; sessions roll up awkwardly. We will need a translation layer: *X sessions = Y dot releases = Z customer-visible features.*

## Open questions

- What is a sensible session-length norm? (Hypothesis: 60–120 minutes, gated by attention not clock.)
- Is "release-able session" the right exit bar, or too strict for early-arc work?
- How do we represent arcs / campaigns without rebuilding the epic-and-story machinery we are trying to replace?
- How do PMs and engineers co-frame a session without re-introducing ticket overhead?
- What does this look like for non-greenfield work (large existing codebases, legacy)?

## Connection back to this project

cLLM is a working proof of the model:
- Most days produced one or more **release-able sessions** ending in `make deploy` + smoke + tag.
- The **dot-release rhythm** matches the session rhythm almost 1:1.
- **Repo memory** (`/memories/repo/*.md`) is the cross-session compounding mechanism — it works.
- The **switch from squash to FF-merge** during this very session is itself an example: the unit of decision was "this session, change the policy and capture the rationale," not "open a ticket, schedule it next sprint."

## Next step (when ready)

Frame a small experiment with one or two willing teams:
1. Run a quarter where capacity is planned in sessions, not points.
2. Track sessions-per-week, sessions-to-first-demo, sessions-to-ship.
3. Compare predictability and customer-visible delivery rate against the prior quarter.
4. Decide whether to roll out, refine, or roll back.
