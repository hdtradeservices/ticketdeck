# TicketDeck setup

One-time setup to run TicketDeck, optionally on top of [herdr](https://herdr.dev) for the
start-work / detach-keep-running / switch-tickets workflow.

## 1. Build TicketDeck onto your PATH

```sh
cd ~/Repos/ticketdeck
go build -o ~/.local/bin/ticketdeck ./cmd/ticketdeck
```

Rebuild with the same command after pulling changes.

## 2. Linear key

TicketDeck reads `LINEAR_API_KEY`. Read-only scope is enough for everything except the
optional status-change hotkey (`s` → Done/Validate/Cancel), which needs a **write-scoped**
key. Export it in your shell rc so every session and the herdr server inherit it:

```sh
export LINEAR_API_KEY=lin_api_...      # https://linear.app/settings/api
```

At this point `ticketdeck` works standalone (Claude backend): `ticketdeck`.

## 3. herdr (optional, recommended)

herdr is an agent multiplexer that gives detach/re-attach persistence and a live agent-state
sidebar. Install the official Linux x86_64 binary and the Claude integration hook:

```sh
gh release download --repo ogulcancelik/herdr --pattern 'herdr-linux-x86_64' \
  --output ~/.local/bin/herdr && chmod +x ~/.local/bin/herdr
herdr integration install claude       # lets herdr detect claude working/blocked/idle
```

The integration hook is a no-op outside a herdr pane and is reversible:
`herdr integration uninstall claude`. TicketDeck's `--backend auto` uses herdr when it's
installed, else the built-in Claude path.

## 4. Launch: the `deck` command

`~/.local/bin/deck` opens the herdr workspace with TicketDeck already in a pane:

```sh
deck
```

It ensures a herdr server is running, starts TicketDeck as the `deck` agent (once), and
attaches. In TicketDeck, **Enter** on a ticket runs `herdr agent start <TICKET> -- claude …`
as a sibling pane; start work, **detach** (herdr keybinding) to leave it running, come back
and Enter another ticket. Detaching/quitting herdr leaves the server and all ticket sessions
running in the background.

**Layout:** the deck lives in **tab 1**; each ticket — or project — you open lands in **its own
tab**, so only one thing is visible at a time and nothing piles up as split panes. Open a ticket
→ you're taken to its tab → work → jump back to the deck (tab 1) → open the next one. Agents
keep running in their background tabs. A project's tab is titled by the project's name; a
ticket's by `KEY  short title`.

**TicketDeck keys** (inside the deck):

The table below describes the keys on a **ticket** row. A **project** row (the `PROJECTS`
section at the top) takes the same keys against the project: `⏎` opens/attaches the
project's own session, `d` its detail overlay (progress, dates, lead, your tickets in it,
its description), `o` the Linear project page, `p` the PRs across all its tickets, `H`
hands the session to another Claude subscription, and the writes land on the project's own
fields — `s` status (Planned / In Progress / Blocked / Completed / Cancel, confirm-gated),
`P` priority, `a` **lead** (a project's nearest thing to an assignee). Completing or
cancelling a project closes its session, like Done does for a ticket.

`t` is the one ticket key that does **not** carry over: `/triage` triages a single issue,
so on a project row it says so and does nothing. Unfold the project and triage one of its
tickets instead.

| goal | key |
|---|---|
| move / page / top-bottom | `↑`/`↓` (`j`/`k`) · `PgUp`/`PgDn` · `g`/`G` |
| open the ticket (launch/attach its session) | `Enter` — the first time, a reminder shows how to get back (`Ctrl+b 1`); `⏎` proceeds, `d` proceeds and never shows it again. If **another deck is already running that ticket**, it stops and shows that deck's live state instead of forking the session — see [One deck per ticket](#multiple-claude-subscriptions-accounts). |
| show the ticket's **description** (rendered markdown) | `d` (in the overlay: `Enter` opens the session, `o` browser, `p` PR, `↑`/`↓` scroll, `esc` closes) |
| show the ticket's **investigation** or **implementation plan** | `i` / `P`, inside the description overlay. Those write-ups live in the ticket's Linear comments, not its description, so the deck fetches that ticket's comments the first time you press one of these keys and reuses them after. It shows the newest `## Investigation summary` (`/investigate`) or `## Implementation plan` (`/plan`) comment, with a line naming who wrote it and when. Pressing the same key again goes back to the description, as does `esc`; `d` closes the overlay outright. Says so plainly when the ticket has no such comment yet — and `r` re-reads the comments, for when the agent posts its write-up while you're looking. |
| open the ticket in the **web browser** | `o` |
| open the ticket's **linked PR** in the browser | `p` — one PR opens straight away. Several open a **picker** (a ticket's PRs usually span different repos): rows are ordered most-actionable-first and show state + `repo#number` + title; `↑`/`↓` select, `⏎` open, `1`-`9` open that one directly, **`a` opens all of them**, `esc` cancels. |
| **change status** → Done / Validate / Monitoring / Blocked / Cancel | `s`, then `d`/`v`/`m`/`b`/`c`, then `y` to confirm — **writes to Linear; needs a write-scoped `LINEAR_API_KEY`**. Moving to a terminal state (**Done / Cancel**) also **closes that ticket's Claude session** if one is running (transcript persists — resumable), then returns focus to the deck (not the neighbor tab). Moving to **Done** also runs the **unblock cascade**: every still-open ticket this one was *blocking* is moved to its team's **Triage** state. |
| **change priority** → Urgent / High / Medium / Low / None | `P`, then `u`/`h`/`m`/`l`/`0` — **write; write-scoped key** |
| **assign / reassign / unassign** | `a` opens a picker — type to filter people, `↑`/`↓` select, `⏎` assign (top row = **Unassign**), `esc` cancel — **writes to Linear; write-scoped key** (reassigning away from you drops the ticket off the list) |
| open an **ad-hoc Claude session** not tied to any ticket (own tab) | `n` |
| **`/triage` in the background** | `t` — starts the ticket's session in its own (unfocused) tab if it isn't running, submits `/triage`, and keeps you on the deck (**runs a Claude turn**; herdr backend). On an "Other sessions" row it just submits `/triage` to that session. Gated the same way `Enter` is when another deck is running the ticket. |
| **fold/unfold** a priority section (collapsed shows a ticket count) | `Space` (toggle) · `←` collapse · `→` expand — on the `PROJECTS` header this folds the whole section; on a project row (or one of its tickets) it folds that project's ticket list |
| **search / filter the list** | `/` — type to filter tickets by key or title (live, case-insensitive; all matching groups expand). `⏎` keeps the filter and returns to list nav; `esc` clears it. While a filter is applied the footer shows `filter "…" · esc clear`. |
| **hand the session to another Claude subscription** | `H` — for when this subscription hits a limit mid-task. Stops the session here, copies its transcript to the other account, and it becomes resumable in that deck. `⏎` confirms, `1`-`9` picks a target, `esc` cancels. Only offered when a second subscription exists — see [Multiple Claude subscriptions](#multiple-claude-subscriptions-accounts). |
| refresh | `r` — a manual refresh. Session badges also refresh on their own every few seconds, and the instant the deck regains focus, so you don't have to. |

An **"Other sessions"** section at the bottom lists live Claude sessions not shown as a
ticket badge — ad-hoc `n` sessions and sessions for tickets that dropped off the list
(Done/Cancelled). With the cursor on one: `Enter` switches to it, **`x` closes it** (its
transcript persists, so it stays resumable).
| quit | `q` (standalone) — **under the `deck` launcher `q` is disabled** so it can't orphan the deck; leave herdr with `Ctrl+b q` (detach, keeps sessions running), or `Ctrl+c` as a hard exit |

**Icons / badges:**

- **Priority** group headers are color-coded: Urgent (red) · High (orange) · Medium (yellow) · Low (blue) · No priority (gray), always showing a `(count)`.
- **Session** badge per ticket: `●` working (green) · `◆` needs input (amber) · `○` idle (cyan) · `✓` done · `↻` **resumable** (an on-disk session you can reattach — shown even right after a fresh start, before any agent is running) · `·` none yet.
- **Account** dot `⦿` left of the session badge, in the owning subscription's color — which Claude account is running that ticket's session, including one running under another account. Labelled with the account name on the highlighted row. Only rendered when you have more than one subscription; see [Multiple Claude subscriptions](#multiple-claude-subscriptions-accounts).
- **Top-10 focus** — priority sections are auto-folded unless they hold one of your top 10 tickets (by priority, then status, then recency), and this is re-applied on each refresh. So if your top 10 are all Urgent, only Urgent stays open; if they span Urgent/High/Medium, those stay open and lower sections fold (shown as `▸ Low (3)`). Expand any folded section with `→`/`Space` (it re-folds on the next refresh).
- **Working tickets are dimmed** — a ticket whose session is actively `working` renders in faint gray (no bright id/title), so your eye is drawn to the tickets that still need you (needs-input, idle, resumable, untouched) rather than the ones already in progress. Move the cursor onto one and it still highlights normally.
- **Time-in-state** — live sessions (working / needs-input / idle) show how long they've held that state once it's been ≥1 minute, e.g. `◆ needs input 20m` or `● working 45m`. Useful for spotting a session parked a while (waiting on CI, or one that's needed input for a bit). The detail view (`d`) spells it out. Note: it's measured from when the deck first saw the state, not necessarily the session's true start.
- **PR** `⇄` marks a ticket with a linked pull request, colored by its state: merged (violet) · open (green) · closed (red) · draft/unknown (gray). A ticket with several shows the count — `⇄3` — and `p` opens the picker. The color reflects the most actionable PR (open beats draft beats merged beats closed).
- **Blocked-by** `⛔ ZEN-1234, …` (red) trails a **Blocked** ticket's title, listing the still-open tickets it's blocked by (up to 3, then `+N`); the `d` detail view spells them all out.
- **Recently done** tickets stay on the deck for 12h after they're completed, rendered **struck-through** and dimmed, then drop off. They don't consume top-10 focus slots, so a lower priority section holding only done tickets still folds.
- **Validation flags** trail the title when a ticket carries a validation label: `⚑ validation failed` (red, `validation-failed`) or `⚑ inconclusive` (amber, `validation-inconclusive`); also shown in the `d` detail view.

**herdr keys** (prefix `Ctrl+b`):

| goal | key |
|---|---|
| **back to the deck** (ticket list) | **`Ctrl+b` then `1`** — the deck is pinned to tab 1 |
| **show this ticket's description** (from inside its session) | **`Ctrl+b` then `i`** — a popup runs `ticketdeck describe`, which resolves the ticket from the pane you're in (`HERDR_ACTIVE_PANE_ID`), fetches it read-only, and renders the markdown; `q` closes the popup. Installed by `install.sh` as a `[[keys.command]]` with `type = "popup"`. |
| back to the deck (alias) | **`Ctrl+b` then `d`** — custom binding running `herdr agent focus deck` (config.toml `[[keys.command]]`) |
| jump to any ticket/the deck **by name** (searchable navigator) | `Ctrl+b` then `g`, type a key, `Enter` |
| pick another tab | `Ctrl+b` then `2`…`9`, or `Ctrl+b p`/`Ctrl+b n` (prev/next) |
| **detach** the whole workspace — everything keeps running in the background | `Ctrl+b` then `q` |
| hide the spaces+agents sidebar (see note — resets each restart) | `Ctrl+b` then `b` |
| close/kill the current pane (**stops that agent** — don't use to background) | `Ctrl+b x` |

> **Don't set `[keys.indexed]` in `config.toml`.** Setting `tabs = "ctrl"` (to get
> `Ctrl+1..9`) silently **shadows** the default `switch_tab = "prefix+1..9"`, so
> `Ctrl+b 1` stops working too — and `Ctrl+digit` isn't delivered by most
> terminals anyway, so you're left with no numeric tab switching at all.

> **Restart = clean workspace.** herdr never re-runs a pane's command on restart —
> it restores every pane as a bare shell (the deck's `bash` respawn loop and each
> ticket's `claude` are not re-launched). So the `deck` launcher, on a cold start,
> resets the workspace (`~/.config/herdr/session.json`, sidebar prefs preserved):
> stale shell tabs are cleared and the deck is recreated with its respawn loop as
> **tab #1**, so `Ctrl+b 1` always works. You lose nothing — your ticket **sessions**
> live on disk and show as `↻ resumable` in the deck; re-open them with `Enter`.
> While the server stays up, the deck's respawn loop keeps ticketdeck alive in tab
> #1 even if it's quit or crashes.

> **Sidebar.** herdr has no setting to start the sidebar collapsed, and the
> collapse toggle isn't persisted across a server restart — so the spaces+agents
> sidebar reappears each fresh start. Press `Ctrl+b b` once to hide it (with
> `sidebar_collapsed_mode = "hidden"` it then disappears fully for that session).

To background a ticket and open another, **switch tabs (`Ctrl+b 1` for the deck) — never
`Ctrl+b x`** (that kills the agent). Within a running session, backgrounded agents show
`working`/`idle`/`needs-input` badges; after a full restart every ticket shows `↻ resumable`
(re-open with `Enter`). `config.toml` sets `sidebar_collapsed_mode = "hidden"` so a collapsed
sidebar disappears fully, and `hide_tab_bar_when_single_tab` hides the tab row until you have
more than one tab.

Set where new ticket sessions launch (default `~/Repos`; Claude narrows to the exact repo on
your first message):

```sh
export TICKETDECK_ROOT=~/Repos
```

### Prefer a shell function/alias instead of the script?

The `~/.local/bin/deck` script is canonical (it also self-heals the tab-#1
pinning described above). A minimal bash-function equivalent — reuses a live deck
and focuses it, but skips the numbering self-heal:

```sh
deck() {
  local root="${TICKETDECK_ROOT:-$HOME/Repos}"
  herdr status 2>/dev/null | grep -q 'status: running' || { herdr server >/dev/null 2>&1 & sleep 0.5; }
  herdr agent list 2>/dev/null | grep -q '"name":"deck"' || \
    herdr agent start deck --cwd "$root" -- ticketdeck >/dev/null 2>&1
  herdr agent focus deck >/dev/null 2>&1
  herdr
}
```

### Multiple Claude subscriptions (accounts)

If you have more than one Claude Code subscription — each logged into its own
config dir (`~/.claude` for the default, `~/.claude-<name>` for others, e.g.
`~/.claude-support`) — run a separate, fully isolated deck per account:

```sh
deck                     # default subscription (~/.claude)
deck --account support   # the 'support' subscription (~/.claude-support)
```

Each account gets its **own herdr workspace** (separate server socket +
`session.json`), so its ticket sessions burn *that* subscription's rate limits
and never collide with the default deck. Both decks show the **same Linear
tickets** (the Linear key is shared) — only which Claude subscription runs the
sessions differs.

**Telling the decks apart.** Every deck names itself — there is no unlabelled
deck. Three cues, in order of how far away you can read them:

| Cue | Where |
| --- | --- |
| OS window / tab title `deck ⦿ support` | your taskbar, without focusing the window |
| `⦿ <name>` badge in a **per-account color** | title bar, top left |
| both accounts' usage | title bar, active account first |

Name the default deck something better than `default` by exporting
`TICKETDECK_ACCOUNT` in your shell rc:

```sh
export TICKETDECK_ACCOUNT=matt   # labels the ~/.claude deck "matt"
```

The name is published into that config dir, so **every** deck calls the
subscription `matt` — the label and its accent color stay the same whichever
deck you're looking from.

**Seeing where the headroom is.** The title bar shows *every* subscription's
5h/7d usage, not just the one you're in — the active account on the title line
with its reset hint, the others on the line below:

```
TicketDeck  ⦿ matt  assigned · open only  ◷ 5h 94% (12m) · 7d 61%
            ⦿support 5h 12% · 7d 8%
```

So when one account is throttled you can see the other has room without
switching decks to go look.

**Seeing which account is on a ticket, and what it's doing.** Every ticket with
a session carries a `⦿` dot in the owning subscription's color, in the column
left of the status badge — including sessions running under the *other* accounts,
which this deck otherwise can't see. Move the cursor onto a row and the dot is
labelled with the account name:

```
          ⦿ ● working 4m    ZEN-3395  ●  fix shipworks sweep
▶ support ⦿ ● working 12m   ZEN-3401  ○  retry backoff
          ⦿ ↻ resumable     ZEN-3402  ○  order alert dedupe
```

The name only appears on the highlighted row (naming every row would cost the
title that much width on every line), but the column is reserved either way, so
moving the cursor never shifts the columns. A deck with one subscription doesn't
render the column at all.

The **badge is filled in for other decks' sessions too** — `● working`,
`◆ needs input`, `○ idle`, `↻ resumable` — so a ticket someone else's deck is
part-way through doesn't read as untouched. Two things differ from a badge for
one of this deck's own sessions:

- **It's colored like the deck, not like the status.** The words already say the
  state; what a glance needs from someone else's row is whose it is. So a remote
  `● working` is `support`-colored, not green.
- **The time is "since it last wrote", not "time in this state."** Locally the
  deck knows when a status changed. For another account it reads the transcript's
  mtime, which is the more useful number anyway: a `● working` session that last
  wrote 40m ago is wedged, not busy.

Ownership is read from each account's transcripts on disk plus its herdr
workspace, so an account whose deck isn't running still shows up (as `↻
resumable`). Live status needs that account's herdr server to be up. Refreshed
every 3 seconds, alongside this deck's own badges. When a ticket has been worked
under both accounts — a hand-off leaves the transcript behind in the source — the
dot names the live session, or the one that wrote most recently.

All of this applies to **project** rows too: a project's session carries the same
`⦿` dot and live badge, so you can see which subscription is working a project
without switching decks.

**One deck per ticket.** `⏎` on a ticket that another deck is *actively running*
stops and shows what's there instead of opening it:

```
Already open on another deck  ZEN-3401

  ⦿ support   ● working right now, last wrote 2m ago

  A session here would be a second one on the same ticket: both decks
  append to their own copy of one transcript, which then diverge with no
  way to merge them, and two agents work the ticket at once.

 ⏎  leave it to ⦿support
  p  open its PR here instead
  o  open it here anyway

  esc  cancel
  work it where it lives:  deck --account support
```

Both `⏎` and `esc` back out — a reflexive keypress must not be the thing that
forks a session — and the footer then reminds you which deck to launch. Only `o`
overrides. `t` (background `/triage`) goes through the same gate, because it
starts the session when there isn't one here.

Two cases that look similar but aren't gated: a session **this** deck already
runs (`⏎` re-attaches it, nothing forks) and another deck's **stopped** session
(nothing to collide with — the row badges it `↻` in that deck's color, and
opening it here starts fresh). To pick up a stopped session's context, hand it
over from the deck that has it with `H`.

The gate reads the same 3-second ownership poll the badges do, so a session
started elsewhere in the last couple of seconds can still slip past it. It
catches the mistake people actually make: opening a ticket another deck has been
working on for minutes.

**Handing a session to the other subscription.** `H` on a ticket — or on a
**project** — moves its session to another account, for when the current
subscription hits a limit mid-task. It stops the session here, copies its
transcript into the other account's config dir, and the session becomes
resumable in that deck. A project hands off exactly as a ticket does: the move
is keyed on the session id, which doesn't care which kind of work it holds.

Three things to know:

- **Context carries over; a live process does not.** Resuming replays the
  conversation, so an in-flight tool call is lost. Hand off between steps.
- **Never run one session in two accounts at once.** That's why `H` stops the
  session first — two accounts appending to their own copy of one transcript
  diverge with no way to reconcile them.
- **It refuses to destroy work.** If the target account already has *newer* work
  on that ticket, the hand-off fails instead of overwriting it. An older copy
  there is just stale, so a ticket can move back and forth freely.

- **Switching is non-destructive.** Detach one deck (`Ctrl+b q`) and launch the
  other; every background session in each workspace keeps running untouched. A
  session you started on `matt` finishes its work whether or not you're looking
  at it — attach the matt deck again later and it's right there.
- **Tool auth is shared.** The isolated workspace is an overlay of your real
  `~/.config` (symlinked through), so `gh`, `gcloud`, `git`, etc. use the same
  credentials in both decks; only Claude's own config dir differs.
- **First-time setup for an account:** log in to its config dir once —
  `CLAUDE_CONFIG_DIR=~/.claude-support claude /login` — then `deck --account
  support` works. (`ticketdeck --account support` does the same for the
  standalone, non-herdr path.)

## Modes / flags

```sh
ticketdeck                         # TUI, backend auto (herdr if installed)
ticketdeck --backend claude        # force built-in Claude path (foreground per ticket)
ticketdeck --backend herdr         # force herdr
ticketdeck --dry-launch            # Enter prints the launch command instead of running it
ticketdeck --demo                  # canned data, no Linear key
ticketdeck --demo --preview        # one styled frame, then exit
ticketdeck --root <dir>            # override where new sessions launch
ticketdeck --account <name>        # run as the ~/.claude-<name> subscription (see "Multiple Claude subscriptions")
```

Keys: `↑/↓` move · `PgUp/PgDn` page · `g`/`G` top/bottom · `enter` open · `r` refresh · `q` quit.

## Revert

```sh
herdr integration uninstall claude          # remove the Claude state hook
rm ~/.local/bin/deck ~/.local/bin/ticketdeck ~/.local/bin/herdr
# settings.json backup from install time: ~/.claude/settings.json.pre-herdr
```
