# HVE Presentation: Helium vs cLLM — 30-Minute Talk + Q&A

Audience: CVP, partner FDEs, principal FDEs (same as `cllm-demo.md`)
Scope: Standalone HVE talk. cLLM is the case study, not the subject.
Format: 30 minutes talk + 15–20 minutes Q&A.

## Core Message

> Helium took 4 of us 26 weeks to ship an MVP. Five years later, with GitHub Copilot and HVE, I built a comparable MVP — same engineering bar, different domain — in 4 days, alone.
>
> cLLM is **an evolution of Helium**, not a competitor. The service is different, so the logs and metrics and dashboards are different. The fundamentals are the same. The shock is **how fast the fundamentals come back together** with HVE.
>
> HVE is no longer a hypothesis. It is the evidence.

This talk is not "AI built it for me." It is: an experienced FDE, equipped with HVE practices and Copilot, replicates the engineering scaffolding of a 4-person, 26-week project in days — with race-clean tests, written design docs, evidence-backed reports, and live observability from day one.

## What I Want Them to Leave With

1. HVE delivers an order-of-magnitude shift in time-to-MVP — and we now have side-by-side evidence with a project people in this room remember.
2. HVE amplifies senior engineers; it does not replace engineering judgment. Experience is what makes the 4-day number real.
3. Adoption is no longer optional inside the FDE org — this is the new floor for what one engineer can ship in a week.

## The Honest Comparison (the slide everyone will photograph)

| Axis | Helium (2020-ish) | cLLM (Apr–May 2026) |
|---|---|---|
| Time to MVP | **26 weeks** (6 months) | **4 days** of focused work |
| Team | 1 Partner FDE + 1 Principal FDE + 1 Senior FDE + 1 Principal PM | 1 Partner FDE |
| People-weeks | **~104** | **~0.8** |
| Reduction | — | **~130×** less effort |
| Stack experience at start | New to K8s, Prometheus, Grafana, GitOps | 5 yrs each, top-tier K8s FDE |
| Domain experience | First time building this kind of service | Nth Helium-shaped project |
| Net-new for me | — | GPUs on K8s, vLLM (had never heard of it) |
| Tooling | Hand-written, Stack Overflow, docs | GitHub Copilot + HVE workflow |

## Apples-to-Apples Feature Checklist

Same engineering bar Helium set. Both columns are "shipped and demoable." The services solve different problems — Helium was an HTTP API workload generator and SRE tool; cLLM is an LLM inference experimentation platform — so the *shape* of logs, metrics, and dashboards differs. The **fundamentals checklist** is what we are comparing.

| Capability | Helium 26 wk | cLLM 4 d |
|---|---|---|
| Production-style service | ✅ | ✅ `cllm` Go server (different domain) |
| Test client | ✅ | ✅ `ask` CLI |
| Benchmark client | ✅ | ✅ `ask --bench` w/ ramp, mixed prompts |
| Structured logging | ✅ | ✅ correlation IDs end-to-end |
| Prometheus metrics | ✅ | ✅ /metrics, custom + standard |
| Live benchmark dashboard | ✅ | ✅ Grafana, 3-layer (cLLM / vLLM / GPU) |
| GitOps (Flux on K8s) | ✅ | ✅ `clusters/z01/` Flux-managed |
| Race-clean test suite | partial | ✅ `go test -race` green |

Domain-specific cLLM capabilities (token admission, multi-node routing, cache-replay synthetic fleet, MCP control plane) are not part of the apples-to-apples comparison — they exist because cLLM is a different service. They are not "Helium plus"; they are "different problem."

Helium had its own depth that cLLM does not: NGSA, Walmart production, the SRE tooling around it, and the Triplet Strategy. **That depth came from years of customer use.** cLLM is three weeks old. The fair claim is that cLLM is on the same trajectory, with the engineering fundamentals shipped in days instead of months.

## Quantified Evidence (real numbers from this repo)

Pulled from git and `wc -l` on April 18 → May 2:

