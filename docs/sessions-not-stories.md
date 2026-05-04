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

## Greenfield cadence — staying current as an FDE

The session model implies a corollary for individual development:
**one greenfield project per half, budgeted at ~10–12 sessions.**

Why it matters:
- The landscape moves fast. Six months of customer work without a net-new build quietly erodes the instincts HVE depends on.
- Greenfield is where you encounter new stacks, new domains, and new failure modes — the raw material that makes your next customer engagement sharper.
- 10–12 sessions is small enough to protect on any calendar, and large enough to produce real, shippable evidence (cLLM is proof).

In session terms: this is one arc, not a project. Frame it, time-box it, ship something real, write the HVE story. Then bring that back to customers.

## Code review in the session model

Code review is one of the biggest unsolved friction points in HVE adoption. The current model treats it as an async interrupt — a notification queue that pulls engineers out of their own sessions to context-switch into someone else's code. That is the worst possible review experience, and it does not get better just because the code was written faster.

The session model suggests a different approach:

**Review is a session, not a queue.**
A dedicated 30–60 minute review session — scoped, focused, uninterrupted — produces better signal than three fragmented 10-minute drive-bys. Budget it that way. Block the time. Treat a mis-framed or under-prepared review request the same way you treat a mis-framed session: send it back.

**FF-merge + session hygiene changes the review surface.**
If the author ends every session with green tests, a coherent diff, a design note, and a tag — the reviewer's job is fundamentally different. You are not reconstructing intent from a squashed blob. You are reviewing a session's worth of deliberate, documented work. The diff tells a story because the session was shaped to tell one.

**Repo memory as pre-read.**
If the author updated repo memory at session close, the reviewer can orient in minutes. The "what is this repo even doing" tax — which dominates most review time on unfamiliar code — largely disappears. This is a structural speedup, not a nice-to-have.

**The open question: who reviews, and when?**
If sessions compound and repo memory carries context forward, does every session need a synchronous human review — or is review better reserved for arc boundaries and customer-facing changes? Low-risk, well-tested, well-documented sessions may self-review via the artifact they produce. Higher-stakes arc completions warrant a dedicated review session from a second engineer. This needs experimentation, not a policy.

**What to track (if experimenting):**
- Review lag: time from session close to review complete
- Review session length vs. review quality (bugs caught, design feedback given)
- Re-work rate: sessions that required a follow-up session due to review feedback

## Ceremonies as sessions

Most Agile ceremonies are already session-shaped. They have defined scope, a natural exit artifact, and a time box. They have just accumulated overhead that makes them feel bigger than they are — largely because the information they exist to surface was never written down anywhere else.

HVE changes that. Repo memory, design docs, FF-merge, and tags make most ceremony inputs ambient. The ceremonies do not disappear — they compress.

### Sprint retro → reflection session

The cleanest mapping. Fixed scope (what happened this arc), clear artifact (a committed change to the team's working agreement), natural time box (60–90 min).

The HVE version:
- Everyone pre-reads the git history, session log, and arc doc *before* the meeting
- The retro itself is pure synthesis and decision — no reconstruction, no surprises
- It ends with a written, committed process change — not a list of feelings on a board

Exit artifact: a pull request to the team's working agreement or process doc. If there is no artifact, the session did not close.

### Daily standup → synchronized code review window

The standup's real job is "are we blocked, and does anyone need help?" A shared daily CR window does that — and produces value at the same time.

How it works:
- A fixed daily window (e.g. 9–9:45am) where everyone is doing code review
- Blockers surface naturally — you are all in the same cognitive space at the same time
- Questions get answered on demand; bigger issues pull in the wider team immediately
- No status theater. The git log is the status.

Design question worth experimenting with: true synchronous block (everyone reviewing together, on a call) vs. shared time window (everyone reviewing independently, channel open for questions). The latter scales better across time zones.

Exit artifact: reviewed and merged (or returned) sessions from the prior day.

### Sprint planning → arc framing session

Planning is bloated today because estimating in points requires negotiation. Replace points with sessions and planning becomes:
- Here is the arc goal
- Here are the candidate sessions, in order
- Here is what done looks like
- Here is what is explicitly out of scope

One engineer + one PM, 60–90 minutes, produces a framed arc ready to execute. The rest of the team does not need to be in the room — they read the arc doc.

Exit artifact: a committed arc framing doc with goal, session list, exit criteria, and explicit out-of-scope decisions.

### Sprint demo → tag-driven release notes

This one nearly eliminates itself. If every session ends with a tag and release notes, the "demo" is reading the changelog. The information is already ambient.

What survives:
- A monthly or arc-boundary stakeholder demo still makes sense for visibility
- The weekly "show what you built" meeting becomes redundant when artifacts speak for themselves

Exit artifact: the release notes are the artifact. The demo, if it happens, is a presentation of what is already written — not a substitute for writing it.

### Office hours — connective tissue between sessions

Office hours are not a ceremony. They are the intentional space where cross-session questions get resolved without breaking anyone's flow.

Two flavors, same model:

**Dev team office hours.**
A standing weekly session — open, time-boxed (60 min), no agenda required. The critical property: attendance is *pull*, not *push*. If your sessions are going cleanly and repo memory is current, you may not need it that week. But it exists as a safety valve for cross-session blockers, architecture questions that span more than one engineer's arc, and the "I've been stuck for two sessions" signal that something needs to be talked through, not just coded through.

Optional attendance is what gives it value. A week where nobody shows up is a good week, not a failed meeting.

**Stakeholder office hours.**
A shorter standing window (30 min, weekly or bi-weekly) that gives business stakeholders a predictable on-ramp without ceremony overhead. Right now stakeholders either wait for a demo or interrupt engineers ad hoc. A known open window decouples "can I ask a question" from "is there a scheduled review." It also naturally filters urgency — if it can wait until Thursday office hours, it probably was not urgent enough to break a session.

The session properties hold: fixed time box, pull-based attendance, clear purpose, light exit artifact (a brief note of what was discussed and any decisions made).

Office hours attendance is itself a health signal. Heavy attendance means sessions are not well-framed or blockers are not surfacing early enough. Light attendance means the system is working.

### The underlying pattern

Ceremonies exist to compensate for information that is not written down.

- Standups exist because nobody knows what anyone else is doing
- Planning is long because intent was never documented
- Retros are fuzzy because the arc was not framed clearly at the start
- Demos are required because there are no release notes

HVE + session hygiene makes most of that information ambient *before* the ceremony starts. The result is not fewer ceremonies — it is ceremonies that actually fit in a session, because the pre-work is already done.

**Hypothesis worth testing:** a team running full session hygiene should be able to cut total ceremony time by 50–70% within one quarter, with no loss of coordination or visibility — and measurable gains in both.

## Next step (when ready)

Frame a small experiment with one or two willing teams:
1. Run a quarter where capacity is planned in sessions, not points.
2. Track sessions-per-week, sessions-to-first-demo, sessions-to-ship.
3. Compare predictability and customer-visible delivery rate against the prior quarter.
4. Decide whether to roll out, refine, or roll back.
