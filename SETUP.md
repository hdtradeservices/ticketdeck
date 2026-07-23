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

**Layout:** the deck lives in **tab 1**; each ticket you open lands in **its own tab**, so only
one thing is visible at a time and tickets never pile up as split panes. Open a ticket → you're
taken to its tab → work → jump back to the deck (tab 1) → open the next ticket. Agents keep
running in their background tabs.

**TicketDeck keys** (inside the deck):

| goal | key |
|---|---|
| move / page / top-bottom | `↑`/`↓` (`j`/`k`) · `PgUp`/`PgDn` · `g`/`G` |
| open the ticket (launch/attach its session) | `Enter` — the first time, a reminder shows how to get back (`Ctrl+b 1`); `⏎` proceeds, `d` proceeds and never shows it again |
| show the ticket's **description** (rendered markdown) | `d` (in the overlay: `Enter` opens the session, `o` browser, `p` PR, `↑`/`↓` scroll, `esc` closes) |
| open the ticket in the **web browser** | `o` |
| open the ticket's **linked PR** in the browser | `p` (shown with a `⇄` icon, colored by PR state) |
| **change status** → Done / Validate / Monitoring / Blocked / Cancel | `s`, then `d`/`v`/`m`/`b`/`c`, then `y` to confirm — **writes to Linear; needs a write-scoped `LINEAR_API_KEY`**. Moving to a terminal state (**Done / Cancel**) also **closes that ticket's Claude session** if one is running (transcript persists — resumable), then returns focus to the deck (not the neighbor tab). Moving to **Done** also runs the **unblock cascade**: every still-open ticket this one was *blocking* is moved to its team's **Triage** state. |
| **change priority** → Urgent / High / Medium / Low / None | `P`, then `u`/`h`/`m`/`l`/`0` — **write; write-scoped key** |
| **assign / reassign / unassign** | `a` opens a picker — type to filter people, `↑`/`↓` select, `⏎` assign (top row = **Unassign**), `esc` cancel — **writes to Linear; write-scoped key** (reassigning away from you drops the ticket off the list) |
| open an **ad-hoc Claude session** not tied to any ticket (own tab) | `n` |
| **`/triage` in the background** | `t` — starts the ticket's session in its own (unfocused) tab if it isn't running, submits `/triage`, and keeps you on the deck (**runs a Claude turn**; herdr backend). On an "Other sessions" row it just submits `/triage` to that session. |
| **fold/unfold** a priority section (collapsed shows a ticket count) | `Space` (toggle) · `←` collapse · `→` expand |
| refresh | `r` |

An **"Other sessions"** section at the bottom lists live Claude sessions not shown as a
ticket badge — ad-hoc `n` sessions and sessions for tickets that dropped off the list
(Done/Cancelled). With the cursor on one: `Enter` switches to it, **`x` closes it** (its
transcript persists, so it stays resumable).
| quit | `q` (standalone) — **under the `deck` launcher `q` is disabled** so it can't orphan the deck; leave herdr with `Ctrl+b q` (detach, keeps sessions running), or `Ctrl+c` as a hard exit |

**Icons / badges:**

- **Priority** group headers are color-coded: Urgent (red) · High (orange) · Medium (yellow) · Low (blue) · No priority (gray), always showing a `(count)`.
- **Session** badge per ticket: `●` working (green) · `◆` needs input (amber) · `○` idle (cyan) · `✓` done · `↻` **resumable** (an on-disk session you can reattach — shown even right after a fresh start, before any agent is running) · `·` none yet.
- **Top-10 focus** — priority sections are auto-folded unless they hold one of your top 10 tickets (by priority, then status, then recency), and this is re-applied on each refresh. So if your top 10 are all Urgent, only Urgent stays open; if they span Urgent/High/Medium, those stay open and lower sections fold (shown as `▸ Low (3)`). Expand any folded section with `→`/`Space` (it re-folds on the next refresh).
- **Working tickets are dimmed** — a ticket whose session is actively `working` renders in faint gray (no bright id/title), so your eye is drawn to the tickets that still need you (needs-input, idle, resumable, untouched) rather than the ones already in progress. Move the cursor onto one and it still highlights normally.
- **Time-in-state** — live sessions (working / needs-input / idle) show how long they've held that state once it's been ≥1 minute, e.g. `◆ needs input 20m` or `● working 45m`. Useful for spotting a session parked a while (waiting on CI, or one that's needed input for a bit). The detail view (`d`) spells it out. Note: it's measured from when the deck first saw the state, not necessarily the session's true start.
- **PR** `⇄` marks a ticket with a linked pull request, colored by state: merged (violet) · open (green) · closed (red) · draft/unknown (gray). Press `p` to open it.
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

## Modes / flags

```sh
ticketdeck                         # TUI, backend auto (herdr if installed)
ticketdeck --backend claude        # force built-in Claude path (foreground per ticket)
ticketdeck --backend herdr         # force herdr
ticketdeck --dry-launch            # Enter prints the launch command instead of running it
ticketdeck --demo                  # canned data, no Linear key
ticketdeck --demo --preview        # one styled frame, then exit
ticketdeck --root <dir>            # override where new sessions launch
```

Keys: `↑/↓` move · `PgUp/PgDn` page · `g`/`G` top/bottom · `enter` open · `r` refresh · `q` quit.

## Revert

```sh
herdr integration uninstall claude          # remove the Claude state hook
rm ~/.local/bin/deck ~/.local/bin/ticketdeck ~/.local/bin/herdr
# settings.json backup from install time: ~/.claude/settings.json.pre-herdr
```
