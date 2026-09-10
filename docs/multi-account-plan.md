# One deck, every account

A plan to run all your Claude subscriptions from a single TicketDeck session:
each ticket opens under whichever account has room, sessions move between
accounts without leaving the deck, and you never launch a second deck to switch.

Status: proposal, not built. Written 2026-09-09.

## The end state

- One `deck`. It shows every subscription's usage and runs sessions under all of
  them.
- Opening a ticket picks an account for you, based on the usage readings the deck
  already collects. It says which one it picked before it opens.
- `H` hands a live session to another account in place: the tab stops, the
  transcript moves, the tab comes back under the new account. No second deck, no
  detach.
- An account that runs out mid-ticket is a one-key move, not a dead end.

## Why it can't happen today

Three things bind a deck to exactly one account, and all three have to go.

1. **The launcher.** `scripts/deck --account NAME` exports
   `CLAUDE_CONFIG_DIR=~/.claude-NAME` for the whole process tree. Every pane the
   deck opens inherits it, so a deck's sessions are all one account by
   construction.
2. **The workspace.** That same launcher gives each account its own herdr server
   (`XDG_CONFIG_HOME=~/.config-NAME`, its own socket). Two accounts means two
   workspaces, which is why switching is detach-and-relaunch.
3. **The code.** `session.ConfigDir()` reads the process environment, and
   `TranscriptPath`, `SessionExists` and `MarkResumable` all call it. Anything
   that asks "does this ticket have a session?" can only answer for the deck's
   own account.

The deck already works around this from the outside. `account.Owners` reads the
*other* accounts' transcripts and pokes their herdr sockets to badge a row, and
`quota.Poll` reads every account's usage into a shared cache. So the deck can
already see all four accounts — it just can't *run* any but its own.

## The one thing that makes it possible

herdr runs a pane's command verbatim, so an `env` prefix sets that pane's
account:

```
herdr agent start ZEN-1234 --cwd ~/Repos -- \
  env -u CLAUDE_CODE_OAUTH_TOKEN CLAUDE_CONFIG_DIR=$HOME/.claude-support claude --resume <id>
```

Verified on herdr 0.7.4 on 2026-09-09, against a throwaway server: the pane's
process saw `CLAUDE_CONFIG_DIR=/home/matthew/.claude-support` while the server
around it had the default. `internal/herd.Run` already passes everything after
`--` through untouched, so this is a change to how the argv is built and nothing
else.

`-u CLAUDE_CODE_OAUTH_TOKEN` is not optional. That variable, when set, belongs to
the account that exported it, and it would silently override the config dir the
line just set.

## Phase 0 — make the account an argument

No user-visible change. This is the refactor everything else needs.

- Add `ConfigDir` to `session.LaunchSpec` and `session.Ticket`.
- Give `TranscriptPath`, `SessionExists` and `MarkResumable` a config-dir
  parameter. `session.ConfigDir()` stays as the default for callers that don't
  care.
- `herd.claudeInner` prefixes `env -u CLAUDE_CODE_OAUTH_TOKEN CLAUDE_CONFIG_DIR=<dir>`
  whenever the target account isn't the deck's ambient one, and nothing when it
  is — an unchanged argv for the common case keeps the diff honest.
- Test: planning a launch for account X yields an argv starting with `env`, and
  planning for the deck's own account doesn't.

**Nothing changes for a user until Phase 2.** Phases 0 and 1 are safe to ship on
their own.

## Phase 1 — record which account runs which ticket

- `~/.ticketdeck/sessions.json`: `key → {account, updated_at}`, written when a
  session launches or moves.
- Treat it as a hint, never as truth. The authority stays what
  `account.Owners` already computes from the transcripts on disk and the live
  workspaces — a live session beats a newer file, and a file beats nothing.
  The ledger only seeds the badges before the first status poll lands, and breaks
  ties when one ticket has a transcript under two accounts.
- The account dot on each row already renders from `Owners`, so this phase adds
  no UI.

## Phase 2 — pick the account when a session starts

The router reads `quota.Poll`'s shared cache. It must not call the usage
endpoint itself: that budget is shared with every Claude Code status line on the
machine, and polling it per deck is what earned the 429s the cache exists to
stop.

Rules, in order:

