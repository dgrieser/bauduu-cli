package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dgrieser/bauduu-cli/internal/bauduu"
)

// watchState is the last seen "Bisherige Sets" list, per account.
type watchState struct {
	User    string        `json:"user"`
	Checked time.Time     `json:"checked"`
	Loans   []bauduu.Loan `json:"loans"`
}

// loanChange is one detected difference, phrased for a notification.
type loanChange struct {
	Title string
	Line  string
}

// defaultStatePath keeps one state file per account. The readable part is
// lossy, so a hash of the (case-insensitive) username keeps names such as
// "a+b" and "a@b" apart.
func defaultStatePath(user string) string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	lower := strings.ToLower(user)
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_' {
			return r
		}
		return '_'
	}, lower)
	sum := sha256.Sum256([]byte(lower))
	return filepath.Join(dir, "bauduu", fmt.Sprintf("sets-%s-%x.json", safe, sum[:4]))
}

// loadState returns nil without error when there is no state yet.
func loadState(path string) (*watchState, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s watchState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	return &s, nil
}

// saveState replaces the state file atomically, so an interrupted run never
// leaves a truncated file that would look like "everything is new".
func saveState(path string, s *watchState) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func loanLabel(l bauduu.Loan) string {
	if l.Placeholder {
		return l.Name
	}
	if n := l.SetNumber(); n != "" {
		return fmt.Sprintf("%s (%s)", stripSetNumber(l.Name, n), n)
	}
	return l.Name
}

func loanTitleName(l bauduu.Loan) string {
	if l.Placeholder {
		return l.Name
	}
	return stripSetNumber(l.Name, l.SetNumber())
}

// diffLoans compares two snapshots by row ID. Changes follow the order of
// the new list (newest first); removed rows come last.
func diffLoans(old, cur []bauduu.Loan, now time.Time) []loanChange {
	before := make(map[int]bauduu.Loan, len(old))
	for _, l := range old {
		before[l.ID] = l
	}
	seen := make(map[int]bool, len(cur))
	var changes []loanChange
	for _, l := range cur {
		seen[l.ID] = true
		prev, existed := before[l.ID]
		switch {
		case !existed && l.Placeholder:
			changes = append(changes, loanChange{
				Title: "Versand geplant",
				Line:  l.Name,
			})
		case !existed && l.Active():
			changes = append(changes, loanChange{
				Title: "Neues Set unterwegs: " + loanTitleName(l),
				Line:  fmt.Sprintf("Versendet: %s am %s", loanLabel(l), l.Shipped),
			})
		case !existed:
			changes = append(changes, loanChange{
				Title: "Neuer Eintrag: " + loanTitleName(l),
				Line:  fmt.Sprintf("Neu: %s, versendet %s, retour %s, Status %s", loanLabel(l), l.Shipped, l.Returned, l.Status),
			})
		case prev != l:
			changes = append(changes, describeUpdate(prev, l, now))
		}
	}
	for _, l := range old {
		if !seen[l.ID] {
			changes = append(changes, loanChange{
				Title: "Eintrag entfernt: " + loanTitleName(l),
				Line:  "Entfernt: " + loanLabel(l),
			})
		}
	}
	return changes
}

