# CliproxyBar

A macOS menu bar app that shows whether you are working within your pooled Claude quota.
It only monitors: it reads `~/.cli-proxy-api/quota-history.jsonl` (written by this fork's
quota history) and the `disabled` flag of each auth file. It needs no management key and
makes no upstream calls.

**Menu bar:** two stacked mini bars for the Claude pool, 5h on top and weekly below. The
fill shows how much of the pool is used and the colour shows the verdict:

- green: at the current burn rate the pool lasts until the resets
- orange: it lasts, but drops below 20% (5h) or 15% (weekly) left
- red: safe per-seat headroom is projected to run out before a reset
- grey: no sample from the proxy in 10 minutes

A red warning triangle and red bars appear in advance when every seat is projected to
reach either quota limit (90% of 5h or 99% weekly, matching the router). The dropdown
shows **Headroom running out** with a predicted loss and recovery time, or **No safe headroom**, names each seat's limiting window, and shows the earliest reset
that restores headroom in both windows on one seat. If both windows constrain a seat, both
must reset. **Quota exhausted** means every seat has at least one window at 100%.
The safety thresholds only demote seats: the proxy may still try their remaining quota.
These are quota observations, not a live check for upstream rejection or credential health.
The headroom warning retains each window's highest reading until reset so one-point jitter
cannot clear it; the usage bars still show the latest reported percentages.
The two pooled projections now spend both windows on the same seat: one seat's spare 5h
quota cannot be combined with another seat's spare weekly quota. A forecast that persists
for two minutes triggers one headroom notification, with a five-hour cooldown.

**Dropdown:** for each pool (Claude, Codex), pooled 5h and weekly usage, burn in points per
hour against a sustainable pace, and a projection of when the pool runs out. Each account
shows bars with a tick for elapsed window time (fill past the tick = ahead of pace), reset
countdowns, projected usage at reset, and which account the next new chat likely goes to.

The joint simulator (`JointProjection.swift`, mirrored in the panel's
`jointQuotaProjection.ts`) uses the existing learned weekly rhythm and reactive 5h rate.
It keeps current workload shares on their seats while both windows have headroom, then
moves the constrained share to the earliest weekly reset with room. It stops spending
during a gap and resumes at resets, without assuming queued work. A 5h window starts on
first use, not on a timer while idle. It samples rates at most five minutes apart and
splits at exact reset and threshold instants. No burn/profile tuning constants changed.
Predictions are scenarios; future demand and real chat affinity can differ.

`CPB_LIVE=1 swift test --filter dumpHeadroomReplay` replays earlier observations only,
without looking at future quota samples, to inspect the advance warning.

## Install

Needs macOS 14+ and Swift 6.

```zsh
./install.sh
```

This runs the tests, builds a release `~/Applications/CliproxyBar.app`, signs it ad hoc
and starts it at login with LaunchAgent `local.cliproxybar` (log:
`~/Library/Logs/cliproxybar.log`). Re-run it after changing the app.

`CPB_LIVE=1 swift test --filter dumpLive` prints the model for your live data.
