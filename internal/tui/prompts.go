package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/fleet"
	"github.com/hdtradeservices/ticketdeck/internal/herd"
	"github.com/hdtradeservices/ticketdeck/internal/ledger"
	"github.com/hdtradeservices/ticketdeck/internal/route"
	"github.com/hdtradeservices/ticketdeck/internal/session"
)

// The two one-line prompts: naming a new ad-hoc session (n), and renaming one
// (R). Both edit a single line of text in the footer, so the list stays in view.

type promptKind int

const (
	promptNew promptKind = iota
	promptRename
)

type prompt struct {
	kind promptKind
	text string
	// acct is the account a new session opens on: 0 is "wherever the router
	// says", then one entry per account in the router's order.
	acct int
	// target is the session being renamed.
	target fleet.Session
}

// newChoices are the accounts tab steps through after "auto", best first. The
// account auto would pick is left out, so every press of tab changes something.
func (m Model) newChoices() []account.Account {
	auto, _ := m.launchAccount("")
	var out []account.Account
	for _, a := range m.handoffTargets("") {
		if a.Name != auto.Name {
			out = append(out, a)
		}
	}
	return out
}

func (m Model) openNewPrompt() (tea.Model, tea.Cmd) {
	m.prompt = &prompt{kind: promptNew}
	m.notice = ""
	return m, nil
}

func (m Model) openRenamePrompt(s fleet.Session) (tea.Model, tea.Cmd) {
	m.prompt = &prompt{kind: promptRename, text: firstNonEmpty(s.Name, s.Key), target: s}
	m.notice = ""
	return m, nil
}

func (m Model) updatePrompt(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := *m.prompt
	switch msg.String() {
	case "esc":
		m.prompt = nil
		m.notice = "canceled"
		return m, nil
	case "enter":
		m.prompt = nil
		if p.kind == promptNew {
			return m.startNew(strings.TrimSpace(p.text), p.acct)
		}
		return m.rename(p.target, strings.TrimSpace(p.text))
	case "backspace":
		if r := []rune(p.text); len(r) > 0 {
			p.text = string(r[:len(r)-1])
		}
	case "ctrl+u":
		p.text = ""
	case "tab", "shift+tab":
		if p.kind == promptNew && len(m.newChoices()) > 0 {
			n := len(m.newChoices()) + 1
			step := 1
			if msg.String() == "shift+tab" {
				step = n - 1
			}
			p.acct = (p.acct + step) % n
		}
	default:
		if len(msg.Runes) > 0 && cols(p.text) < 60 {
			p.text += string(msg.Runes)
		}
	}
	m.prompt = &p
	return m, nil
}

// promptLine renders the open prompt in the footer's status slot.
func (m Model) promptLine() string {
	p := m.prompt
	if p.kind == promptRename {
		return noticeStyle.Render(fmt.Sprintf("  rename %s: %s▏   ⏎ save · esc cancel", truncCols(m.sessionLabel(p.target), 30), p.text))
	}
	name := p.text
	if name == "" {
		name = dimStyle.Render("(optional)")
	}
	on := ""
	if m.multiAccount() {
		choices := m.newChoices()
		if p.acct == 0 || p.acct > len(choices) {
			a, why := m.launchAccount("")
			on = "  on " + m.acctStyle(a.Name).Render("⦿ "+a.Name) + dimStyle.Render(" ("+strings.TrimPrefix(why, "⦿"+a.Name+" ")+")")
		} else {
			a := choices[p.acct-1]
			note := "your pick"
			for _, r := range m.acctRows() {
				if r.Name == a.Name && r.Verdict == route.Out {
					note += " · " + r.Note
				}
			}
			on = "  on " + m.acctStyle(a.Name).Render("⦿ "+a.Name) + dimStyle.Render(" ("+note+")")
		}
		if len(choices) > 0 {
			on += dimStyle.Render(" · tab: another account")
		}
	}
	return noticeStyle.Render("  new session name: ") + name + noticeStyle.Render("▏") + on + dimStyle.Render("   ⏎ start · esc cancel")
}

// startNew opens an ad-hoc session under its own id, so the deck can find it,
// its account and its name again after a restart.
func (m Model) startNew(name string, choice int) (tea.Model, tea.Cmd) {
	acct, why := m.launchAccount("")
	if choices := m.newChoices(); choice > 0 && choice <= len(choices) {
		acct, why = choices[choice-1], "chosen"
	}
	t := session.Ticket{Key: name, Scratch: true, SessionID: session.NewID(), Account: acct.Name, ConfigDir: acct.ConfigDir}
	spec := m.backend.ScratchSpec(t, m.root)
	if m.dry {
		return m.runSpec(spec, spec.Name)
	}
	label := firstNonEmpty(name, spec.Name)
	cwd := spec.Cwd
	_ = ledger.Update(t.SessionID, func(e *ledger.Entry) {
		e.Key, e.Kind, e.Name = spec.Name, ledger.Scratch, name
		e.Account, e.ConfigDir, e.Cwd = acct.Name, acct.ConfigDir, cwd
		e.Started = time.Now()
	})
	m.ledger = ledger.Load()
	next, cmd := m.runSpec(spec, label)
	nm := next.(Model)
	if m.multiAccount() && !spec.Foreground {
		nm.notice = fmt.Sprintf("opening %s on ⦿%s (%s)…", label, acct.Name, why)
	}
	return nm, cmd
}

// rename names a session. The ledger keeps the name for the deck; a live
// session also gets a matching agent name and tab label, and Claude's own
// title, so `/resume` lists it by the same name.
func (m Model) rename(s fleet.Session, name string) (tea.Model, tea.Cmd) {
	if name == "" || name == s.Name {
		m.notice = "name unchanged"
		return m, nil
	}
	newKey := s.Key
	if s.Kind == ledger.Scratch {
		var others []session.SessionRef
		for _, r := range m.homeRefs() {
			if r.Name != s.Key {
				others = append(others, r)
			}
		}
		if n := herd.UniqueName(herd.AgentName(name), others); n != "" {
			newKey = n
		}
	}
	if s.ID != "" {
		_ = ledger.Update(s.ID, func(e *ledger.Entry) {
			e.Key, e.Name = newKey, name
			e.Kind = firstNonEmpty(e.Kind, s.Kind)
			e.Account, e.ConfigDir = firstNonEmpty(e.Account, s.Account), firstNonEmpty(e.ConfigDir, s.ConfigDir)
			e.Cwd = firstNonEmpty(e.Cwd, s.Cwd)
		})
		m.ledger = ledger.Load()
	}
	m.notice = fmt.Sprintf("renamed → %s", name)
	if !s.Live || !s.Home || !m.backend.Herdr() {
		return m, m.refreshStatuses()
	}
	backend := m.backend
	ref := session.SessionRef{Name: s.Key}
	waiting := s.Status.Waiting()
	return m, func() tea.Msg {
		if err := backend.Rename(ref, newKey, name); err != nil {
			return detachedDoneMsg{action: "rename " + s.Key, err: err}
		}
		// Claude's /rename is local — no model turn — but it is typed into the
		// session's prompt, so only while nothing is being typed or run there.
		if waiting {
			_, _ = backend.Send(newKey, "/rename "+name)
		}
		return detachedDoneMsg{action: "renamed " + name}
	}
}
