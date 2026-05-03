# Project Helium

Remember back to when we started Helium (that was a lot of fun!). How long would you estimate it was before we had the following functionality (or equivalent)? I realize we didn't go in the exact same order, which is why I need your big brains.

A service
A test client
A benchmark client
Structured logging
Prometheus metrics
A "good" dashboard(s) that update live as the benchmark is running
Something like GitOps (using Flux on K8s)

The core Helium team was myself (a Partner FDE today; Principal at the time of Helium), another Principal FDE, a Senior FDE, a Principal Program Manager. (At the time, our engineering titles were Software Engineer; we are all FDEs now and standardize on current titles when telling the story.)

The team all agrees that it took the 4 of us about 6-7 months to ship our first good release.

Some thoughts from the team

Oh this whole thread takes me back 🥰.  I agree with the 6-7 month time estimate. It's tricky to quantify since I feel like the bar of what was "good"/"done" changed over time, like App Service -> k8s, Azure Monitor dashboards -> Grafana, 24hr smoke testing.

6mths MVE, 7 for MVP, 8 for release

Agree - and WebV came later and lots of enhancements. Then Helium became NGSA, Walmart deployed it to production for the SRE team, then we used it for triplets ... By any measure, Helium was a HUGE success, but, especially for team culture. Look at how well most of that team is doing today. It was good to be us!

## Hypervelocity Engineering (HVE)

Hypervelocity Engineering (HVE) is a methodology that leverages AI and structured workflows to accelerate high-quality software development while maintaining engineering standards.
Concept and Principles

Hypervelocity Engineering is not about coding faster indiscriminately; it emphasizes speed with direction and quality. Unlike “vibe coding,” HVE focuses on producing well-structured, maintainable code rapidly by combining human expertise with AI assistance. Velocity in HVE refers to both speed and direction, meaning teams can pull more work per sprint without sacrificing quality or project goals. It involves codifying best practices, error handling, and code contracts into AI-guided workflows, ensuring consistent adherence to standards.

HVE also transforms traditional development cycles. For example, instead of lengthy paper prototypes and stakeholder meetings, teams can rapidly generate interactive prototypes and iterate in real time with AI support, improving collaboration and reducing rework.

Benefits and Impact
HVE enables small, expert teams to innovate rapidly while maintaining quality, security, and scalability. It reduces the time from concept to prototype from weeks to hours or minutes, allowing teams to respond faster to customer needs and market changes. Additionally, AI guidance evolves over time, increasing both velocity and long-term acceleration in software development.

Practical Applications
Rapid prototyping and stakeholder feedback loops
AI-assisted coding with built-in best practices
Co-development of architectural decision records (ADRs) and implementation
Enhanced developer productivity metrics without compromising code quality
In essence, Hypervelocity Engineering reshapes software development by combining AI, structured workflows, and human expertise to deliver high-quality solutions at unprecedented speed.

## Prompt
using the helium project as a baseline, I want to create a HVE presentation that is an apples to apples comparison of cLLM vs. Helium.

For Helium , let's use 26 weeks as the baseline for an MVP.

Using GitHub Copilot, I was able to create a similar MVP in 4 days.

Some of the differences
it's about 5 years later and I've done Helium like projects multiple times
we were all new to Kubernetes, Prometheus, Grafana, GitOps
I now have 5 years of experiences in each and am considered one of the best K8s FDEs in Microsoft
I didn't have much experience with GPUs on K8s when I started
I had never heard of vLLM

ask me any questions and then let's plan a 30 minute presentation.

## Copilot Q&A

The following clarifying questions were asked by GitHub Copilot before drafting the HVE presentation plan in `presentation-hve-30min.md`. Captured here so anyone else running a similar Helium-vs-HVE comparison for their own project can answer the same questions and get a comparable plan.

### 1. Audience

**Q:** Who is the audience for this HVE presentation?

Options:
- Same as demo deck (CVP, partner FDEs, principal FDEs) *(recommended)*
- Engineering leadership / ICs considering adopting HVE
- Mixed exec + technical (broader internal MS audience)
- External / customer-facing

**A:** Same as demo deck (CVP, partner FDEs, principal FDEs).

### 2. Primary Takeaway / Framing

