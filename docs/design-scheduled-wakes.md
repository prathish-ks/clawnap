# Scheduled work on a hibernating host (design note, 2026-10-05)

A sleeping cell cannot run its own schedule. This note records what a host can
see of a cell's scheduled work, what it must wake for, what it may let slip,
and what is built so far.

Everything under "Verified" was read from a real 2026.9.7 cell on the fleet
host, or from the documentation shipped inside that image, on 2026-10-05.
Everything under "Design" is a decision, not a measurement.

## Verified

**The host already has access, and needs no credentials for it.** A cell's
whole OpenClaw home is a bind mount from the host (`<cells>/<name>/state`), so
the host reads the cell's own files directly: no exec into the container, no
gateway token, no API call. It works while the cell is frozen, which is when a
host needs it, and a frozen cell cannot change its schedule, so a value read at
pause time stays true for the whole sleep.

**Schedules live in the gateway's SQLite store**, `state/openclaw.sqlite`,
table `cron_jobs`. The useful columns are `enabled`, `job_json` (schedule kind
and expression, `wakeMode`, `sessionTarget`, `payload.kind`, `delivery.mode`)
and `state_json` (`nextRunAtMs`, `startupCatchupAtMs`, last run status). The
next run time is already computed as an absolute epoch-millisecond value, so a
host needs no cron parser and no timezone handling. Older builds kept this in
`~/.openclaw/cron/jobs.json`, and a `cron.store` config key can move the path;
both need handling.

**A stock cell is not schedule-free.** Before a tenant adds anything, three
jobs are already enabled:

| Job | Schedule | Wake mode | Delivers |
|---|---|---|---|
| Heartbeat | every 30 min (1 h under Anthropic OAuth/token auth) | next-heartbeat | to the owner DM, if one resolves |
| Memory dreaming promotion | cron `0 3 * * *` | now | no (`delivery: none`, isolated session) |
| Skill collection review | every 7 days | next-heartbeat | no (`delivery: none`, isolated session) |

**Heartbeat is the proactivity mechanism**, confirmed from the docs shipped in
the image: a system-owned automation that runs a periodic agent turn in the
main session so the model can surface anything needing attention. It uses the
ordinary system prompt and sends its prompt verbatim as a scheduled user
message, so every tick is a real agent turn with a model call behind it. Two
details matter to a host: without a resolvable owner DM an ambient poll skips
with `reason=no-route` (which is why our unpaired test cells recorded four
consecutive skips), and the stock prompt tells the agent to reply with a
no-reply token when nothing needs attention. Proactive behaviour is opt-in.

**Missed work is delayed, not dropped, and the gateway paces its own
recovery.** On startup, overdue agent-turn jobs and jobs awaiting a heartbeat
are rescheduled rather than replayed immediately, deliberately to keep model
and tool execution out of scheduler startup. This is the post-thaw housekeeping
we have been measuring since 2026-09-30 without naming it.

**Coalescing is strong but not total (measured 2026-10-05).** Counting rows in
the cell's own `cron_run_receipts` after booting cells that had been down since
2 October, roughly three days:

| Job | Occurrences missed | Runs on return |
|---|---|---|
| Heartbeat (every 30 min) | ~144 | 1 |
| Memory dreaming (daily) | 3 | 2 on two cells, 1 on a third |

So the high-frequency job collapses to a single run as documented, but the
daily agent-turn job did not reduce to one. Plan for a maintenance wake costing
more than one catch-up turn after a long sleep, and do not state coalescing as
a flat guarantee. A tenant can set `cron.skipMissedJobs` to advance missed recurring
slots instead of catching them up, which the docs justify as avoiding stale
reminders and unnecessary model calls. One-shot (`at`) jobs always catch up
regardless of that setting.

## Design

**The classifying rule: wake only for work whose value is tied to a wall clock
and whose result reaches a person.** Everything else rides along with the next
wake, because the gateway coalesces what it missed.