- **125 commits** across 12 calendar days; **99 commits in the first 5 days** (the "4-day MVP" window).
- **~19.5K lines of Go** in `cllm/`, **~7.8K lines of Go tests** (≈ 40% test ratio).
- **~1.6K lines of Python** (MCP server).
- **~8.3K lines of K8s YAML** (Flux-managed cluster).
- **~6.2K lines of Markdown** — design docs, ADR-style docs, scenario reports.
- **5 design docs** in `docs/` (cost admission, memory pressure, multi-node, phase-aware allocation, release process) — co-written with Copilot.
- **5 scenarios + 5 evidence reports** in `benchmark/`.

This is the slide that ends the "but is it real?" conversation.

## Presentation Shape

| Time | Segment | Purpose |
|---:|---|---|
| 0:00 – 2:00 | Opening: the bet | Frame HVE as the question, Helium vs cLLM as the answer |
| 2:00 – 5:00 | Helium baseline | Honor the team and the work; set the 26-week / 4-person bar |
| 5:00 – 9:00 | The comparison slide | Side-by-side table, then the 130× reduction reveal |
| 9:00 – 13:00 | What changed | Four forces: Copilot+HVE, reuse, experience, repeat domain |
| 13:00 – 16:00 | **How this actually happened** | Organic origin story — vLLM → ask → bench → cLLM → DSL → MCP |
| 16:00 – 24:00 | Live demo | MCP-driven cLLM benchmark, end-to-end |
| 24:00 – 26:00 | Evidence + the stack | Numbers + breadth of what shipped |
| 26:00 – 28:00 | **Lessons learned / IP to harvest** | Process, tooling, Copilot, mindset — take-home value |
| 28:00 – 29:00 | Caveats (briefly, on purpose) | Honest about seniority, reuse, repeat domain |
| 29:00 – 30:00 | Call to action | HVE is the new floor. Adopt the practice. |
| 30:00 – 50:00 | Q&A | Productization, fidelity, where HVE breaks |

## Segment-by-Segment Talk Track

### 0:00 – 2:00 — Opening: The Bet

Open cold with the comparison number. No throat-clearing.

> Five years ago, four of us — two principals, a senior, and a PM — spent 26 weeks shipping the first good release of Helium. Most of you remember it. Several of you helped build it.
>
> Three weeks ago, alone, with Copilot and an HVE workflow, I shipped a comparable MVP in four days — a service, benchmark client, structured logs, Prometheus, live Grafana, GitOps. Same bar Helium set. Then I kept going.
>
> That is what I want to show you today. Not a demo of a new product. A demo of what one experienced FDE can now ship in a week.

### 2:00 – 5:00 — Helium Baseline (honor the work)

This segment is important. Do not minimize Helium. The credibility of the comparison depends on respecting the original. Name the team on the slide — by name, with respect.

The Helium core team:

- **Bart** — Partner FDE today (Principal at the time of Helium) — me
- **Joseph** — Principal FDE
- **Anne** — Senior FDE
- **Deanna** — Principal PM

> Helium was a huge success and a hugely valuable tool. It grew into **NGSA**, was deployed to production at **Walmart** for their SRE team, and anchored Walmart's **Triplet Strategy**. The team that built it — Joseph, Anne, Deanna, and me — is still doing some of the best work in the company.
>
> The team agreed: 6 months MVE, 7 months MVP, 8 months release. Four people. Most of us were new to Kubernetes. New to Prometheus. New to Grafana. GitOps wasn't even mainstream yet.
>
> So when I say "26 weeks for an MVP" — that is not a slow team. That is a strong team learning a brand new stack while delivering it.

Land the point: **the 26-week number is the right baseline, not a strawman.**

### 5:00 – 9:00 — The Comparison Slide

Show the two tables above. Walk them slowly. Let the room read.

Then deliver the reveal:

> 104 people-weeks versus 0.8 people-weeks. Roughly **130 times less effort** for the same engineering bar.
>
> Important framing: cLLM is **an evolution of Helium**, not a competitor. Different service, different domain — so the logs and metrics and dashboards are different. The fundamentals are the same. The shock is how fast the fundamentals come back together with HVE.
>
> And to be fair: Helium earned its depth over years — NGSA, Walmart production, the SRE tooling, the Triplet Strategy. cLLM is three weeks old. We are not claiming cLLM has surpassed Helium. We are claiming **the engineering scaffolding now takes days, not months.**

Pause. This is the photograph slide.

### 9:00 – 13:00 — What Actually Changed

