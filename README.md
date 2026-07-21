# TicketDeck

A terminal dashboard for your assigned [Linear](https://linear.app) tickets that launches and
re-attaches a [Claude Code](https://claude.com/claude-code) session per ticket — grouped by
priority, with live session status, linked PRs, and one-key status/assignee changes.

It runs on top of [herdr](https://github.com/ogulcancelik/herdr) (an agent-aware terminal
multiplexer) so you can start work on a ticket, detach and leave it running, and jump to
another — each ticket in its own tab.

## Quickstart

```sh
# 1. install ticketdeck + the deck launcher + herdr, all into ~/.local/bin
curl -fsSL https://raw.githubusercontent.com/hdtradeservices/ticketdeck/main/install.sh | bash

# 2. add your Linear key (read-only is enough; write scope only for status/assignee changes)
export LINEAR_API_KEY=lin_api_...        # https://linear.app/settings/api

# 3. launch
deck
```

Requires [Go](https://go.dev/dl) (to build) and a POSIX shell. Works on Linux and macOS.
Prefer to clone first? `git clone https://github.com/hdtradeservices/ticketdeck && cd ticketdeck && ./install.sh`.

`deck` opens the herdr workspace with the ticket list pinned to tab 1. See
[`SETUP.md`](SETUP.md) for the full setup, keybindings, and how the `deck` launcher works.

## Try it without a key

```sh
ticketdeck --demo               # TUI on canned sample data
ticketdeck --demo --preview     # one styled frame, then exit
ticketdeck --demo --dump        # plain-text grouped list
```

## Keys (in the deck)

| | |
|---|---|
| move · page · top/bottom | `↑`/`↓` (`j`/`k`) · `PgUp`/`PgDn` · `g`/`G` (wraps at the ends) |
| open / attach the ticket's session | `Enter` |
| description overlay | `d` (in it: `Enter` opens the session · `o` browser · `p` PR) |
| open ticket / linked PR in browser | `o` / `p` |
| send `/triage` to the ticket's session | `t` |
| change status (Done/Validate/Monitoring/Blocked/Cancel) | `s` → key → `y` |
| assign / reassign / unassign | `a` |
| open an ad-hoc (non-ticket) session | `n` |
| fold/unfold a priority section | `Space` · `←`/`→` |
| refresh · quit | `r` · `q` |

Full table (including herdr's own keys) in [`SETUP.md`](SETUP.md).

## How it works

- Fetches **assigned + open** issues (hides `completed`/`canceled`/`duplicate` — Done,
  Cancelled, Duplicate — but keeps `Validate`, a completed-type QA gate that's still
  actionable). Groups **priority → status**, newest-updated first.
- Refreshes every ~60s with jitter and keeps the last good list on API error.
- **Session badges** per ticket: `●` working · `◆` needs input · `○` idle · `✓` done ·
  `↻` resumable (an on-disk session you can reattach) · `·` none. Working tickets are dimmed
  so your eye goes to what needs you. Linked PRs show a `⇄` flag; validation labels show a
  `⚑` flag.
- **Enter** launches `claude` bound to a deterministic per-ticket session id (resumes if one
  exists), seeding the ticket's identity via `--append-system-prompt` — **no auto-submitted
  prompt and no model turn**: the first token spend is always your first message.
- **Backends** (`--backend claude|herdr|auto`, default `auto`): `herdr` gives the
  detach/re-attach + tab-per-ticket workflow; `claude` drives the `claude` CLI directly
  (foreground per ticket). `auto` uses herdr when it's installed.

## Writes to Linear

TicketDeck is read-only by default. The status-change (`s`) and assignee (`a`) hotkeys are the
only writes, are confirm-gated, and need a **write-scoped** `LINEAR_API_KEY`. Moving a ticket
to a terminal state (Done/Cancel) also closes its Claude session (the transcript persists).

Some defaults are tuned for the author's Linear workspace — the status targets
(Done/Validate/Monitoring/Blocked), the `validation-*` labels, and the `/triage` command.
They're easy to adjust in the source.

## Credits & license

TicketDeck is MIT-licensed (see [`LICENSE`](LICENSE)). It shells out to two separately-installed
tools it does not bundle: [Claude Code](https://claude.com/claude-code) and
[herdr](https://github.com/ogulcancelik/herdr) (AGPL-3.0). Built with
[Bubble Tea](https://github.com/charmbracelet/bubbletea).