| Class | Identified by | Policy |
|---|---|---|
| Timed and user-facing | one-shot schedule, or a delivery target set | pre-wake before due |
| Internal maintenance | delivery disabled, recurring, isolated session | never wake for it; catch-up on the next wake |
| Proactive check-in | heartbeat payload | tier choice, off by default |

That classification removes the obvious hazard by policy rather than by
jitter: the nightly memory job on every cell is internal, so a hundred cells
never contend at 03:00.

**Two safety rules outrank the classification.**

- *Fail toward waking.* An unclassifiable job (unknown payload kind, schema
  change upstream) is treated as time-critical. Over-waking costs density,
  which is visible and tunable; under-waking loses a tenant's reminder, which
  is silent.
- *Read schedule metadata only, never payloads.* `job_json` carries prompt
  text, some of it long and tenant-specific. The host's promise is that it does
  not read messages. A reader extracts times, flags and kinds, and must never
  log or persist a payload body.

**The maintenance rotation (built, see below).** The classification above
leaves a gap: a cell nobody ever messages gets no natural wake, so its internal
maintenance never runs at all. A rotation closes it. Wake the
longest-unwoken hibernated cell on a pace derived from the fleet, so every cell
is reached at least once per target interval. Cells woken by real traffic sort
last and never consume a slot, so the real rate is well below the nominal pace.

Cost per maintenance wake, measured on the host 2026-10-05 with `-min-awake`
at the shipped 3 m and the fleet's 45 s idle timeout:

| Cell | Awake for | Note |
|---|---|---|
| v18 | 3 m 51 s | first wake in 92 h, catch-up held it awake past the floor |
| v15 | 3 m 06 s | |
| v16 | 3 m 05 s | |

Measured again 2026-10-07 at the **shipped 10 m idle**, which corrects both of
the estimates above. The idle clock does not start when the cell wakes; it
starts when the cell's catch-up stops generating traffic. On a cell that had
been asleep 131 hours:

| Segment | Measured |
|---|---|
| Wake to ready (stopped cell: a full gateway boot) | ~75 s |
| Ready to last activity (catch-up) | ~14 min |
| Last activity to hibernate (idle timeout) | 10 min |
| **Total awake** | **25 min 27 s** |

So the cost is `boot + catch-up + idle`, and the catch-up term grows with how
far behind the cell is. At a 12 h target over ~100 cells the pace is ~7 min, so
25 minute wakes overlap: the run peaked at **4 cells awake at once**, matching
3.6 predicted. This is a worst case for a neglected fleet, not the steady
state, where a 12 h rotation faces 12 h of missed work rather than five days.

A pace floor cannot bound this, because the duration varies with how far behind
the cell is. `-maintain-concurrent` (default 1) bounds it directly: the
rotation starts a wake only while it is holding fewer than that many cells
awake. The interval then stretches while a fleet is behind and tightens again
as cells come current, which is self-correcting and keeps the cost at about
0.8 GB.

The rotation also buys back a little proactivity. Because missed ticks are
coalesced, a cell woken once a day runs one heartbeat turn a day. That is not
the always-on product, but it is a real tier and it means a reactive host is
not silently dead on that feature.

**Tiers, not defaults.** A reactive tier wakes on messages and on timed
user-facing jobs and suppresses proactive check-ins. A proactive tier honours
heartbeats and costs roughly what an always-on cell costs. Arithmetic, again
not yet measured: at a 30 minute cadence about 12 cells of a 100-cell fleet are
awake at any moment, which does not fit 16 GB; at 60 minutes about 6, which
fits but consumes the burst headroom. Selling the choice is honest; silently
downgrading proactivity is not.

**Tenant-owned knobs worth pointing hibernating tenants at**, since they are
the tenant's to set and not the host's to flip: `heartbeat.activeHours`,
`heartbeat.every`, and `cron.skipMissedJobs`.

