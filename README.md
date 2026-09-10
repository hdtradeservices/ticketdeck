# TicketDeck

A terminal dashboard for your [Linear](https://linear.app) work that launches and re-attaches
a [Claude Code](https://claude.com/claude-code) session per **project** and per **ticket** —
your projects up top with their progress, the loose tickets grouped by priority below, with
live session status, linked PRs, and one-key status/assignee changes.

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

## Staying up to date

The deck checks for a newer release on startup (cached daily, non-blocking) and shows a
`⬆ vX.Y available` banner when you're behind. To update:

```sh
ticketdeck update      # pulls the latest release (re-runs the installer)
deck                   # restarts the deck onto it
```

Run `deck` after updating. `ticketdeck update` replaces the binary on disk, but a deck already
running in a herdr pane keeps the old one until its process restarts — so the update looks like
it did nothing. `deck` now spots that stale process and restarts it in place, keeping the agent
and tab #1.

`ticketdeck --version` prints the running build. Releases are cut by tagging (`vX.Y.Z`), which
builds prebuilt binaries for Linux/macOS — so teammates install and update without needing Go.

**Maintainers — cut a release:**

```sh
git tag v0.2.0 && git push origin v0.2.0    # GitHub Actions builds binaries + publishes the release
```

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
| the ticket's **investigation** or **plan**, in that overlay | `i` · `P` — the `/investigate` and `/plan` write-ups, pulled from the ticket's Linear comments. Same key again returns to the description; `r` re-reads the comments. |
| open ticket in browser | `o` |
| open linked PR — picker when there are several | `p` (in it: `⏎` open · `1`-`9` open that one · `a` open all) |
| show the ticket's description **from inside its session** | `Ctrl+b` then `i` (popup) |
| `/triage` a ticket in the background (starts its session if needed) | `t` |
| change status (Done/Validate/Monitoring/Blocked/Cancel) | `s` → key → `y` |
| change priority (Urgent/High/Medium/Low/None) | `P` → key |
| assign / reassign / unassign | `a` |
| open an ad-hoc (non-ticket) session | `n` |
| fold/unfold a priority section, the **Projects** section, or one project's tickets | `Space` · `←`/`→` |
| refresh · quit | `r` · `q` |

On a **project** row the same keys act on the project: `⏎` opens its session, `d` its
detail, `o` the project page, `p` the PRs across its tickets, `H` hands it to another
account, and the three writes resolve to the project's own fields — `s` status, `P`
priority, `a` **lead**. `t` is the exception: `/triage` triages one issue, so it stays
ticket-only.

Full table (including herdr's own keys) in [`SETUP.md`](SETUP.md).

## How it works

- **Projects come first.** The deck opens with a `PROJECTS` section listing the Linear
  projects that are yours — the ones you **lead** — each with a progress bar, its
  open-ticket count, the PRs across its tickets, and an `⚑ overdue` / `⚑ at risk` flag.
  Every project row **opens a Claude session of its own** (`⏎`), with the same badges,
  account dot, and hotkeys a ticket row has.

  A ticket that belongs to a project is **removed from the priority sections below** and
  hangs off its project instead, folded away by default (`Space` on the project unfolds
  it). So the priority sections become just the loose tickets — the work that isn't
  already tracked by a project.

  Projects are **ordered by priority**, the way the ticket sections below them are —
  Urgent first, no-priority last — with live work ahead of planned work within one
  priority, then the nearest target date. A colored tick before the name is the row's
  priority, since the section has no priority headers to say so.

  A project moved to **Completed lingers struck-through for 12h**, like a done ticket,
  then drops off the deck for good — including one that still holds open tickets of
  yours. Those tickets aren't lost with it: they go back to the priority sections they
  came from. Cancelling a project drops it immediately, same as a cancelled ticket.

  Nothing gets lost in the move: if one of your tickets sits in a project you *don't*
  lead, that project still gets a row (dimmed, progress `n/a`) so the ticket is still
  reachable. Progress never rounds up — Linear reports 0.999 for a project with one
  ticket left, and the deck shows `99%`.
- A project session is bound to the project's **slug**, not its name, so renaming a
  project in Linear doesn't orphan its conversation. Its identity prompt is seeded with
  the project's summary, progress, target date, and the keys of your open tickets in it
  — so the session starts knowing its own scope. As with tickets, no prompt is
  auto-submitted: the first token spend is your first message.
- Fetches **assigned + open** issues (hides `completed`/`canceled`/`duplicate` — Done,
  Cancelled, Duplicate — but keeps `Validate`, a completed-type QA gate that's still
  actionable). Groups **priority → status**, newest-updated first, and auto-folds priority
  sections that hold none of your **top 10** tickets so the view stays focused (re-applied on
  refresh).
- Refreshes every ~60s with jitter and keeps the last good list on API error.
- **Session badges** per ticket: `●` working · `◆` needs input · `○` idle · `✓` done ·
  `↻` resumable (an on-disk session you can reattach) · `·` none. Working tickets are dimmed
  so your eye goes to what needs you. Linked PRs show a `⇄` flag; validation labels show a
  `⚑` flag.
- **Blocked tickets** show what's holding them up: a red `⛔ ZEN-1234, …` note lists the open
  tickets a Blocked ticket is blocked by (also in the `d` detail view).
- **Multiple PRs per ticket** — a ticket's work usually splits across repos, so `p` opens a
  picker rather than guessing: each row shows the PR's state, `repo#number`, and title,
  ordered most-actionable-first (open → draft → merged → closed). `⏎` opens the selected one,
  `1`-`9` opens that row directly, and **`a` opens all of them** for reviewing the whole
  change. A single-PR ticket still opens straight away. The row's `⇄` icon carries the count
  (`⇄3`).
- **Recently-done tickets linger**: a ticket moved to Done stays in the deck for 12h,
  rendered struck-through and dimmed, then drops off. It doesn't count toward the top-10 focus.
- **Unblock cascade**: marking a ticket **Done** sends every still-open ticket it was
  *blocking* to its team's **Triage** state, so newly-unblocked work resurfaces (a Linear
  write; needs a write-scoped key).
- **Enter** launches `claude` bound to a deterministic per-ticket session id (resumes if one
  exists), seeding the ticket's identity via `--append-system-prompt` — **no auto-submitted
  prompt and no model turn**: the first token spend is always your first message. Each ticket
  opens in its own tab titled `KEY  short title` (not a bare id).
- **Description from inside a session** — `Ctrl+b i` opens a popup with the current ticket's
  rendered description, resolved from whichever ticket pane you invoked it in (so you don't have
  to jump back to the deck). It's a read-only Linear fetch — no model turn. The deck's own `d`
  overlay shows the same for the cursor ticket.
- **Multiple Claude subscriptions** — if you have more than one Claude account
  (each in its own config dir), run an isolated deck per subscription:
  `deck --account support` uses `~/.claude-support`, gets its own herdr workspace
  (own server + tabs), and shows a `⦿ support` badge with that account's usage
  bar. Both decks show the same Linear tickets; switching is just detach + launch
  the other, and background sessions keep running. Tool auth (`gh`/`gcloud`/…) is
  shared. See [`SETUP.md`](SETUP.md#multiple-claude-subscriptions-accounts).
- **Which account is on a ticket, and what it's doing** — every ticket with a session carries
  a `⦿` dot in the owning subscription's color, and the highlighted row spells out the name.
  That includes sessions running under the *other* accounts, which a deck otherwise can't see:
  ownership is read from each account's transcripts and its herdr workspace. One subscription,
  no column.

  A session another deck holds badges its state right on the row — `● working`, `◆ needs
  input`, `○ idle`, `↻ resumable` — with how long since it last wrote, so a peer that's wedged
  looks different from one that's busy. Those badges are colored by **deck** rather than by
  status: the words already say the state, and what a glance needs from someone else's row is
  whose it is. Refreshed every 3s.
- **One deck per ticket, and per project** — `⏎` (and `t`) on a ticket another deck is actively running stops
  and says so instead of opening it. A second session would fork the ticket: the session id
  comes from the ticket key alone, so both decks would append to their own copy of one
  transcript and diverge with no way to merge them — and two agents would work the ticket at
  once. `⏎`/`esc` leaves it alone, `p` opens its PR instead, and `o` overrides. A session
  *this* deck already runs is not a conflict (it re-attaches), nor is a stopped one elsewhere
  (nothing to collide with — the row badges it `↻` in that deck's color). Project rows take
  the same gate for the same reason: a project's session id comes from its slug alone.
- **Claude usage** — the title bar shows your Claude 5-hour and 7-day rate-limit
  utilization (`◷ 5h 52% · 7d 42%`), color-coded, with a rough reset countdown. Same source
  as Claude Code's status line (the OAuth usage endpoint); it's a metadata read, so it does
  **not** spend model tokens. Shown only for OAuth logins (not API-key setups). Every deck on
  the machine shares one reading per account through `~/.ticketdeck/usage.json`, refreshed at
  most every 5 minutes — polling per deck earned 429s and blanked the line. When a reading is
  missing the row says why: `rate limited` clears itself, `token expired` means that account's
  token needs a refresh, which only happens when you next run Claude Code under it. A `~` after
  the numbers means they're more than two intervals old.
- **Backends** (`--backend claude|herdr|auto`, default `auto`): `herdr` gives the
  detach/re-attach + tab-per-ticket workflow; `claude` drives the `claude` CLI directly
  (foreground per ticket). `auto` uses herdr when it's installed.

## Writes to Linear

TicketDeck is read-only by default. The status (`s`), priority (`P`) and assignee (`a`)
hotkeys are the only writes, are confirm-gated, and need a **write-scoped**
`LINEAR_API_KEY`. Moving a ticket to a terminal state (Done/Cancel) also closes its Claude
session (the transcript persists).

On a project row those three keys write the project instead: its **status**
(Planned / In Progress / Blocked / Completed / Cancel), its **priority**, and its **lead**.
Completing or cancelling a project closes its session too.

Some defaults are tuned for the author's Linear workspace — the ticket status targets
(Done/Validate/Monitoring/Blocked), the project status targets, the `validation-*` labels,
and the `/triage` command. They're easy to adjust in the source.

## Credits & license

TicketDeck is MIT-licensed (see [`LICENSE`](LICENSE)). It shells out to two separately-installed
tools it does not bundle: [Claude Code](https://claude.com/claude-code) and
[herdr](https://github.com/ogulcancelik/herdr) (AGPL-3.0). Built with
[Bubble Tea](https://github.com/charmbracelet/bubbletea).