func describeUpdate(prev, l bauduu.Loan, now time.Time) loanChange {
	var parts []string
	title := "Geändert: " + loanTitleName(l)
	returned := prev.Returned.IsZero() && !l.Returned.IsZero()
	if returned {
		title = "Retour verbucht: " + loanTitleName(l)
		part := fmt.Sprintf("Retour verbucht: %s am %s", loanLabel(l), l.Returned)
		if d := l.Days(now); d >= 0 {
			part += fmt.Sprintf(" nach %d Tagen", d)
		}
		parts = append(parts, part)
	}
	if prev.Status != l.Status && !(returned && l.Status == "Zurückgegeben") {
		parts = append(parts, fmt.Sprintf("Status %s: %s → %s", loanLabel(l), prev.Status, l.Status))
	}
	if prev.BonusBricks != l.BonusBricks {
		diff := l.BonusBricks - prev.BonusBricks
		parts = append(parts, fmt.Sprintf("Bonussteine %s: %+d (jetzt %d)", loanLabel(l), diff, l.BonusBricks))
		if !returned && prev.Status == l.Status {
			title = "Bonussteine: " + loanTitleName(l)
		}
	}
	if prev.Shipped != l.Shipped {
		parts = append(parts, fmt.Sprintf("Versanddatum %s: %s → %s", loanLabel(l), orDash(prev.Shipped.String()), orDash(l.Shipped.String())))
	}
	if !returned && prev.Returned != l.Returned {
		parts = append(parts, fmt.Sprintf("Retourdatum %s: %s → %s", loanLabel(l), orDash(prev.Returned.String()), orDash(l.Returned.String())))
	}
	if len(parts) == 0 {
		parts = append(parts, "Geändert: "+loanLabel(l))
	}
	return loanChange{Title: title, Line: strings.Join(parts, "; ")}
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

// summarize turns changes into one notification title and description.
func summarize(changes []loanChange) (title, description string) {
	lines := make([]string, len(changes))
	for i, c := range changes {
		lines[i] = c.Line
	}
	if len(changes) == 1 {
		title = "Bauduu: " + changes[0].Title
	} else {
		title = fmt.Sprintf("Bauduu: %d Änderungen bei Bisherige Sets", len(changes))
	}
	return title, strings.Join(lines, "\n")
}

type watchOptions struct {
	notify    string
	statePath string
	test      bool
	dryRun    bool
	verbose   bool
}

func newWatchCmd(opts *options) *cobra.Command {
	w := &watchOptions{}
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Check Bisherige Sets for changes and notify (for cron)",
		Long: `watch loads Bisherige Sets, compares it with the last run and reports changes:
newly shipped sets, booked returns, status and bonus brick changes.

The first run only stores the current list. Later runs stay silent when
nothing changed, so cron sends no mail. On changes the --notify command runs;
without --notify, title and description are printed to stdout instead.

The --notify value is a /bin/sh command template. {{title}} and
{{description}} become quoted references to $BAUDUU_TITLE and
$BAUDUU_DESCRIPTION, so the text from the site is never parsed as shell
syntax. The quoting adapts to where the placeholder stands (bare, inside
"double" or 'single' quotes, $(...) or backticks), so each value arrives
as one argument:

  bauduu watch --notify 'notify -t {{title}} -d {{description}}'
  bauduu watch --notify 'curl -s -H "Title: {{title}}" -d {{description}} ntfy.sh/topic'

The state is saved only after the notification succeeded, so a failed
command is retried on the next run.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWatch(cmd.Context(), cmd.OutOrStdout(), opts, w)
		},
	}
	f := cmd.Flags()
	f.StringVar(&w.notify, "notify", "", "shell command template with {{title}} and {{description}}")
	f.StringVar(&w.statePath, "state", "", "state file (default $XDG_STATE_HOME/bauduu/sets-<user>-<hash>.json)")
	f.BoolVar(&w.test, "test", false, "send a test notification through --notify and exit")
	f.BoolVar(&w.dryRun, "dry-run", false, "print the rendered notify command instead of running it; keep the state unchanged")
	f.BoolVarP(&w.verbose, "verbose", "v", false, "also report when nothing changed")
	return cmd
}

func notifyOrPrint(ctx context.Context, out io.Writer, w *watchOptions, title, description string) error {
	if w.notify == "" {
		_, err := fmt.Fprintf(out, "%s\n%s\n", title, description)
		return err
	}
	if w.dryRun {
		cmdline, err := renderCommand(w.notify)
		if err != nil {
			return err
		}
		// Print a standalone equivalent of what runNotify would execute.
		_, err = fmt.Fprintf(out, "BAUDUU_TITLE=%s BAUDUU_DESCRIPTION=%s /bin/sh -c %s\n",
			shellQuote(title), shellQuote(description), shellQuote(cmdline))
		return err
	}
	return runNotify(ctx, w.notify, title, description)
}

func runWatch(ctx context.Context, out io.Writer, opts *options, w *watchOptions) error {
	// Validate the template before touching the network, so a cron typo
	// fails loudly instead of after a successful fetch.
	if w.notify != "" {
		if _, err := renderCommand(w.notify); err != nil {
			return err
		}
	}
	if w.test {
		if w.notify == "" {
			return errors.New("--test needs --notify")
		}
		return notifyOrPrint(ctx, out, w, `Bauduu: Testbenachrichtigung`,
			`Quoting-Test: 'einfach' "doppelt" $HOME `+"`id`"+` \ ; & | * %`+"\nZweite Zeile")
	}
	creds, err := resolveCredentials(opts.user, opts.configPath, true)
	if err != nil {
		return err
	}
	statePath := w.statePath
	if statePath == "" {
		if statePath = defaultStatePath(creds.User); statePath == "" {
			return errors.New("cannot determine state path; use --state")
		}
	}
	prev, err := loadState(statePath)
	if err != nil {
		return err
	}
	// Diffing another account's list would report every set as new.
	if prev != nil && prev.User != "" && !strings.EqualFold(prev.User, creds.User) {
		return fmt.Errorf("state file %s belongs to user %q, not %q; use another --state", statePath, prev.User, creds.User)
	}
	client, err := login(ctx, opts, creds)
	if err != nil {
		return err
	}
	loans, err := client.Loans(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	next := &watchState{User: creds.User, Checked: now.UTC().Truncate(time.Second), Loans: loans}
	if prev == nil {
		if w.dryRun {
			fmt.Fprintf(out, "Kein Ausgangsstand in %s; ein echter Lauf würde %d Einträge speichern.\n", statePath, len(loans))
			return nil
		}
		if err := saveState(statePath, next); err != nil {
			return err
		}
		fmt.Fprintf(out, "Ausgangsstand gespeichert: %d Einträge in %s\n", len(loans), statePath)
		return nil
	}
	changes := diffLoans(prev.Loans, loans, now)
	if len(changes) == 0 {
		if w.verbose {
			fmt.Fprintf(out, "Keine Änderungen (%d Einträge, letzter Lauf %s)\n", len(loans), prev.Checked.Local().Format(time.DateTime))
		}
		if w.dryRun {
			return nil
		}
		return saveState(statePath, next)
	}
	title, description := summarize(changes)
	if err := notifyOrPrint(ctx, out, w, title, description); err != nil {
		return err
	}
	if w.dryRun {
		return nil
	}
	return saveState(statePath, next)
}
