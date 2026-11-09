# CliproxyBar

A macOS menu bar app that shows whether you are working within your pooled Claude quota.
It only monitors: it reads `~/.cli-proxy-api/quota-history.jsonl` (written by this fork's
quota history) and the `disabled` flag of each auth file. It needs no management key and
makes no upstream calls.

**Menu bar:** two stacked mini bars for the Claude pool, 5h on top and weekly below. The
fill shows how much of the pool is used and the colour shows the verdict:

- green: at the current burn rate the pool lasts until the resets
- orange: it lasts, but drops below 20% (5h) or 15% (weekly) left
- red: at this pace the pool runs out before it refills
- grey: no sample from the proxy in 10 minutes

**Dropdown:** for each pool (Claude, Codex), pooled 5h and weekly usage, burn in points per
hour against a sustainable pace, and a projection of when the pool runs out. Each account
shows bars with a tick for elapsed window time (fill past the tick = ahead of pace), reset
countdowns, projected usage at reset, and which account the next new chat likely goes to.

The projection spends the measured burn (5h: last hour, weekly: last 24h) from the account
whose window resets first, as the `soonest-reset` routing strategy does, and refills each
account at its reset.

## Install

Needs macOS 14+ and Swift 6.

```zsh
./install.sh
```

This runs the tests, builds a release `~/Applications/CliproxyBar.app`, signs it ad hoc
and starts it at login with LaunchAgent `local.cliproxybar` (log:
`~/Library/Logs/cliproxybar.log`). Re-run it after changing the app.

`CPB_LIVE=1 swift test --filter dumpLive` prints the model for your live data.