No live coding here. Keep this segment narrative — the only live demo in the deck is the MCP/cLLM run later. Be honest and structured. Four forces, in order of magnitude:

1. **GitHub Copilot + HVE workflow** — the largest single multiplier. Codifies best practices, error handling, and contracts into the loop. Generates ADRs alongside code. Turns design conversations into working prototypes inside one session.
2. **Reuse — it is in my DNA as a senior engineer.** I did not start cLLM from a blank repo. I started from the **K8s-on-the-edge + GitOps** stack I have been building for **Domino's** for the last year — cluster manifests, Flux layout, Prometheus and Grafana wiring, the deployment patterns. That is why the first 5 days produced 99 commits: the runway was already paved. **Reuse is not a shortcut. It is the craft.**
3. **Five years of compounding experience** — K8s, Prometheus, Grafana, GitOps are now muscle memory. I am considered one of the strongest K8s FDEs in Microsoft. That experience is what lets Copilot move at full speed without producing slop.
4. **Repeat domain** — I have built Helium-shaped systems multiple times. Pattern recognition is free velocity.

The honest framing:

> HVE does not work equally for every engineer on every project. It rewards experience. A senior engineer with HVE pulls 10× to 100×. A junior engineer with HVE still pulls real gains, but the multiplier is smaller. **HVE amplifies engineering judgment. It does not replace it.**

What was genuinely new to me — and Copilot still got me there in 4 days:

- GPUs on Kubernetes (DCGM exporter, NVIDIA device plugin, scheduling)
- vLLM (had never heard of it before this project)
- The whole LLM-serving cost model (tokens-as-resource, not requests-as-resource)

That is the strongest evidence of all: **the multiplier held even where I had no prior expertise.**

### 13:00 – 16:00 — How This Actually Happened

This is a critical narrative beat. **I did not set out to build "Helium 2."** I was just working how I normally work. Tell the story plainly — it is the most authentic moment in the deck.

> I want to walk you through how this actually came together, because nothing about it was planned as a comparison to anything. It was just an engineer solving the next blocker.
>
> 1. My goal was simple: **evaluate a private LLM server running on the edge.**
> 2. I started where any senior engineer would — **with what I already had.** The K8s-on-the-edge GitOps stack from my Domino's project. Already paved.
> 3. I did some research with Copilot and **chose vLLM.** Deployed it.
> 4. I needed a way to talk to it, so I wrote `ask` — a small CLI in **bash and Python.**
> 5. I needed a way to test it, so I wrote a **benchmark tool**, also in bash and Python.
> 6. At some point I realized those should be one tool, and they should be in **Go**, because I have built a number of Go CLIs and Go CLIs rock. Rewrote them.
> 7. I tested vLLM until **it hard-crashed.** Now I had a real envelope.
> 8. To scale tests further, I needed more GPUs. **And internal Azure GPUs are nearly impossible to get** — real customer demand eats them all. That blocker is what created cLLM.
> 9. I wanted cLLM to be a **synthetic GPU**, not a "cache replay" toy. That design choice is what made the rest of the work valuable.
> 10. The **DSL** emerged from real benchmarking needs. It has worked brilliantly.
> 11. The **MCP server** went on top. The win is no longer that *I* can run benchmarks. The win is that **anyone can.**
>
> Nothing here was planned as a Helium comparison. Each step solved the next real blocker. **That is what HVE looks like in practice — and that is why this story is worth telling.**

### 16:00 – 24:00 — Live Demo

Reuse the existing MCP-driven cLLM demo from `cllm-demo.md`. The framing is different here: we are not selling cLLM, we are showing what HVE produces.

**Honesty note for this segment:** the **cLLM service, benchmark client, structured logs, Prometheus metrics, Grafana dashboards, and GitOps** were all in the 4-day MVP. The **MCP control plane** is *not* part of the 4-day count — it was added later, in roughly **another half-day to a day** of HVE work on top. Call this out on stage so no one feels misled when they ask later.

Compressed flow (≈ 9 min):

1. **Inspect the fleet** — `list_nodes`, `get_benchmark_status`. Show topology + Grafana.
2. **Run a bounded benchmark** — `run_scenario` on `baseline.yaml`. Watch dashboard light up live.
3. **Add a synthetic node** — `create_synthetic_node` for an `rtx` lane. Show routing pick it up.
4. **Show MCP-generated report** — `summarize_experiment` writes a Markdown report.

