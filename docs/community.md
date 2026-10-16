# Where the OpenClaw hosting conversation happens (2026-09-26)

| Venue | Link | Why it matters |
|---|---|---|
| OpenClaw Discord (official) | https://discord.gg/clawd | Newcomers, setup help, platform channels, skill dev, showcase. Providers answer questions here. Look for self-hosting / deployment / security channels. |
| OpenClaw GitHub Discussions and issues | https://github.com/openclaw/openclaw | #114145 (scale-to-zero), #119035 (wake-only cron), #143471 (multi-node fleet), #148529 (632-agent startup). The scale/hosting people are in these threads. |
| r/OpenClaw, r/selfhosted, r/LocalLLaMA | reddit | Self-hosters comparing providers and complaining about memory. |
| Nous Research Discord (Hermes) | https://discord.gg/nousresearch | 140k members; Hermes is the second workload providers host. |
| OpenClaw German community | https://discord.gg/Uxuxdkt6 | Smaller; EU hosting angle (Hetzner). |
| awesome-openclaw lists | github.com/rohitg00, alvinreal, SamurAIGPT, vincentkoc | Providers submit themselves here (Molted PR #73, clawhire PRs). PR authors are the founders. |
| Provider blogs with authors | agent37.com/blog, blink.new/blog, molted.net/guides, revuo.ai | The reviewers who have already talked to every provider. |

Approach (replaces "send 10 interview requests"): buy the cheapest plan at three providers (Agent37 $3.99, OpenHosst $2.99, Lease Packet $5) to get a support channel and see their isolation; publish the density benchmark and the host checker; reply to public posts with something specific; ask for a 30-day pilot, not an opinion.

## Outreach note (numbers checked 2026-10-12)
Use as the body of a DM, a Discord post in a hosting channel, or a reply under a provider's post. Keep it to this length; the point is the numbers and the pilot ask.

---
I run a small host-side supervisor for stock OpenClaw cells (nothing changed inside the container) and have been measuring it on a Hetzner CX43, 8 vCPU / 16 GB:

- Stock cells with zram degrade at ~35 per host. Supervised: **100 cells on the same host, all healthy**, with ~2.7 GB of RAM still free at rest (idle cells paused and their memory reclaimed to a swapfile; ~90 MiB of RAM per cold cell, the rest of its ~0.8 GB on disk). Cells do not get more expensive as they age — I measured a week-old cell against one created an hour earlier and they hold the same memory; what grows is their swap footprint, which is why I size swap at 1 GB per cell.
- Wake on the first inbound Telegram message: recently active cells ~1 s; cold cells ~3 s alone, 3–8 s for the last of ten waking at once. At any instant no more than ~15 % of cells hold their memory (5 % warm, up to 10 mid-wake); the rest sit at ~65 MiB each.
- No user-visible artefact of the freeze: the supervisor covers OpenClaw's post-thaw window from the cell's own log; the tenant's config stays at defaults.

All of this was measured on the smallest sensible host (8 shared vCPU, 16 GB, throttled NVMe): the supervisor is tuned by four numbers, resident share, the conversations you expect at once (an active gateway holds ~0.8 GB and only idle cells can be reclaimed), the burst of cold wakes you want at page-in speed, and the RAM you have, and it scales with better hardware rather than needing it. Per-cell infrastructure cost drops by roughly 60–70 % on a cloud host at that density. If you run OpenClaw for customers on cloud VMs, I'd like to run a 30-day pilot on one of your hosts with your real tenants and give you the before/after numbers. Happy to share the full measurements first.
---
Target order: providers on cloud VMs (per-GB pricing) before Hetzner-class providers; the 60–70 % figure assumes ~20–30 stock cells per 16 GB and 100 supervised.

## Outreach log (Gate A, commercial half)

Gate A needs ≥2 of 10 providers naming density a top-3 cost with ≥$500/mo
intent, and 3 pilot offers sent. One row per contact, including the ones that
go nowhere — a "no" with a reason is evidence, silence is not. Log the date
sent, not the date drafted.

| Date | Who | Channel | What was asked | Reply | Density a top-3 cost? | Pilot offered / accepted |
|---|---|---|---|---|---|---|
| | | | | | | |

Standing count: **0 of 10 contacted, 0 pilot offers sent, 0 replies.** Started
2026-10-12 as item 2 of the re-planned Phase 1.

Rules kept from the approach above: buy the cheapest plan first so there is a
support channel and their isolation is visible; ask for a 30-day pilot on their
host with their real tenants, not for an opinion; lead with the measurements
file rather than the claim. If a provider says density is not a top-3 cost,
write down what they said is, verbatim — that is the most useful sentence in
this file and the thesis turns on it.
