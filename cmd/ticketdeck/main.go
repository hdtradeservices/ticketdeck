package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hdtradeservices/ticketdeck/internal/herd"
	"github.com/hdtradeservices/ticketdeck/internal/linear"
	"github.com/hdtradeservices/ticketdeck/internal/tui"
)

func main() {
	demo := flag.Bool("demo", false, "use canned data instead of the Linear API (no key needed)")
	dump := flag.Bool("dump", false, "print the grouped ticket list and exit (no TUI)")
	preview := flag.Bool("preview", false, "render one styled TUI frame and exit (no event loop)")
	height := flag.Int("height", 0, "viewport height for --preview (0 = show all)")
	root := flag.String("root", "", "default working dir for new sessions (else $TICKETDECK_ROOT, else cwd)")
	dryLaunch := flag.Bool("dry-launch", false, "on enter, print the launch command instead of running it")
	backendName := flag.String("backend", "auto", "launch backend: claude | herdr | auto (herdr if installed, else claude)")
	logPath := flag.String("log", "", "debug log file (default ~/.ticketdeck/ticketdeck.log; 'off' disables)")
	flag.Parse()

	if closeLog := setupLog(*logPath); closeLog != nil {
		defer closeLog()
	}

	fetcher, err := buildFetcher(*demo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ticketdeck:", err)
		os.Exit(1)
	}

	if *dump {
		runDump(fetcher)
		return
	}

	if *preview {
		frame, err := tui.Preview(fetcher, *height)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ticketdeck:", err)
			os.Exit(1)
		}
		fmt.Println(frame)
		return
	}

	backend, err := resolveBackend(*backendName, *dryLaunch, herd.Available())
	if err != nil {
		fmt.Fprintln(os.Stderr, "ticketdeck:", err)
		os.Exit(1)
	}

	p := tea.NewProgram(tui.New(fetcher, resolveRoot(*root), *dryLaunch, backend), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "ticketdeck:", err)
		os.Exit(1)
	}
}

// setupLog points TicketDeck's debug log at a file. Default
// ~/.ticketdeck/ticketdeck.log; "off" disables. Returns a closer (or nil).
func setupLog(path string) func() {
	if path == "off" {
		return nil
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		dir := filepath.Join(home, ".ticketdeck")
		_ = os.MkdirAll(dir, 0o755)
		path = filepath.Join(dir, "ticketdeck.log")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ticketdeck: cannot open log:", err)
		return nil
	}
	tui.SetLog(f)
	return func() { _ = f.Close() }
}

// resolveBackend picks the launch backend: explicit claude/herdr, or auto
// (herdr if available, else the built-in claude path). In dry-launch mode the
// herdr availability check is skipped so its commands can be previewed before
// installing herdr. `available` is injected (herd.Available()) so this stays
// testable regardless of what's installed.
func resolveBackend(name string, dry, available bool) (tui.Backend, error) {
	switch name {
	case "claude":
		return tui.ClaudeBackend{}, nil
	case "herdr":
		if !dry && !available {
			return nil, fmt.Errorf("--backend herdr: herdr not found on PATH (see https://herdr.dev)")
		}
		return tui.HerdBackend{}, nil
	case "auto", "":
		if available {
			return tui.HerdBackend{}, nil
		}
		return tui.ClaudeBackend{}, nil
	default:
		return nil, fmt.Errorf("unknown --backend %q (use claude|herdr|auto)", name)
	}
}

// resolveRoot picks the default working dir for new sessions: --root, then
// $TICKETDECK_ROOT, then the current directory.
func resolveRoot(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if env := os.Getenv("TICKETDECK_ROOT"); env != "" {
		return env
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "."
}

func buildFetcher(demo bool) (tui.Fetcher, error) {
	if demo {
		return demoFetcher{}, nil
	}
	key := os.Getenv("LINEAR_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("LINEAR_API_KEY not set (or pass --demo)")
	}
	return linear.NewClient(key), nil
}

func runDump(f tui.Fetcher) {
	issues, err := f.FetchAssignedOpen(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "ticketdeck:", err)
		os.Exit(1)
	}
	for _, g := range linear.GroupByPriorityThenStatus(linear.FilterVisible(issues)) {
		fmt.Printf("\n▛ %s\n", g.PrioLabel)
		for _, sb := range g.Statuses {
			fmt.Printf("  %s\n", sb.Status)
			for _, is := range sb.Issues {
				fmt.Printf("    %-10s %s\n", is.Identifier, is.Title)
			}
		}
	}
}
