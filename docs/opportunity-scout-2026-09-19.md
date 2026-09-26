# Open-source opportunity scout — 19 September 2026

Objective: find the next Isthmus-class project with materially stronger commercial upside.
Method: 4 parallel scouts (agent runtimes, security/identity, coding-agent/enterprise AI, dev platforms/infra) plus direct deep-dives into GitHub issues, HN, hosting-market pricing and vendor pages. Star counts are as seen on 2026-09-19.

## Candidate field (Step 1)

| Repo | Stars | Recent momentum | Category | Outcome |
|---|---|---|---|---|
| openclaw/openclaw (Fleet cell supervisor) | 390k | Star growth slowed since May; managed-hosting market still expanding (Molted: 11,000+ instances, 300+ clients; 10+ providers) | agent host / hosting | Finalist 1 |
| paperclipai/paperclip | 81k | 0 to 81k since 2 Mar; 219 contributors; weekly releases; +560/wk now | agent control plane | Finalist 2 |
| Infisical/agent-vault, paradigmxyz/iron-proxy, nolabs-ai/nono | 2.2k / 0.7k / 4.1k | Docker Sandboxes HN thread 694 pts; Hermes adopted iron-proxy; agent-vault 156-pt Show HN | agent egress and credentials | Finalist 3 |
| NousResearch/hermes-agent | 247k | +2.6k/wk; 3.6k contributors | agent host | Finalist 4 |
| rtk-ai/rtk + headroomlabs-ai/headroom | 81k + 73k | +1.1k/wk and +1.5k/wk; both ROSS Q2 top-10 | LLM cost | Finalist 5 |
| deepseek-ai/deepseek-harness | 229k | +9k/wk; fastest launch ever | harness | Reject: DeepSeek owns the core; dev preview; CVE-2026-82533 |
| earendil-works/pi | 107k | +18.5k in Aug | harness | Not a base; no permission system by design (feeds Finalist 3) |
| stablyai/orca | 72k | +5.3k/wk | parallel-agent ADE | Reject: ~90 clones, 7 YC-backed competitors |
| diegosouzapw/OmniRoute | 68k | +23k in Aug | LLM gateway | Reject: LiteLLM 59k, Bifrost, Portkey (Palo Alto) |
| usestrix/strix | 64k | +30k Jul–Sep | AI pentest | Reject: funded company owns roadmap |
| vercel-labs/agent-browser | 43k | 321/day | browser automation | Watch |
| Dokploy/dokploy | 37k | +10k in 2026 | self-hosted PaaS | Watch: multi-org asks (#3195) but own Cloud |
| nanocoai/nanoclaw | 31k | +65/wk, stalled since June | agent host | Already covered by Isthmus |
| TencentCloud/TencentDB-Agent-Memory | 27k | +15.6k in Aug | agent memory | Watch |
| NVIDIA/NemoClaw + OpenShell | 22.5k + 8.7k | GTC-driven | agent sandbox | Reject: NVIDIA-controlled alpha |
| RyanCodrai/turbovec | 17k | 7 contributors | vector index | Reject: ICLR ethics dispute; Qdrant shipped it |
| pgdogdev/pgdog | 5.5k | Coinbase, Ramp in prod | Postgres proxy | Watch: shard-safety linter is a niche |
| agentgateway/agentgateway | 4.9k | 2.5x since Mar; Linux Foundation | MCP gateway | Watch: issue #239 (per-user token exchange) open 14 months |

## Shortlist (Steps 2–10)

### 1. OpenClaw fleet hosting — BUILD (after a 2-week experiment)

| # | Field | Finding |
|---|---|---|
| 1 | Repository | https://github.com/openclaw/openclaw ; fleet docs https://docs.openclaw.ai/gateway/multi-tenant-hosting |
| 2 | Stars | 390k, 82k forks, 1,000+ contributors, MIT, OpenClaw Foundation |
| 3 | Momentum | 250k by 3 Mar 2026, ~390k now. Repo growth slowed since May, but the hosting market did not: Molted runs 11,000+ instances for 300+ clients; Agent37 ($3.99–99.99/mo), Blink Claw ($45–180), Clawctl ($49+), xCloud, elest.io, OpenHosst, Lease Packet, Hostinger, MyClaw all sell managed OpenClaw. Fleet cell supervisor shipped Sep 2026. |
| 4 | What it does | Always-on personal agent gateway reachable from 50+ chat channels, with browser, shell, cron and a skills marketplace. |
| 5 | Creator philosophy | Bring the AI into the chat apps you already use and let it act; one gateway per person; everything runs locally. |
| 6 | Friction removed | Wiring agent, channels, tools and memory into one process that just runs. |
| 7 | Compromise | A single-user Node gateway that owns all state in SQLite and idles at 400–800 MB. Multi-tenancy is "one full gateway per tenant in a container"; upstream lists lightweight per-tenant processes, multi-machine coordination, billing and self-service as explicit non-goals. |
| 8 | Unresolved pain | Issue #148529: 632 agents, 12-minute startup vs 2 s; PR #151805 still 380 s cold / 131 s warm at 2 GB peak. #143524: SQLite WAL grows to 1.4–2.8 GB. #13758: 1.9 GB RSS after 13 h. ClawFleet measured 700 MB idle per instance; hosting guides say "4 GB per concurrent agent". Molted sells "3x more agents per machine" as a feature. OpenClawMachines Show HN: "gateway is not designed as multi-tenant". |
| 9 | Proposed improvement | A Go fleet control plane that runs stock OpenClaw cells unchanged but (a) hibernates idle cells and wakes them on inbound message through a shared lightweight ingress, (b) over-commits memory safely with cgroup limits and self-healing, (c) coordinates cells across hosts, (d) enforces per-cell security invariants (egress block, secrets, mounts, hardened runtime class, all reusable from Isthmus), (e) exposes metering and billing hooks. |
| 10 | Why users care | 3–5x more agents per server, instant wake on message, no changes to their agents or skills. |
| 11 | Why companies pay | Hosting providers price at $3–45/instance/month; infra per idle instance is the margin. Agencies run 20+ client cells. Regulated firms that currently ban OpenClaw need a governed fleet. |
| 12 | Technical score | 44/50 (momentum 5, problem 4, philosophy 5, friction 5, asymmetry 4, boundary 5, compat 5, solo 4, credibility 4, defensibility 3) |
| 13 | Commercial score | 31/40 (value 5, pain 4, WTP 3, recurring 4, enterprise 3, ops outsourced 4, stickiness 4, acquisition 4) |
| 14 | Total | 75/90 — very strong |
| 15 | MVP scope | Single-host Go daemon over the Docker API: idle detection, hibernate/restore (stop+start first, CRIU checkpoint second), webhook ingress for Telegram/Slack/Discord, per-cell cgroup limits, WAL checkpointing, Prometheus metrics, `doctor` and `security-check` ported from Isthmus. |
| 16 | MVP complexity | Medium. 6–10 weeks for single host. Multi-host and WhatsApp keep-alive are phase two. |
| 17 | Monetisation | Apache-2.0 single-host supervisor; paid control plane per host or per active cell; on-prem enterprise licence; optionally run your own dense hosting for agencies. |
| 18 | Acquirers | Molted (fills multi-host and density), Hostinger (largest OpenClaw VPS marketer, one-click templates), Docker (did the NanoClaw deal, ships Sandboxes), Cloudflare (containers for agents), DigitalOcean/Hetzner marketplaces. |
| 19 | Largest risks | OpenClaw commoditised by Google Remy, Microsoft, Claude Cowork; upstream reverses its non-goal; websocket-only channels (WhatsApp) resist hibernation; multi-tenant security liability; providers are price-sensitive. |
| 20 | Recommendation | BUILD, gated on the experiment below. |

### 2. Paperclip — INVESTIGATE

| # | Field | Finding |
|---|---|---|
| 1 | Repository | https://github.com/paperclipai/paperclip |
| 2 | Stars | 81k, 14.9k forks, 219 contributors, MIT, Paperclip Labs Inc. (pseudonymous founder, no disclosed funding) |
| 3 | Momentum | Launched 2 Mar 2026; 30k in three weeks; 70k by June; 81k now at ~560/wk. 15% fork ratio (OSSInsight: deployment intent). Weekly stable releases. |
| 4 | What it does | Org chart, task board, budgets, governance and audit for teams of heterogeneous agents (Claude Code, Codex, OpenClaw, Hermes) that wake on heartbeats. |
| 5 | Creator philosophy | "Frameworks were designed for demos, not production"; manage agents the way Kubernetes manages containers; transparency over smart defaults. |
| 6 | Friction removed | Coordinating many agents toward goals with budgets and human approval, instead of ad-hoc scripts. |
| 7 | Compromise | One Node/Express process is API, WebSocket server, heartbeat scheduler and subprocess supervisor, with Postgres underneath. Agent config is executable input (process adapters). |
| 8 | Unresolved pain | #958: unpaginated runs push Node to 3 GB+ RAM and 30% CPU. #1825: OOM every ~60 min. #2553: "Performance sucks at bigger scale?". #7411: an agent OOM kill takes the whole server down via systemd. #13366 (13 Sep): control-plane OOM produced 21 permanent blocks across 13 issues and 58 cancelled runs in 25 min. CVE-2026-41679 (CVSS 10) RCE via malicious agent import; DNS rebinding 9.6; unauthenticated routes 8.3. #3028 SSO/OIDC open. Community wrote paperclip-sandbox (Docker + mitmproxy allowlist) as a workaround. Consultancies charge $4,999+ per multi-company setup. |
| 9 | Proposed improvement | Crash-isolated Go run-supervisor (durable run ledger, budget and execution-authorization invariants, reconciliation) beside the Node API, packaged as a hardened multi-company distribution with OIDC, audit export and sandbox-by-default. |
| 10 | Why users care | Never lose an in-flight run when the UI or log pipeline blows up; run 10x more agents per box. |
| 11 | Why companies pay | Agencies (20 clients = $10k/mo savings per TRUETECH), teams already spending $250–500/mo on tokens, enterprises blocked on SSO/audit. |
| 12 | Technical score | 39/50 (momentum 5, problem 4, philosophy 5, friction 5, asymmetry 3, boundary 4, compat 4, solo 3, credibility 4, defensibility 2) |
| 13 | Commercial score | 31/40 (value 4, pain 4, WTP 4, recurring 4, enterprise 4, ops 4, stickiness 4, acquisition 3) |
| 14 | Total | 70/90 — interesting; fails the technical ≥40 gate on upstream velocity |
| 15 | MVP scope | Hardened distribution: OIDC, audit export, egress allowlist, run-supervisor sidecar, one-command multi-company install. |
| 16 | MVP complexity | Medium-high; the Node control plane is large and upstream already moved execution to a Rust runner. |
| 17 | Monetisation | Managed multi-company hosting ($99–499/mo per agency) plus enterprise on-prem licence. |
| 18 | Acquirers | Paperclip Labs (ownership transfer), Hostinger/Railway (one-click hosting), Molted (adds Paperclip to OpenClaw/Hermes fleets), Atlassian/Linear (agent work management). |
| 19 | Largest risks | 219 contributors and weekly releases will close the reliability gaps; Paperclip Labs' own cloud; PaperclipCloud already sells hosting from $21/mo. |
| 20 | Recommendation | INVESTIGATE: re-check in 60 days whether #13366-class failures persist after the Rust runner matures. |

### 3. Agent egress and credential broker — INVESTIGATE

| # | Field | Finding |
|---|---|---|
| 1 | Repository | https://github.com/Infisical/agent-vault (Go, MIT+ee), https://github.com/paradigmxyz/iron-proxy (Go, Apache-2.0), https://github.com/nolabs-ai/nono (Rust, Apache-2.0) |
| 2 | Stars | 2.2k / 674 / 4.1k |
| 3 | Momentum | agent-vault: 156-pt Show HN, 1,000 daily users in 3 months. iron-proxy adopted by Hermes as its egress layer. nono: 3.1k to 4.1k since July, Datadog and Okta engineers as users. Docker Sandboxes HN thread: 694 points, 396 comments. |
| 4 | What it does | Agent sets HTTPS_PROXY; proxy swaps placeholder tokens for real secrets at egress, default-deny allowlist, audit log; nono adds kernel-enforced least privilege. |
| 5 | Creator philosophy | The agent should never hold the real secret; enforce below the SDK layer. |
| 6 | Friction removed | No per-agent secret plumbing; one wrapper works for any harness. |
| 7 | Compromise | TLS interception. |
| 8 | Unresolved pain | OAuth2 refresh hands the new token straight to the agent; cert pinning, WebSockets, SigV4 and GCP signed requests bypass the proxy; no response-body credential stripping; NO_PROXY clobbering; Infisical calls its own design "clunky". Pi has no permission system by design; OpenClaw 2.0 stores secrets unencrypted. |
| 9 | Proposed improvement | Proxy-side OAuth refresh and token exchange, SigV4/GCP re-signing, WebSocket support, per-agent identity, hosted policy and audit plane. |
| 10 | Why users care | Agents never see a real credential; every outbound call is attributable. |
| 11 | Why companies pay | Security teams already blocking agent rollouts; regulated industries. |
| 12 | Technical score | 40/50 |
| 13 | Commercial score | 32/40 |
| 14 | Total | 72/90 — very strong, but incumbents are funded |
| 15 | MVP scope | Go proxy with OAuth refresh interception, SigV4 re-signing, identity tags, SIEM export. |
| 16 | MVP complexity | High; correctness of MITM plus auth protocols is unforgiving. |
| 17 | Monetisation | Per-agent seat pricing, enterprise licence, SIEM integrations. |
| 18 | Acquirers | Infisical, 1Password, Datadog, CrowdStrike/SentinelOne, Docker. |
| 19 | Largest risks | Infisical GA'd a commercial Agent Proxy; nolabs is building an enterprise platform and hiring; Docker Sandboxes ships the pattern natively. |
| 20 | Recommendation | INVESTIGATE only if the OpenClaw fleet thesis fails; the same egress code is a module of Finalist 1. |

### 4. Hermes multi-tenant hosting — WATCH

| # | Field | Finding |
|---|---|---|
| 1 | Repository | https://github.com/NousResearch/hermes-agent |
| 2 | Stars | 247k, 51.8k forks, 3.6k contributors, MIT |
| 3 | Momentum | 99k in 8 weeks, 247k now at +2.6k/wk; 5k+ open issues |
| 4 | What it does | Self-improving personal agent across CLI and chat channels with skills and memory. |
| 5 | Creator philosophy | Skills and memory as the moat; model-agnostic; runs on a $5 VPS. |
| 6 | Friction removed | A single agent that learns and can be reached from any chat app. |
| 7 | Compromise | One agent equals one tenant; memory is global and sessions are not scoped. |
| 8 | Unresolved pain | Issue #34352 "Solving the Multi-Tenant Hermes Problem": private-DM memory leaked into public articles; 12+ related issues; label "needs-decision"; NimbleCo fork ran 28 agents on shared infra at 400 MB total vs 200 MB per process. Managed Hermes hosting sells at $3–20/mo; Nous runs Hermes Cloud at $0.56/day. |
| 9 | Proposed improvement | Tenant-scoped memory and session isolation plus dense multi-tenant hosting supervisor. |
| 10 | Why users care | Run many clients or groups on one instance without leakage. |
| 11 | Why companies pay | Agencies and community operators; hosting providers. |
| 12 | Technical score | 36/50 |
| 13 | Commercial score | 25/40 |
| 14 | Total | 61/90 |
| 15 | MVP scope | memory:scope hook fork plus supervisor. |
| 16 | MVP complexity | Low-medium. |
| 17 | Monetisation | Hosting margin; small. |
| 18 | Acquirers | Nous Research, Molted, Agent37, xCloud. |
| 19 | Largest risks | Nous merges a 70-line hook and the thesis evaporates; Nous competes on hosting with capital. |
| 20 | Recommendation | WATCH. Hermes cells are a natural second workload for Finalist 1. |

### 5. Agent-token FinOps and correctness layer over RTK and Headroom — WATCH

| # | Field | Finding |
|---|---|---|
| 1 | Repository | https://github.com/rtk-ai/rtk , https://github.com/headroomlabs-ai/headroom |
| 2 | Stars | 81k (Rust) and 73k (Python/Rust), both Apache-2.0 |
| 3 | Momentum | Both launched Jan 2026; ROSS Q2 top-10; +1.1k/wk and +1.5k/wk |
| 4 | What it does | Compress shell output and tool results before the model reads them. |
| 5 | Creator philosophy | Deterministic local noise filtering beats prompt-level "ignore this". |
| 6 | Friction removed | Token spend on stale tool output. |
| 7 | Compromise | Lossy by construction; savings measured at the wrong layer. |
| 8 | Unresolved pain | 121-pt HN skeptic thread: "$4.96 saved on a $926 bill", demand for cost-per-correct-answer; Headroom #3650/#3634 silently drop fields; discussion #969 anxiety over Anthropic OAuth blocking. |
| 9 | Proposed improvement | Per-team billed-savings attribution, lossy-compression regression tests, policy and audit. |
| 10 | Why users care | Proof of real savings; no silent data loss. |
| 11 | Why companies pay | Engineering managers with agent bills. |
| 12 | Technical score | 37/50 |
| 13 | Commercial score | 23/40 |
| 14 | Total | 60/90 |
| 15 | MVP scope | Proxy-side metering and diff-based correctness harness. |
| 16 | MVP complexity | Low-medium. |
| 17 | Monetisation | Per-seat SaaS. |
| 18 | Acquirers | Headroom Labs, rtk-ai, Datadog LLM Observability, Vantage/CloudZero. |
| 19 | Largest risks | Both upstreams are companies that will ship reporting; low stickiness. |
| 20 | Recommendation | WATCH. |

## Final recommendation: OpenClaw fleet control plane

Why this is the next Isthmus with stronger upside: Isthmus put a small Go trust-kernel under an untouched TypeScript agent host. This is the same move one level up: a small Go host-side supervisor under untouched OpenClaw cells. The difference is the buyer. NanoClaw users were hobbyists; here the buyer is a hosting business with an infrastructure bill and per-instance pricing, and the market already has a $3.5k-MRR agency example on Molted's own pricing page. Upstream has written the thesis for us by declaring lightweight tenancy, multi-host and billing out of scope. Isthmus code (doctor, security-check, egress block, mount validation, hardened runtime class check) drops straight into cell hardening.

Product thesis: OpenClaw successfully removes the friction of wiring an always-on agent into the chat apps people already use, but retains a one-full-gateway-per-user architecture that makes shared hosting expensive and fragile. We can preserve stock OpenClaw cells and their whole skills ecosystem while eliminating idle-instance cost and single-host limits, producing 3–5x more agents per server with sub-5-second wake on message.

Killer metric: agents per server, at p95 wake-on-message under 5 seconds. Target 5x over the stock fleet supervisor.

First experiment (10 working days):
1. Days 1–2: baseline. 50 stock cells on one 16 GB Hetzner box under the upstream fleet supervisor. Record idle RSS, CPU, cold start, WAL growth, max cells before OOM.
2. Days 3–7: Go supervisor prototype over the Docker API: idle detector (no channel traffic for N minutes), stop-and-restore hibernation, tiny shared webhook ingress for Telegram, Slack and Discord that wakes the right cell, cgroup limits.
3. Days 8–9: measure cells per host and wake latency; try `docker checkpoint` (CRIU) for sub-second restore.
4. Day 10: verify state survives hibernate and wake (sessions, memory, cron, channel auth). Write it up with numbers.
Success: at least 3x baseline cells per host with p95 wake under 5 s and zero state loss.

Commercial validation, in parallel with the experiment:
- Interview 10 providers (Molted, Agent37, Clawctl, Blink, xCloud, elest.io, OpenHosst, Lease Packet, MyClaw, Hostinger): instances per host today, infra cost per instance, top three operational costs.
- Publish a density calculator landing page with the experiment's numbers.
- Ask three providers for a paid pilot ($500–2,000/mo) on their own workload before writing multi-host code.
- First $1k: one pilot. $100k ARR: 8 providers at ~$1k/mo plus ~15 agencies at ~$150/mo. $1M ARR: 10–15 on-prem enterprise fleets at $30–100k/yr on top of the provider base, or an acquisition by Molted, Hostinger or Docker once it is the default supervisor.

Stop condition: fewer than 2 of 10 providers name density or per-instance cost as a top-three problem or will pay $500/mo; the experiment cannot reach 3x at p95 wake under 5 s; upstream ships hibernation or multi-host fleet; or the provider count and Molted's instance count are declining during the validation window.
