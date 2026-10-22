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
| move the session to another Claude account — or, with no session yet, open it on one | `H` |
| accounts: usage, sessions, pin new sessions to one, auto-move on/off | `A` |
| bring in the sessions still running in older per-account decks | `M` |
| open an ad-hoc (non-ticket) session — name it, pick its account with `Tab` | `n` |
| rename an ad-hoc session · close it (`x` again on a stopped one forgets it) | `R` · `x` |
| jump to the next / previous session waiting on you (unfolds what hides it) | `]` · `[` |
| fold/unfold a priority section, the **Projects** section, or one project's tickets | `Space` · `←`/`→` |
| every key · refresh · quit | `?` · `r` · `q` |

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
- **Session badges** per ticket: `●` working · `◐` background (the lead is idle but a
  subagent, shell command or monitor it started is still running) · `◆` needs input ·
  `◉` your turn (it finished a turn you haven't looked at) · `○` idle · `⊘` out of usage on
  its account, with when it gets it back · `✗` stopped on an API error (login expired, API
  unreachable) · `↻` resumable (an on-disk session you can reattach) · `·` none. Working
  tickets are dimmed so your eye goes to what needs you. Linked PRs show a `⇄` flag;
  validation labels show a `⚑` flag.
- **The line above the keys** says what the highlighted row's session is doing: which
  account it is on, what it last did (`last: Bash: go test ./...`), how full its context is,
  what it is waiting on, and how it stopped. A row with no session says where `⏎` would open
  it, and why — so the account choice is visible before you act on it.
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
- **One deck runs every Claude account.** If you have several subscriptions — each in its
  own config dir, `~/.claude` and `~/.claude-<name>` — one `deck` runs sessions under all of
  them. Each session's tab gets its account's `CLAUDE_CONFIG_DIR`, so its usage lands on that
  subscription. The line under the title shows every account's 5-hour and 7-day usage and
  how many sessions it runs; the one a new session would open on is underlined. See
  [`SETUP.md`](SETUP.md#multiple-claude-subscriptions-accounts).
- **It picks the account the way `claude-pick` does.** `⏎` resumes a session on the account
  it already lives on. A new one goes where `claude-pick` (in the shared `~/claude` repo)
  would send it: to the weekly allowance that renews soonest, since whatever is left at the
  reset is lost; away from an account whose 5-hour limit would stop the session within the
  next few hours; and spread across accounts by how busy each one's sessions are. Both read
  the same `CLAUDE_PICK_*` settings and the same pending-launch markers, so a burst from
  either lands on different accounts. The deck ports the score rather than running
  `claude-pick`, which refreshes an expired login by spending a small model turn. An account
  that is out — its limit refused a session, a window is spent, or nobody is signed in — is
  skipped, and so is one whose config dir holds a `.ticketdeck-hide` file (a background
  worker's seat). `A` shows the ranking and why the next account is next, and pins new
  sessions to one account if you'd rather choose.
- **It keeps going when an account runs out.** When a session's account hits its usage
  limit, the deck moves the session to the best-placed account and tells it to carry on — on
  its own, once per cut-off, and only after the limit has ended the turn. With nowhere to go
  it waits, and continues where it is once the limit resets. A lapsed login is handled the
  same way. `A` turns this off; `TICKETDECK_AUTO_MOVE=off` does too.
- **`H` moves a session by hand**, in place: the tab closes, the conversation is copied to
  the other account, and it resumes in a new tab. What carries over is the conversation, not
  the process. A moved session keeps the model it was running, whatever the new account's
  default; `H` says what else differs there (MCP servers, hooks, plugins, Remote Control).
- **It says when the accounts are set up differently.** Accounts are configured one at a
  time and drift apart. `A` lists, for each account, what a session moved there would gain
  or lose against this deck's own: model, effort, auto-compact window, MCP servers, hooks,
  plugins, env names, Remote Control.
- **It forecasts the week.** The line under the title adds `→ out Fri 9am` to an account
  that will run out before its week renews at the pace it has been spent so far, and `A`
  has the same for every account.
- **It shows who is spending.** The highlighted session's line says what it spent in the
  last five hours, at API rates (the rate usage credits bill at), and its share of its
  account's spend; `A` lists the five biggest spenders. Subagents count toward the session
  that started them.
- **It flags big contexts, and compacts on request.** Every turn re-reads the whole
  context, so a row whose session carries 300k tokens or more shows `◔` and the size
  (orange; red from 600k). `C` sends `/compact` to the highlighted session, with an optional
  focus, once its turn is over — while its prompt cache is still warm, that costs a fraction
  of a turn.
- **Sessions from older decks come along.** Sessions started by the old one-deck-per-account
  layout (`deck --account`, or `rdeck <name>`) still run in those decks' workspaces. The deck
  sees them — the title bar counts them — and `M` brings them in on the same account; a busy
  one follows once its turn ends.
- **Which account is on a ticket, and what it's doing** — every ticket with a session carries
  a `⦿` dot in the owning subscription's color, and the highlighted row spells out the name.
  One subscription, no column.
- **One session per ticket, and per project** — `⏎` (and `t`) on a ticket whose session runs
  somewhere this deck can't drive — another deck's workspace, or a plain terminal — stops
  and says so instead of opening a second one. A second session would fork the ticket: the
  session id comes from the ticket key alone, so both would append to their own copy of one
  transcript and diverge with no way to merge them. `⏎`/`esc` leaves it alone, `b` brings it
  into this deck, `p` opens its PR instead, and `o` overrides.
- **Ad-hoc sessions have names and survive restarts.** `n` asks for a name and opens the
  session under its own id on the best-placed account; `R` renames one. The deck
  remembers them (`~/.ticketdeck/sessions.json`), so after a herdr restart they are listed
  under Other sessions as resumable, by name, on the account they ran on.
- **Claude usage** — the title bar shows your Claude 5-hour and 7-day rate-limit
  utilization (`◷ 5h 52% · 7d 42%`, or one entry per account when there are several),
  color-coded, with a rough reset countdown. Same source
  as Claude Code's status line (the OAuth usage endpoint); it's a metadata read, so it does
  **not** spend model tokens. Shown only for OAuth logins (not API-key setups). Every deck on
  the machine shares one reading per account through `~/.ticketdeck/usage.json`, refreshed at
  most every 5 minutes — polling per deck earned 429s and blanked the line. When a reading is
  missing the row says why: `rate limited` clears itself; `token expired` means nobody has
  used that account lately — it still takes work, and opening a session under it refreshes
  the token; `not signed in` means it can't run anything until someone logs it in again. A
  `~` after the numbers means they're more than two intervals old, and a reading from before
  the weekly window last reset is not shown at all: it is history, not headroom.
- **It checks its own assumptions.** Much of what the deck reads is undocumented — Claude
  Code's session registry and transcripts, the usage endpoint — and Claude Code ships most
  days. At start and every ten minutes the deck checks each account's registry against
  `claude agents --json`, that live sessions' transcripts are where it looks and still
  parse, that the usage endpoint still answers in the shape it knows, and herdr's version. A
  failed check is named under the title, and an account whose registry stopped matching is
  read through `claude agents --json` instead. `ticketdeck doctor` prints every check.
- **herdr's events, not just a timer.** Under herdr the deck subscribes to the server's
  pane and agent-status events and refreshes the moment one arrives; the 3-second poll
  slows to 10 seconds while the subscription is up, and comes back if it drops.
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
