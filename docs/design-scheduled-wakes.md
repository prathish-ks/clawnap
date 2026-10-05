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
and tool execution out of scheduler startup. Missed timer ticks are coalesced:
a cell that slept through twelve heartbeats runs one on wake, not twelve. This
is the post-thaw housekeeping we have been measuring since 2026-09-30 without
naming it. A tenant can set `cron.skipMissedJobs` to advance missed recurring
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

Arithmetic at 100 cells and a 24 h target, from measured per-cell figures
rather than a measured scenario: a maintenance wake costs the minimum awake
time plus the cell's idle timeout, roughly 11–13 minutes at the shipped 10 m
idle, so a ~14 minute pace keeps about one extra cell awake continuously, near
0.8 GB. That is noise against the ~15 concurrent cells the host is already
sized for, but it is at the edge: the pace must stay longer than the per-wake
awake cost or wakes accumulate.

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
The schedule reader is designed above and not written: the pre-wake machinery
it would feed (`NextDueAt`, `PreWake`, and the rule against sleeping when a job
is due inside the idle window) already ships and is driven by the operator
through `cells add -next-due`.

## Open measurements

1. The true awake cost of one maintenance wake at the shipped idle timeout.
   The rotation's pace floor currently uses `MinAwake`, which is a lower bound,
   not the measured cost.
2. Whether a cell that slept a full day really does coalesce into a single
   catch-up run, as documented.
3. The proactive-tier arithmetic above, on a real host.

## Upstream

The host is reading an internal schema and inferring which jobs are
user-visible from field names. A declared field saying so would remove the
guesswork, which is close to what openclaw/openclaw#119035 (wake-only cron
payload) already asks for. Worth adding to the #114145 thread.