**Q:** What is the primary takeaway you want them to leave with?

Options:
- HVE is real: 26 weeks → 4 days on a comparable MVP *(recommended)*
- HVE only works with senior engineers + Copilot — not a silver bullet
- We should adopt HVE as a standard FDE practice
- All of the above, with a call to action

**A:** Option 1 is the most important. We are already adopting HVE as a standard, so focus on "more evidence." HVE is not strictly limited to very senior engineers but it definitely helps and is quite amazing at what you can accomplish. Important nuance on the comparison: it's not just "26 weeks" — it's **3 engineers and a PM for 26 weeks**.

### 3. Scope

**Q:** Should this be a standalone HVE talk, or extend the existing 30-min cLLM demo deck?

Options:
- Standalone 30-min HVE talk (cLLM is just the case study) *(recommended)*
- Companion deck — pairs with the cLLM demo
- Replace the demo deck entirely with HVE framing

**A:** Standalone 30-min HVE talk; cLLM is the case study.

### 4. Live Demo

**Q:** Do you want a live demo segment, and if so what?

Options:
- Live: show Copilot building a small piece in real time *(recommended)*
- Live: show MCP-driven cLLM benchmark (reuse existing demo)
- No live demo — slides + artifacts only
- Pre-recorded demo clip

**A:** Live: MCP-driven cLLM benchmark (reuse existing demo).

### 5. Evidence Artifacts

**Q:** What concrete artifacts do you want to use as evidence of the 4-day build?

Options (multi-select):
- Git history / commit timeline
- Lines of code, test coverage, benchmark reports
- ADRs / design docs co-written with Copilot
- All of the above

**A:** All of the above — git history/timeline, LOC and test coverage and benchmark reports, ADRs/design docs co-written with Copilot.

### 6. Honesty on Caveats

**Q:** How candid do you want to be about the caveats (5 yrs experience, repeat project, AI limits)?

Options:
- Very candid — caveats are a major section, builds credibility *(recommended)*
- Mention briefly, focus on the win
- Save caveats for Q&A

**A:** Mention briefly, focus on the win.

### 7. Comparison Axes

**Q:** Which axes do you want in the apples-to-apples comparison table for Helium (26 wk) vs cLLM (4 days)?

Options (multi-select):
- Time to MVP
- Team size & seniority
- Tech stack & prior experience
- Feature checklist (service, bench, metrics, dashboards, GitOps)
- Code quality / tests / docs
- Cost (people-weeks)
- All of the above

**A:** All of the above.

### 8. Q&A Time Budget

**Q:** Is this 30 minutes total, or 30 + Q&A like the demo deck?

Options:
- 30 min total (no Q&A budget)
- 30 min talk + 15-20 min Q&A *(recommended)*
- 30 min slot inside a larger session

**A:** 30 min talk + 15–20 min Q&A.

---

### How to reuse this template

To run the same comparison for another project:

1. Establish your **baseline project** — a comparable system you (or a team you know) shipped pre-HVE. Capture: team size, seniority mix, calendar weeks, prior experience with the stack.
2. Compute **people-weeks** for the baseline: `engineers × weeks` (count the PM separately if appropriate).
3. Build the **HVE project** under measurable conditions: real git history, tests, docs, evidence reports.
4. Pull the **quantified evidence**: commits per day, Go/Python LOC, test LOC, YAML LOC, docs LOC, design-doc count, scenario-report count.
5. Answer the 8 questions above for your audience and goals.
6. Produce a side-by-side table on the same axes used in `presentation-hve-30min.md` and let the **ratio of people-weeks** speak first.

## Open Questions for You

1. Do you want to name the Helium teammates on slide 3, or keep it anonymous-but-respectful?
Joseph - Principal FDE
Anne - Senior FDE
Deanna - Principal PM

2. Should the "what changed" segment include a 60-second live Copilot moment (e.g. add a small feature on stage), or keep the live demo strictly to MCP/cLLM?  just the MCP demo

3. Any specific CVP-level talking points you want me to lean into (cost, time-to-customer-value, talent leverage)?
Dan (CVP and my boss) is interested in HVE adoption, time to customer value, observability, engineering fundamentals, AI adoption