Anchor steps 1–3 with: *"the cLLM side of this shipped in the 4-day MVP."* When step 4 lands, say: *"the MCP layer driving this was about a day on top — that is the second-order point: HVE compounds."*

### 24:00 – 26:00 — Evidence It Is Real (and the stack you just saw)

This is the "prove the 4 days" slide. Show the numbers from the **Quantified Evidence** section. Add screenshots of:

- `git log --oneline` first week (99 commits in 5 days).
- `docs/` directory listing — 5 design docs, none hand-waved.
- `benchmark/reports/` — 5 evidence-backed scenario reports.
- `go test -race ./...` green.

> Not a prototype. Not a sketch. Tests pass. Designs are written down. Reports back the claims.

### 26:00 – 28:00 — Lessons Learned / IP to Harvest

This is the take-home segment. Frame it as **gifts to the room**, not a retrospective. Move quickly — one sentence per bullet. Tell them they will get the slide.

**Process**

> Treat every project as an HVE case study from day one. Several of my early commits went straight to `main` — the story would be cleaner if I had branched and PR'd from the start, even working alone.
>
> The release process I documented in `docs/release-process.md` is gold — it forces good hygiene. **One lesson we already acted on:** we used to squash-merge at release. The side effect was that **a lot of individual commits became invisible** — each release collapsed into one. The HVE story we needed ("99 commits in the first 5 days," the day-by-day arc of admission, DSL, MCP) was being thrown away on `main`. **We switched to FF-merge** so the full history survives. Squash felt tidy; FF tells the truth.
>
> **Update repo memory at every release.** That practice came from asking Copilot how to get more out of Copilot — best meta-tip of the project.
>
> **Save your designs and plans.** I saved some; many are lost in chat history. Be disciplined.

**Tooling**

> The `ask` CLI is highly reusable — pull it out as standalone IP for any LLM-fronted project.
>
> Use the **Grafana API**, not ConfigMap dashboards — ConfigMap dashboards cannot be edited and saved live. The API can also auto-add dashboards to favorites.
>
> Use **Tombstone** to clear test metrics and keep dashboards tidy.

**Working with Copilot**

> Copilot had one major hallucination on this project — lost the plot of what we were building. **The tests and dashboards caught it early.** That is the safety net. If you do not have tests and dashboards, you do not have a safety net.
>
> Ask Copilot for suggestions on getting more value from Copilot. Compounding insight.

**Mindset**

> Don't be afraid to try things — **cut a release first** so `git revert` is one command away.
>
> Have fun. The speed is genuinely amazing. Let it be.
>
> **PMs get as much value from HVE as engineers — possibly more.** Bring them in. Deanna would have *loved* this.

### 28:00 – 29:00 — Caveats (brief, on purpose)

Four sentences, then move on. The room will respect that you said it; they will not respect a 5-minute disclaimer. **Be specific about seniority — honesty is the credibility lever.**

> Four honest caveats. One: I am one of the most senior ICs in this org — in a team of about 1,100 FDEs, there are 4 Partner-level ICs and 2 Distinguished Engineers, and I am one of those 6 — HVE rewards experience and I have a lot of it. Two: I have built Helium-shaped projects before — repeat domain helps. Three: I reused a year of K8s-on-the-edge and GitOps work from my Domino's project as the starting stack — reuse is in my DNA, and it should be in yours. Four: this is one project, not a fleet — we will get more data points as more FDEs adopt HVE.
>
> None of those caveats remove the 130× number. They explain it.

### 29:00 – 30:00 — Call to Action

> HVE is no longer the future. It is the new floor for what one experienced FDE can ship in a week. We are adopting it as standard practice in this org because the evidence is now overwhelming — and this is one more data point.
>
> If you are an FDE and you are not yet running an HVE workflow daily, you are leaving an order of magnitude on the table. That is the message.

Close.

## CVP-Level Talking Points (for Dan)

Dan cares about: **HVE adoption, time-to-customer-value, observability, engineering fundamentals, AI adoption.** Each is already in the deck — these are the moments to lean in and address him directly.

