# Adaptive reserve routing

Configure `routing.strategy: adaptive-reserve` and `routing.home-account` with an exact
credential ID or email. Enable session affinity. Claude uses adaptive reserves; other
providers retain soonest-reset selection. The strategy is opt-in and the old strategy
remains available.

The proxy reads Claude OAuth usage every two minutes using the existing authenticated
HTTP executor. These are usage reads, not generation requests. Poll failures never
change cooldowns; observations older than ten minutes cannot establish safe surplus.
A null/inactive window is budgeted as a full window starting on the next use, and is
labelled explicitly. Its synthetic planning reset is omitted from quota history.

## Forecast and selection

The last 30 days of quota observations seed an in-memory demand history on startup.
The forecast uses **total account consumption**, including the proxy user's traffic.
There is no reliable per-employee attribution. This is conservative protection, not a
measurement of colleagues' usage or a guarantee against sudden bursts.

Only fresh observations establish quiet periods. Repeated heartbeats, negative deltas,
reset crossings and gaps over 30 minutes do not establish a demand rate. Thirty minutes
of observed intervals are needed to learn a recent or hourly rate. Missing history uses
4 percentage points per hour for the five-hour window and 0.3 points per hour for the
weekly window. Hourly patterns distinguish weekdays and weekends in the proxy timezone.

For the short window, reserve the greater of the recent-hour rate and one quarter of the
typical hourly rate, integrated until reset. For the weekly window, adjust hourly demand
by recent activity (15–200% of typical); that adjustment fades over the next 24 hours so
one quiet hour is not treated as a week-long absence. Safety buffers are 5 points short
and 3 points weekly, tapering over the final hour/day. Reserve is capped at 95% of a window.

Spare = 100% - used - reserve. A new chat needs at least 5 points of short-window spare
and 2 points of weekly spare, with a fresh observation in both windows. Eligible accounts
rank by `min(short spare, weekly spare) + 0.25 * max(short spare / (1 + hours to reset),
weekly spare / (1 + days to reset))`. Scores are divided into fixed 0.1-point bands;
home preference breaks ties within a band, followed by exact score and credential ID.
Fixed bands keep ordering deterministic and transitive.

Warm chats and inherited subagents retain affinity while spare is positive. They move
only when a reserve is threatened and another eligible account exists; the different
entry and exit margins discourage oscillation. Recovery of home does not repatriate chats.
If no account has safe surplus, home is the last resort (confirmed exhaustion is ranked
last). This can consume reserved capacity and is explicitly labelled in the dashboard.
Credential cooldowns and request eligibility still apply before selection.

## Observability

`/v8/management/observability/routing` uses the same decision function and comparator as
the selector. Each Claude candidate has an `adaptive` decision with both windows, forecast,
buffer, spare, freshness, home status, score band and reason. The queue is provider-wide:
model support, priority and request eligibility can narrow real request candidates.
The Accounts section shows the calculation and Chats explains affinity. Forecasts that
use defaults are labelled as learning. Logs record the selected account and decision.

A restart drops session bindings, costing each open chat one cache rebuild. Quota history
survives restarts. Changes to routing strategy/home also rebuild the affinity selector;
plan those changes with the same care.