1. **Stick.** A ticket with a transcript under account A resumes under A, unless
   A is excluded below. Moving costs a transcript copy and loses any in-flight
   tool call, so it needs a reason.
2. **Exclude the unusable.** An account whose entry says `token expired`,
   `not signed in` or `token rejected` can't run anything — a session there fails
   at launch, not later.
3. **Exclude the nearly-spent.** 5h utilization ≥ 90%, or 7d ≥ 95%. Headroom that
   dies twenty minutes into a ticket is worse than a slower start.
4. **Prefer the most room**, weighting the weekly window: it's the scarcer
   resource and it doesn't come back for days. Sort by 5h ascending, then by 7d
   ascending, then by fewest live sessions (a number the 3-second status poll
   already has).
5. **Refuse to guess on stale numbers.** `quota.Entry.Stale` already marks a
   reading older than two intervals. When the winner's reading is stale, fall
   back to the deck's own account and say so on the row.

Worked against this machine's cache as it stood at 22:12 on 2026-09-09:

| account | 5h | 7d | routed? |
|---|---|---|---|
| `m0` | 1% | 56% | **picked** |
| `support` | 9% | 44% | runner-up |
| `default` | 31% | 51% | eligible |
| `ap` | 100% | 56% | excluded — `token expired`, and 5h is spent |

`ap` is the case worth noticing: it carries a full-looking reading *and* a
failure reason, because the entry keeps the last good numbers after an attempt
fails. Rule 2 has to fire on the reason, not on the numbers.

**Surfacing it.** The existing "how to get back" overlay gains a line —
`opening under ⦿m0 · 5h 1% · 7d 56%` — and a key to pick a different account
instead. A router that chooses silently is one the user can't correct.

## Phase 3 — hand off without leaving the deck

`account.HandOff` already does the hard part: it copies the transcript between
config dirs, and refuses when the destination holds newer work than the source.
The session id comes from the ticket key alone, so the same ticket keeps its
identity in every account.

In one workspace, `H` becomes: close the pane → copy the transcript → start a new
pane on the same tab under the target account with `--resume <id>` → focus it.
One tab blinks and comes back. Today the same key ends with "now go run
`deck --account support`".

**Say what a move costs.** Resuming replays the conversation, so what carries
over is context, not a running process. An in-flight tool call is lost. The
confirm overlay should keep saying so.

**Auto-handoff stays opt-in and never silent.** When the account running a
session crosses the exclusion threshold, offer the move on a key — and only for a
session that is idle or waiting on input. Moving a session mid-tool-call to save
a rate limit trades a small stall for lost work.

## Phase 4 — retire the per-account deck

- `deck --account NAME` keeps working, so nothing breaks mid-transition.
- The default deck stops needing the `~/.config-NAME` overlay: that exists only
  to separate herdr workspaces, and there is now one workspace.
- `account.Owners` keeps probing peer sockets while old decks might still be
  running, then shrinks to reading transcripts.
- The "another deck is already running this ticket" gate (`blockingOwner`) mostly
  stops firing, because one workspace can see its own panes. Keep it until the
  peer-socket probe goes.

## Risks and open questions

- **Does `claude` key everything off `CLAUDE_CONFIG_DIR`?** `scripts/deck`
  already bets that it does, and the accounts on this box work. Per-pane env is
  the same bet at finer grain, so a surprise here would be a surprise today too.
- **Rate limits are per account, not per session.** Two sessions on one account
  share one budget. So the router balances *starts*, and the real check is what
  it does when three sessions on the same account burn it down together — which
  is Phase 3's job, not Phase 2's.
- **The usage endpoint is the fragile input.** Every rule above degrades to "use
  the deck's own account and say why" when a reading is missing or stale. That
  fallback needs a test, not a comment.
- **One workspace, one blast radius.** Today a wedged herdr server takes one
  account's sessions down. After this it takes all of them. Worth accepting; not
  worth leaving unsaid.

## Sequencing

Phase 0 and 1 are mechanical and independently shippable. Phase 2 is the one that
changes behaviour, and it is where the tests matter — the router is pure
(readings in, account out) and should be tested as such, with no filesystem and
no network. Phase 3 reuses `HandOff` and is mostly TUI plumbing. Phase 4 is
deletion, and can wait until you've run the new deck for a week.