| Theme | Where it lands in the deck | One-line for Dan |
|---|---|---|
| **HVE adoption** | Call to action (28:00) and Helium baseline (2:00) | "This is the evidence we needed to make HVE the floor, not the ceiling, for FDE delivery." |
| **Time-to-customer-value** | Comparison slide (6:00) and 130× reveal | "104 people-weeks to 0.8 people-weeks means we get to customer value in days, not quarters — without dropping the engineering bar." |
| **Observability** | Feature checklist (6:00) and live demo (14:00) | "Three-layer observability — cLLM, vLLM, GPU — was day-one, not week-twenty. Customers see what we see, immediately." |
| **Engineering fundamentals** | Evidence slide (23:00) | "Race-clean tests, design docs, evidence-backed reports, GitOps. HVE *raises* the fundamentals bar; it does not lower it." |
| **AI adoption** | What changed (10:00) and demo (14:00) | "Copilot wrote alongside me. MCP lets Copilot *operate* the system. AI is in the build loop and in the run loop — that is the adoption pattern we want field-wide." |

When Dan is in the room, glance at him at minute 9 (the 130× reveal) and minute 28 (the CTA). Those are his slides.

## Q&A Preparation (15–20 min)

Anticipate and pre-answer:

| Likely question | Short answer |
|---|---|
| Would a junior engineer hit 130×? | No. They would still gain meaningfully — probably 3–10×. The multiplier scales with judgment. |
| Is the 4-day code production-grade? | Test-passing, race-clean, GitOps-deployed, evidence-reported. Not yet hardened for multi-tenant prod, but neither was Helium at week 26. |
| What about hallucinations / wrong code? | Caught by tests, by review-as-you-go, and by the HVE habit of writing the design doc with Copilot before the code. |
| Does this kill engineering jobs? | No — it raises the floor of what one engineer is expected to ship. Same arc as compilers, IDEs, and cloud. |
| Can we replicate this for *real* customer work? | Yes — this is the case for adopting HVE org-wide. cLLM is the proof. |
| What did Copilot get wrong? | Pull a real example or two from your commit history during prep — credibility booster. |
| Will this work for greenfield AI/ML research? | HVE accelerates engineering scaffolding around research. The novel research itself still needs human insight. |

## Slide List (15 slides)

1. Title: "Helium vs cLLM — Evidence for HVE"
2. The bet: 26 weeks vs 4 days
3. Helium baseline (honor the team, NGSA, Walmart, Triplet Strategy)
4. The comparison table (axis)
5. The feature checklist (same engineering bar, different domain)
6. The 130× number — evolution, not superset
7. What actually changed (4 forces: Copilot+HVE, reuse, experience, repeat domain)
8. The strongest evidence (GPUs, vLLM, cost model — net-new)
9. **How this actually happened** (organic origin story: vLLM → ask → bench → cLLM → DSL → MCP)
10. Live demo — MCP-driven cLLM
11. Evidence it is real (git, LOC, tests, docs, reports)
12. What that buys you — the stack
13. **Lessons learned — IP to harvest** (process, tooling, Copilot, mindset)
14. Caveats — owned, not hidden
15. Why it matters / call to action — HVE is the new floor
16. Q&A

## Pre-Talk Checklist

- [ ] Cluster healthy: `kubectl get pods -A` clean
- [ ] Grafana cLLM dashboard loaded and live
- [ ] MCP server smoke-tested with Claude/Copilot Chat
- [ ] `git log --oneline | head -100` screenshot ready
- [ ] `go test -race ./...` run fresh, output saved
- [ ] One Copilot-got-it-wrong anecdote rehearsed for Q&A
- [ ] Backup pre-recorded demo clip in case the live cluster wobbles

## Decisions Locked In

- **Helium team is named** on the baseline slide: Bart (Partner FDE today; Principal at the time), Joseph (Principal FDE), Anne (Senior FDE), Deanna (Principal PM).
- **Live demo is MCP-only.** No on-stage Copilot coding moment. The "what changed" segment stays narrative.
- **CVP-level emphasis** (for Dan): HVE adoption, time-to-customer-value, observability, engineering fundamentals, AI adoption. Mapped to specific deck moments in the *CVP-Level Talking Points* section above.
