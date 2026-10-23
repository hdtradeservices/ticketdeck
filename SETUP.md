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
| open the ticket (launch/attach its session) | `Enter` — the first time, a reminder shows how to get back (`Ctrl+b 1`); `⏎` proceeds, `d` proceeds and never shows it again. It opens on the account the router picks — see [Where a session opens](#where-a-session-opens). If **the ticket's session runs somewhere this deck can't drive** (an older deck's workspace, a plain terminal), it stops and shows that session's live state instead of forking it. |
| show the ticket's **description** (rendered markdown) | `d` (in the overlay: `Enter` opens the session, `o` browser, `p` PR, `↑`/`↓` scroll, `esc` closes) |
| show the ticket's **investigation** or **implementation plan** | `i` / `P`, inside the description overlay. Those write-ups live in the ticket's Linear comments, not its description, so the deck fetches that ticket's comments the first time you press one of these keys and reuses them after. It shows the newest `## Investigation summary` (`/investigate`) or `## Implementation plan` (`/plan`) comment, with a line naming who wrote it and when. Pressing the same key again goes back to the description, as does `esc`; `d` closes the overlay outright. Says so plainly when the ticket has no such comment yet — and `r` re-reads the comments, for when the agent posts its write-up while you're looking. |
| open the ticket in the **web browser** | `o` |
| open the ticket's **linked PR** in the browser | `p` — one PR opens straight away. Several open a **picker** (a ticket's PRs usually span different repos): rows are ordered most-actionable-first and show state + `repo#number` + title; `↑`/`↓` select, `⏎` open, `1`-`9` open that one directly, **`a` opens all of them**, `esc` cancels. |
| **change status** → Done / Validate / Monitoring / Blocked / Cancel | `s`, then `d`/`v`/`m`/`b`/`c`, then `y` to confirm — **writes to Linear; needs a write-scoped `LINEAR_API_KEY`**. Moving to a terminal state (**Done / Cancel**) also **closes that ticket's Claude session** if one is running (transcript persists — resumable), then returns focus to the deck (not the neighbor tab). Moving to **Done** also runs the **unblock cascade**: every still-open ticket this one was *blocking* is moved to its team's **Triage** state. |
| **change priority** → Urgent / High / Medium / Low / None | `P`, then `u`/`h`/`m`/`l`/`0` — **write; write-scoped key** |
| **assign / reassign / unassign** | `a` opens a picker — type to filter people, `↑`/`↓` select, `⏎` assign (top row = **Unassign**), `esc` cancel — **writes to Linear; write-scoped key** (reassigning away from you drops the ticket off the list) |
| open an **ad-hoc Claude session** not tied to any ticket (own tab) | `n` — type a name (optional), `Tab` to pick the account, `⏎` to open. See [Ad-hoc sessions](#ad-hoc-sessions). |
| **`/triage` in the background** | `t` — starts the ticket's session in its own (unfocused) tab if it isn't running, submits `/triage`, and keeps you on the deck (**runs a Claude turn**; herdr backend). On an "Other sessions" row it just submits `/triage` to that session. Gated the same way `Enter` is when another deck is running the ticket. |
| **fold/unfold** a priority section (collapsed shows a ticket count) | `Space` (toggle) · `←` collapse · `→` expand — on the `PROJECTS` header this folds the whole section; on a project row (or one of its tickets) it folds that project's ticket list |
| **search / filter the list** | `/` — type to filter tickets by key or title (live, case-insensitive; all matching groups expand). `⏎` keeps the filter and returns to list nav; `esc` clears it. While a filter is applied the footer shows `filter "…" · esc clear`. |
| **move the session to another Claude account** | `H` — stops it, copies its conversation, and resumes it on the chosen account in a new tab. With no session yet, opens it on the chosen account. `⏎` confirms, `1`-`9` picks, `esc` cancels. Only offered with more than one account — see [Moving a session by hand](#moving-a-session-by-hand). |
| **accounts** | `A` — every account's usage, sessions and state, in the order new sessions go to them; `⏎`/`p` pins new sessions to one, `a` back to automatic, `m` turns auto-move on or off. |
| **bring in sessions from older decks** | `M` — sessions still running in a per-account deck's workspace; `⏎` brings one in on the same account, `a` all of them. See [Sessions from older decks](#sessions-from-older-decks). |
| **jump to what needs you** | `]` / `[` — the next or previous session waiting on you: a question or permission prompt, a turn nobody has read, a usage limit, an API error. Unfolds whatever hides it. The title bar counts them (`◆ 3 need you`). |
| **every key** | `?` |
| refresh | `r` — a manual refresh. Session badges also refresh on their own every few seconds, and the instant the deck regains focus, so you don't have to. |

An **"Other sessions"** section at the bottom lists live Claude sessions not shown as a
ticket badge — ad-hoc `n` sessions and sessions for tickets that dropped off the list
(Done/Cancelled) — plus sessions in older decks' workspaces, and the ad-hoc sessions the
deck remembers that aren't running. With the cursor on one: `Enter` switches to it (brings
it in, or resumes it), **`x` closes it** (its transcript persists; an ad-hoc one also drops off
the deck, running or stopped), **`R` renames it**, `H` moves it to another account.
| quit | `q` (standalone) — **under the `deck` launcher `q` is disabled** so it can't orphan the deck; leave herdr with `Ctrl+b q` (detach, keeps sessions running), or `Ctrl+c` as a hard exit |

**Icons / badges:**

- **Priority** group headers are color-coded: Urgent (red) · High (orange) · Medium (yellow) · Low (blue) · No priority (gray), always showing a `(count)`.
- **Session** badge per ticket: `●` working (green) · `◐` background (lavender — the lead is idle but a subagent, shell command or monitor it started is still running) · `◆` needs input (amber) · `◉` your turn (it finished a turn nobody has looked at since) · `○` idle (cyan) · `⊘` limit (red — its account is out, followed by when it gets it back) · `✗` error (it stopped on an API error: a lapsed login, an unreachable API) · `↻` **resumable** (an on-disk session you can reattach — shown even right after a fresh start, before any agent is running) · `·` none yet.
- **Status line** — the line above the key hints says what the highlighted row's session is doing, in more words than the badge has room for: its account, what it last did, how full its context is, what it is waiting on, how it stopped. On a row with no session it says where `⏎` would open it, and why.
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
config dir (`~/.claude` for the primary, `~/.claude-<name>` for the others, e.g.
`~/.claude-support`) — **one deck runs all of them**:

```sh
deck        # every subscription on this machine
```

Each session's tab gets its own account's `CLAUDE_CONFIG_DIR`, so its usage lands
on that subscription, and the deck picks the account for you. Every account
appears on the line under the title — its 5-hour and 7-day usage and how many
sessions it runs — and the one a new session would open on is underlined:

```
TicketDeck  assigned · open only
  ⦿ m0 5h 1% · 7d 56% ●2   ⦿ matt 5h 52% · 7d 42% ●3   ⦿ support out until 2:50pm ●1
```

**Naming the accounts.** The primary account takes its name from the first line
of `~/.claude-accounts` (`rdeck` reads the same file), else from
`TICKETDECK_ACCOUNT`, else `default`; the others are named after their
directory. Colors are assigned by sorted name, so every deck agrees on them.

**First-time setup for an account:** log it in once —
`CLAUDE_CONFIG_DIR=~/.claude-support claude /login`. The deck notices a new
account within a minute, no restart needed.

#### Where a session opens

`⏎` resumes a session on the account it already lives on — moving costs a
transcript copy and a replayed conversation, so it needs a reason. A new session
goes to the account with the most room:

- An account is as full as its **tighter** window: 5% of the 5-hour window buys
  nothing on a subscription at 98% of its week.
- Accounts within ten points of each other go to the one running **fewer
  sessions**, because sessions on one account share its budget.
- An account is **skipped** when its limit refused one of its sessions (until
  the limit resets), a window is spent, its login lapsed, or nobody is signed in.
- One whose usage can't be read — usually an expired token, which is what an
  account nobody has used for hours looks like — still takes work, ranked as if
  it had moderate room; opening a session under it refreshes the token.
- A **nearly spent** account (5-hour ≥ 90% or 7-day ≥ 95%) is used only when
  nothing has more room.
- A session that lives on an account that is out resumes somewhere with room;
  its conversation is copied over first.

The line above the key hints says where `⏎` would open the highlighted ticket,
and why, before you press it. `H` on a ticket with no session yet opens it on an
account you choose; `n` takes `Tab` to pick one for an ad-hoc session. `A` shows
every account in the order the router ranks them, and pins new sessions to one
of them if you'd rather choose (`⏎`/`p` pins, `a` goes back to automatic).

#### When an account runs out

A session cut off by its account's usage limit **moves to the account with the
most room and carries on**: the deck stops it, copies its conversation, resumes
it on the other account in a new tab, and tells it why it stopped and to
continue. A notification says so, wherever you are.

- It acts once per cut-off, and only when the limit has ended the turn — the
  transcript records the refusal, so nothing is in flight to lose. A background
  task the session had running stops with it, and the session is told so.
- Claude puts up a what-now menu when it hits a limit; the deck treats a session
  sitting on that menu exactly like one that went idle.
- When no account has room, the session waits, and once its limit resets the
  deck dismisses the menu and tells it to continue where it is (within half an
  hour of the reset — a cut-off left alone since yesterday stays alone).
- A session whose account's login lapsed moves the same way: there is no reset
  to wait for.
- If the session stops at a prompt of its own when it starts on the new account
  — a folder that account hasn't trusted, say — the deck says so, and sends the
  continue once you've answered it.

Turn it off with `m` in the `A` overlay, or `TICKETDECK_AUTO_MOVE=off` for a
deck that must never act on its own. `TICKETDECK_CONTINUE_PROMPT` replaces what
a moved session is told.

#### Moving a session by hand

`H` on a ticket, a project, or an Other sessions row moves its session to
another account, in place: the tab closes, the conversation is copied, and it
resumes in a new tab under the new account. On a session that isn't running it
only copies the conversation, and the next `⏎` resumes it there.

- **The conversation carries over; a running process does not.** An in-flight
  tool call is lost, so the overlay warns when the session is working.
- **It refuses to destroy work.** If the target account already has *newer*
  work on that session, the move fails instead of overwriting it.
- It refuses an account nobody is signed in to, and says how to log it in.

#### Sessions from older decks

Before one deck ran every account, each subscription had its own deck
(`deck --account NAME`, or `rdeck NAME` on a remote box), each with its own herdr
workspace. Sessions those decks started keep running there. This deck finds
every herdr server on the machine and shows those sessions too — the title bar
counts them (`⇣ 3 in other decks`), and their rows say which deck they are in.

`M` lists them; `⏎` brings one in, `a` brings them all. Bringing a session in
stops it there and resumes it in this deck **on the same account** — nothing is
copied and nothing forks. A busy one is queued and comes in once its turn ends.
`⏎` on one of their rows in Other sessions does the same and switches to it.

Once an old deck's workspace is empty, stop it:
`herdr session stop <name>` (a named session) or close it from its own window.
`deck --account NAME` still works for anyone who wants that layout; that deck
runs every account too, in its own workspace.

**One session per ticket.** `⏎` (and `t`) on a ticket whose session runs
somewhere this deck can't drive stops and says so instead of opening a second
one: both would append to their own copy of one transcript, which then diverge
with no way to merge them. `⏎`/`esc` leaves it alone, `b` brings it into this
deck, `p` opens its PR, and `o` opens it here anyway.

#### Ad-hoc sessions

`n` asks for a name (optional) and the account (`Tab`; automatic by default),
then opens the session under its own id. The deck remembers it in
`~/.ticketdeck/sessions.json` — its id, name and account — so after a herdr
restart it is listed under Other sessions as resumable, by name, on the
account it ran on. `R` renames one: the deck's name for it, its herdr agent
name and tab, and Claude's own title (so `/resume` lists it the same way). `x`
closes it and the deck forgets it, running or stopped (its conversation stays on
disk for `claude --resume`).

An ad-hoc session Claude has titled shows that title until you name it.

#### From a shell

`ticketdeck sessions` prints every account's headroom and every session the
deck would show — which account runs it, which deck's workspace holds it, its
status and what it last did — without opening the deck. `rdeck --status` prints
it for a remote box.

`ticketdeck doctor` runs the deck's self-check and prints every result: each
account's session registry against `claude agents --json`, whether live
sessions' transcripts are where the deck looks and still parse, whether the
usage endpoint still answers in the shape the deck knows, herdr's version, and
how each account's setup differs from the deck's own. It exits non-zero when a
check fails. The deck runs the same checks at start and every ten minutes, and
names a failure under its title.

#### Keeping contexts small

Every turn re-reads a session's whole context, so a session at 600k tokens
spends its account several times faster than one at 100k for the same work.
The deck flags 300k and up on the row (`◔`), and `C` compacts the highlighted
session — with an optional focus, once its turn is over. Claude Code's own
controls, per account (each config dir has its own `settings.json`):

- `"autoCompactWindow": 400000` compacts at that size instead of near the
  1M-token limit Opus 5 and later run with by default. `/autocompact 400k`
  saves the same per model (Claude Code 2.1.288 or later).
- Idle compaction (Claude Code 2.1.290 or later, on by default) compacts a
  session of 200k tokens or more that has sat idle about 54 minutes, just
  before its one-hour prompt cache expires, so the next turn starts from a
  summary instead of re-reading everything uncached.
  `CLAUDE_CODE_IDLE_COMPACT_MIN_TOKENS` changes the threshold.

`A` shows each account's auto-compact window next to its other settings.

## 5. A deck on another box: `rdeck`

`rdeck` attaches to a TicketDeck running somewhere else — a cloud workstation,
say — and leaves it running when you detach. It brings up the remote herdr
server with the right Claude account, starts TicketDeck in it if it isn't
already there, then attaches from here.

```sh
rdeck                    # the deck on the box — it runs every account there
rdeck --status           # report remote state and every account's sessions, attach nothing
rdeck support            # the older per-account deck for the 'support' subscription
rdeck support --restart  # rebuild that session server first (closes its panes)
```

Detaching (`Ctrl+b q`) leaves the server and every ticket session running on the
box. Set `RDECK_HOST` to the ssh target (default `zentail-workbench`).

**One deck, every account.** Plain `rdeck` attaches to the deck in the box's
default herdr session, and that deck runs sessions under every account on the
box. The per-account decks `rdeck <name>` used to be the only way to reach live
in named herdr sessions of the same name; they still work, and the main deck
finds their sessions — `M` brings them in (see
[Sessions from older decks](#sessions-from-older-decks)). Once one is empty,
stop it on the box with `herdr session stop <name>`.

Why named sessions: `herdr --remote` picks a remote server by session name and
has no way to name a socket, so the `XDG_CONFIG_HOME` overlay `deck --account`
uses locally would be unreachable from here.

**A server the bridge started is not usable.** If you run `herdr --remote` by
hand and let it start a server for you, that server has a bare environment: no
`CLAUDE_CONFIG_DIR`, no `LINEAR_API_KEY`, no `~/.local/bin` on `PATH`. The deck
either can't start at all or starts against the wrong subscription. `rdeck`
checks for this and tells you to rerun with `--restart`.

## Modes / flags

```sh
ticketdeck                         # TUI, backend auto (herdr if installed)
ticketdeck --backend claude        # force built-in Claude path (foreground per ticket)
ticketdeck --backend herdr         # force herdr
ticketdeck --dry-launch            # Enter prints the launch command instead of running it
ticketdeck --demo                  # canned data, no Linear key
ticketdeck --demo --preview        # one styled frame, then exit
ticketdeck --root <dir>            # override where new sessions launch
ticketdeck --account <name>        # make ~/.claude-<name> this deck's own account (see "Multiple Claude subscriptions")
ticketdeck sessions                # every account's headroom and every session, without the deck
TICKETDECK_AUTO_MOVE=off ticketdeck   # never move a session on its own
TICKETDECK_CONTINUE_PROMPT='…'        # what a session moved off a spent account is told
```

Keys: `↑/↓` move · `PgUp/PgDn` page · `g`/`G` top/bottom · `enter` open · `r` refresh · `q` quit.

## Revert

```sh
herdr integration uninstall claude          # remove the Claude state hook
rm ~/.local/bin/deck ~/.local/bin/ticketdeck ~/.local/bin/herdr
# settings.json backup from install time: ~/.claude/settings.json.pre-herdr
```
