package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/hdtradeservices/ticketdeck/internal/account"
	"github.com/hdtradeservices/ticketdeck/internal/doctor"
	"github.com/hdtradeservices/ticketdeck/internal/herd"
	"github.com/hdtradeservices/ticketdeck/internal/quota"
)

// errChecksFailed makes `ticketdeck doctor` exit non-zero, so a script can
// gate on it, without printing anything past the table.
var errChecksFailed = errors.New("a check failed")

// runDoctor implements `ticketdeck doctor`: every assumption the deck makes
// about Claude Code, herdr and the accounts, checked now. The deck runs the
// same checks on its own and names a failure in its header; this prints all of
// them, warnings included.
func runDoctor(w io.Writer) error {
	now := time.Now()
	accts := account.All()
	qa := make([]quota.Account, 0, len(accts))
	for _, a := range accts {
		qa = append(qa, quota.Account{Name: a.Name, ConfigDir: a.ConfigDir})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	r := doctor.Run(doctor.Input{
		Accounts: accts, Home: account.Current(), Herdr: herd.Available(),
		Quotas: quota.Poll(ctx, qa, now), Now: now,
	})
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range r.Checks {
		mark := "ok"
		switch c.Level {
		case doctor.Warn:
			mark = "warn"
		case doctor.Fail:
			mark = "FAIL"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", mark, c.Name, c.Detail)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(r.Failures()) > 0 {
		return errChecksFailed
	}
	return nil
}

func init() {
	subcommands["doctor"] = func([]string) error {
		err := runDoctor(os.Stdout)
		if errors.Is(err, errChecksFailed) {
			os.Exit(1)
		}
		return err
	}
}