**A maintenance wake spends the tenant's model budget** on catch-up turns,
roughly one or two per cell per day. The fair framing is that an always-on cell
would have spent it anyway, so the rotation restores what they bought rather
than adding cost. It belongs in the tier description, not hidden.

## Built so far

Only the maintenance rotation, behind `-maintain-every` (0 = off, the default).
It yields to wakes in flight and to the headroom policy **only while it is
ahead of schedule**; once the oldest candidate is past its interval it proceeds
regardless. Yielding without that bound was a review finding: a host that is
busy, or that sits at its headroom target because the headroom policy is doing
its job, would otherwise never maintain a single cell.
The schedule reader is designed above and not written: the pre-wake machinery
it would feed (`NextDueAt`, `PreWake`, and the rule against sleeping when a job
is due inside the idle window) already ships and is driven by the operator
through `cells add -next-due`.

## Verified on the host (2026-10-05, fixed build)

Twenty cells booted on the fleet host, the host held below its headroom target
throughout (about 11–12 GB available against a 14 GB target) so the yield
condition was true on every pass:

| Phase | Setup | Result |
|---|---|---|
| Ahead of schedule | 240 h target, oldest candidate 92 h | 0 maintenance wakes in 6 min, as intended |
| Overdue | 1 h target, same tight host | 4 wakes in 15 min, one per 3 min pace, 0 failures |

The second row is the starvation fix. Before it, the rotation returned at the
headroom check on every pass and would have run nothing at all, on a host whose
memory policy was working exactly as designed.

Two caveats on what this did *not* establish. Every candidate was last awake
within a few minutes of every other, so the ordering was never strongly
discriminated: the picks are consistent with oldest-first but do not prove it
beyond the unit tests. And the candidate set was the whole registry, 103 cells,
because a stopped cell is still a hibernated cell; a maintenance wake on a
stopped cell is a full gateway boot rather than an unpause and costs
considerably more than the figures above. The pace floor bound at 103
candidates, stretching the effective interval from 1 h to about 5 h, which is
the documented safe direction but means the flag's promise only holds while the
fleet is small enough for the floor not to bind.

## The concurrency cap, verified (2026-10-07)

Same host, same settings (`-maintain-every 12h`, shipped 10 m idle), the only
change being `-maintain-concurrent`:

| | Uncapped | Capped at 1 |
|---|---|---|
| Rotation wakes | 4 in 36 min, one per 7 min pace | 1 in 30 min |
| Rotation cells awake at once | 4 | 1 |

The capped run is attributable: the cell it woke (`alice`, ready 03:30:08)
never hibernated inside the window — zero hibernations were logged in the whole
30 minutes — so the budget was never freed and no second wake was eligible.
The headroom check was disabled for both runs, so it cannot explain the
difference.

Awake duration is also far more variable than one figure suggests. In the same
session it was 25 min for one cell, over 45 min for another still working at
139 % of a core, and over 21 min for `alice` when the window closed. No pace
can be chosen safely against a spread like that, which is the case for bounding
concurrency instead.

## Open measurements

1. ~~The true awake cost of one maintenance wake.~~ Measured: 3 m 05 s to
   3 m 51 s at a 45 s idle timeout (2026-10-05), and 25 m 27 s at the shipped
   10 m idle on a cell five days behind (2026-10-07, broken down above). What
   remains open is the **steady-state** cost, where a cell is only ever ~12 h
   behind; that needs a run longer than one session.
2. ~~Whether a cell that slept a full day coalesces into a single catch-up
   run.~~ Measured 2026-10-05: the heartbeat does, the daily job does not
   (see the table above). Still open is how this scales with a longer sleep.
3. The proactive-tier arithmetic above, on a real host.

## Upstream

The host is reading an internal schema and inferring which jobs are
user-visible from field names. A declared field saying so would remove the
guesswork, which is close to what openclaw/openclaw#119035 (wake-only cron
payload) already asks for. Worth adding to the #114145 thread.
