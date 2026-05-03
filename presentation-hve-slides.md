---
marp: true
theme: default
paginate: true
size: 16:9
header: 'Helium vs cLLM — Evidence for HVE'
footer: 'Bart Robertson · Partner FDE · May 2026'
style: |
  section {
    font-size: 24px;
    padding: 36px 60px;
  }
  section.title {
    text-align: center;
    justify-content: center;
  }
  section.title h1 {
    font-size: 64px;
    margin-bottom: 0.2em;
  }
  section.title h2 {
    font-size: 36px;
    color: #555;
    font-weight: 400;
  }
  section.big {
    text-align: center;
    justify-content: center;
  }
  section.big h1 {
    font-size: 88px;
  }
  section.big h2 {
    font-size: 44px;
    color: #555;
  }
  section.dense {
    font-size: 20px;
    padding: 28px 50px;
  }
  section.dense h1 { margin: 0 0 0.3em 0; }
  section.dense ol, section.dense ul { margin: 0.3em 0; }
  section.dense li { margin: 0.15em 0; }
  h1 { color: #0b5394; margin-top: 0; }
  h2 { color: #333; }
  table {
    font-size: 22px;
    margin: 0 auto;
  }
  th { background: #0b5394; color: white; }
  tr:nth-child(even) { background: #f4f7fb; }
  strong { color: #0b5394; }
  blockquote {
    border-left: 4px solid #0b5394;
    color: #333;
    font-style: italic;
    margin: 0.5em 0;
  }
  code { background: #eef3f9; padding: 2px 6px; border-radius: 4px; }
  .small { font-size: 18px; color: #666; }
  .check { color: #2e7d32; font-weight: bold; }
  .x { color: #999; }
---

<!-- _class: title -->
<!-- _paginate: false -->
<!-- _header: '' -->

# Helium vs cLLM
## Evidence for Hypervelocity Engineering

Bart Robertson · Partner FDE
May 2026

---

<!-- _class: big -->

# 26 weeks. → 4 days.

## Same MVP bar. One engineer. Real evidence.

---

# The Bet

> Five years ago, **four of us** spent **26 weeks** shipping the first good release of Helium.
>
> Three weeks ago, **alone**, with **Copilot and an HVE workflow**, I shipped a comparable MVP in **four days** — same engineering bar, different domain.

Today's talk is not "AI built it for me."

It is: **what one experienced FDE can now ship in a week.**

---

# Honoring Helium

The core team that shipped Helium:

- **Bart** — Partner FDE today (Principal at the time)
- **Joseph** — Principal FDE
- **Anne** — Principal FDE (Senior at the time)
- **Deanna** — Principal PM

What we delivered: **6 mo MVP · 7 mo release**

What we were: **all new to Kubernetes, Prometheus, Grafana, GitOps**

> Helium → **NGSA** → deployed to production at **Walmart** for the SRE team → anchored Walmart's **Triplet Strategy**.
> Hugely valuable, hugely successful. The 26-week number is the **right baseline**, not a strawman.

---

# The Apples-to-Apples Comparison

| Axis | Helium (2020-ish) | cLLM (Apr–May 2026) |
|---|---|---|
| Time to MVP | **26 weeks** | **4 days** |
| Team | 2 Principal FDE + 1 Senior FDE + 1 Principal PM | 1 Partner FDE |
| **People-weeks** | **~104** | **~0.8** |
| Stack experience at start | New to K8s, Prom, Grafana, GitOps | 5 yrs each, top-tier K8s FDE |
| Domain experience | First time | Nth Helium-shaped project |
| Net-new domain for me | — | GPUs on K8s, vLLM |
| Tooling | Hand-written, docs, Stack Overflow | **GitHub Copilot + HVE** |

---

# Same Engineering Bar. Different Domain.

cLLM is **an evolution of Helium**, not a competitor to it.
The service is different — so the logs, metrics, and dashboards are different.
The **engineering fundamentals** are the same.

| Capability | Helium 26 wk | cLLM 4 d |
|---|:---:|:---:|
| Production-style service | <span class="check">✓</span> | <span class="check">✓</span> (different domain) |
| Test client | <span class="check">✓</span> | <span class="check">✓</span> |
| Benchmark client | <span class="check">✓</span> | <span class="check">✓</span> |
| Structured logging | <span class="check">✓</span> | <span class="check">✓</span> |
| Prometheus metrics | <span class="check">✓</span> | <span class="check">✓</span> |
| Live Grafana dashboard | <span class="check">✓</span> | <span class="check">✓</span> (3-layer) |
| GitOps (Flux on K8s) | <span class="check">✓</span> | <span class="check">✓</span> |
| Race-clean test suite | n/a | <span class="check">✓</span> |

<span class="small">Helium grew into NGSA and ran in Walmart production. cLLM is on the same arc, three weeks in.</span>

---

<!-- _class: big -->

# ~130×

## less effort, same engineering bar
### cLLM is an **evolution** of Helium — a different service, with the same fundamentals shipped in days

<span class="small">104 FDE-weeks → 0.8 FDE-weeks</span>

---

<!-- _class: dense -->

# What Actually Changed

Four forces, in order of magnitude:

1. **GitHub Copilot + HVE workflow** — the largest single multiplier. Codifies best practices, error handling, and contracts into the loop. ADRs alongside code. Design conversations become working prototypes in one session.

2. **Reuse — it's in my DNA.** I started from the **K8s-on-the-edge + GitOps** stack I have spent the **last year building for Domino's**. Cluster manifests, Flux layout, Prometheus + Grafana wiring — all reused. *That* is why the first 5 days produced 99 commits.

3. **Five years of compounding experience** — K8s, Prometheus, Grafana, GitOps are now muscle memory. That is what lets Copilot move at full speed without producing slop.

4. **Repeat domain** — I have built Helium-shaped systems multiple times. Pattern recognition is free velocity.

> **HVE amplifies engineering judgment. It does not replace it.**
> **Senior engineers reuse on purpose** — that is not a shortcut, it is the craft.

---

# The Strongest Evidence

What was **genuinely new** to me on this project — and Copilot still got me there in 4 days:

- **GPUs on Kubernetes** — DCGM exporter, NVIDIA device plugin, scheduling
- **vLLM** — had never heard of it before this project
- **The LLM serving cost model** — tokens-as-resource, not requests-as-resource

cLLM's synthetic-GPU model captures the behaviors that actually matter:

- **Prefill cost** — the up-front compute spike before the first token
- **KV-cache pressure** — memory drives admission, not request count
- **Concurrent execution loop** vs. **waiting loop** — what runs now, what queues
- **Concurrency-based decay** — per-request throughput degrades as load rises
- **Jitter and stall** — real serving is not smooth; the model is not either

> The multiplier held **even where I had no prior expertise.**

---

# How This Actually Happened (1/2)

I did not set out to build "Helium 2." I set out to **evaluate a private LLM server on the edge.**

1. **Started from the Domino's edge stack** — K8s, GitOps, Prometheus, Grafana. Already paved.
2. **Research with Copilot** → chose **vLLM**. Deployed it.
3. Built **`ask`** in bash/python — needed a way to talk to it.
4. Built a **benchmark** tool in bash/python — needed a way to test it.
5. Combined both, **rewrote in Go**. Go CLIs rock.

<span class="small">Continued →</span>

---

# How This Actually Happened (2/2)

6. **Pushed vLLM until it hard-crashed.** Now I had a real envelope.
7. To scale tests I needed more GPUs. **Internal Azure GPUs are nearly impossible to get** — real customers eat them all.
8. **cLLM was born** — a *synthetic GPU lane*, not a cache-replay toy.
9. **The DSL emerged** from real benchmarking needs. It has worked brilliantly.
10. **MCP server** went on top. Now **anyone** can run benchmarks, not just me.

> Nothing here was planned. Each step solved the next real blocker. **That is what HVE looks like in practice.**

---

# Live Demo — MCP-Driven cLLM

The **cLLM service, benchmark, dashboards, and GitOps** you are about to see — **shipped in the 4-day MVP.**

The **MCP control plane** came a little later: another **half-day to a day** of HVE work on top.

1. **Inspect the fleet** — `list_nodes`, `get_benchmark_status`
2. **Run a bounded benchmark** — `run_scenario baseline.yaml`
3. **Add a synthetic node** — `create_synthetic_node rtx`
4. **Generate the report** — `summarize_experiment`

<span class="small">Switch to terminal + Grafana · MCP itself is a ~1-day add-on, not part of the 4-day baseline</span>

---

# Evidence It Is Real

Real numbers from this repo, April 18 → May 2:

- **125 commits** in 12 days · **99 commits in the first 5 days**
- **~19.5K lines of Go** · **~7.8K lines of Go tests** (≈40% test ratio)
- **~1.6K lines of Python** (MCP server)
- **~8.3K lines of K8s YAML** (Flux-managed)
- **~6.2K lines of Markdown** — design docs, ADRs, evidence reports
- **5 design docs** in `cllm/docs/`, co-written with Copilot
- **5 scenarios + 5 evidence reports** in `benchmark/`
- **`go test -race ./...`** — green

> Not a prototype. Not a sketch. **Tests pass. Designs are written down. Reports back the claims.**

---

# What That Buys You — The Stack

All of this came together in days, not quarters:

- **`cllm`** — Go server, Chat Completions–compatible proxy, **token-based admission control**
- **`ask`** — Go CLI, single-shot **+ benchmark client** with ramp and mixed prompts
- **MCP server** — Python / FastMCP; lets Copilot and Claude **operate the fleet** (the +½–1 day)
- **Cluster** — K3s, **Flux GitOps**, Prometheus, Grafana, DCGM, NVIDIA device plugin
- **Real lane** — vLLM on a real GPU; protected, never mutated by MCP
- **Synthetic lanes** — cache-replay nodes calibrated to vLLM-like behavior

**Three-layer observability shipped on day one:**
**cLLM** (control plane) · **vLLM** (serving) · **GPU** (hardware)

<span class="small">This is the slide that answers "how much is *really* in here?" — a lot.</span>

---

# Lessons Learned — Process & Tooling

**Process**

- **Treat every project as an HVE case study from day one.** Be disciplined on Git practices so the story is recoverable. I wasn't at first and lost storytelling evidence. AI is great at analyzing Git history and building an HVE story.
- **`release-process.md` is gold.** Forces good hygiene. We *used* to squash-merge — it lost individual commits and erased a lot of the HVE story. **We switched to FF-merge** so the full development arc survives on `main`.
- **Update repo memory at every release.** Came from asking Copilot how to get more out of Copilot.
- **Save your designs and plans.** I saved some; many are lost in chat history. Use a process and stay disciplined.

**Tooling**

- **`ask` is highly reusable.** Pull it out as standalone IP for any LLM-fronted project.
- **Use the Grafana API, not ConfigMap dashboards.** ConfigMaps cannot be edited and saved. Bonus: the API can auto-add to favorites.
- **Use Tombstone** to clear test metrics and keep dashboards tidy.

---

# Lessons Learned — Copilot & Mindset

**Working with Copilot**

- **Copilot hallucinated badly once** and lost the plot of what we were building. **Tests and dashboards caught it early.** That is the safety net.
- **Ask Copilot how to use Copilot better.** That is how repo memory entered the release cycle.

**Mindset**

- **Don't be afraid to try things — cut a release first** so `git revert` is one command away.
- **Have fun.** The speed is genuinely amazing. Let it be.
- **PMs get as much value from HVE as engineers — maybe more.** Bring them in.

---

# Caveats — Owned, Not Hidden

Four honest caveats:

1. **I am one of the most senior ICs in our org.** HVE rewards experience — and I have a lot of it.
2. **I have built Helium-shaped projects before.** Repeat domain helps.
3. **I reused a year of K8s/GitOps work** from my Domino's edge project as the starting stack. *Reuse is in my DNA — and it should be in yours.*
4. **This is one project, not a fleet.** More data points coming as more FDEs adopt HVE.

> None of those caveats remove the 130× number. **They explain it.**

---

# Why It Matters

| Theme | What this proves |
|---|---|
| **HVE adoption** | The evidence is in. HVE is the floor, not the ceiling. |
| **Time-to-customer-value** | 104 people-weeks → 0.8. Customer value in **days, not quarters.** |
| **Observability** | 3-layer observability shipped **day one**, not week 20. |
| **Engineering fundamentals** | Race-clean tests, design docs, evidence reports, GitOps. HVE **raises** the fundamentals bar. |
| **AI adoption** | Copilot in the **build loop** (code) and the **run loop** (MCP). |

---

<!-- _class: big -->

# HVE is the new floor.

## Not the future. The floor.

### If you are an FDE not running HVE daily,
### you are leaving an order of magnitude on the table.

---

<!-- _class: title -->

# Q&A

15–20 minutes

<span class="small">Productization · fidelity · where HVE breaks · what Copilot got wrong</span>

---

<!-- _class: dense -->

# Backup: Sessions, Not Stories

If HVE is real, **the unit of planning has to change.**

| Old primitive: **the story** | New primitive: **the session** |
|---|---|
| Sized in points | Sized in *focus minutes* |
| Bounded by sprint calendar | Bounded by cognitive context |
| Accepted via demo | Accepted via tests + tag + repo-memory |
| Not replayable | **Replayable** — chat, git, memory |
| Backlog grows | **Compounds** — each session paves the next |

**Developers** — plan in release-able sessions. Every session ends green: tests, FF-merge, tag, memory update.
**PMs** — stop estimating in points. **Frame the session** (goal, constraints, what *not* to do). Higher leverage, not lower.
**Planning** — velocity becomes *sessions-per-week to value*. Mis-framed wastes a session, not a sprint.

**Open:** sessions are personal · big things still need arcs · ceremonies need to change · how do we roll up to quarter-level reporting?

<span class="small">Full notes: `cllm/docs/sessions-not-stories.md`</span>

---

# Backup: Likely Q&A

| Question | Short answer |
|---|---|
| Would a junior engineer hit 130×? | No — probably 3–10×. The multiplier scales with judgment. |
| Is the 4-day code production-grade? | Test-passing, race-clean, GitOps-deployed. Same level as Helium at week 26. |
| Hallucinations / wrong code? | Caught by tests, by review-as-you-go, and by writing the design doc *with* Copilot **before** the code. |
| Does this kill engineering jobs? | No — it raises the floor. Same arc as compilers, IDEs, cloud. |
| Replicate for real customer work? | Yes. cLLM is the proof. That is the case for org-wide adoption. |
| Greenfield AI/ML research? | HVE accelerates the scaffolding. Novel research still needs human insight. |
| How does this change PMs / planning? | Big topic — see backup *"Sessions, Not Stories."* Short version: estimate in sessions, not points. PMs frame; engineers ship. |
