// Command bauduu shows the bauduu.de LEGO rental customer area as tables:
// Ausleihliste, bisherige Sets, Retouren, Buchungen and Deine Daten.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/dgrieser/bauduu-cli/internal/bauduu"
)

// Set via -ldflags by goreleaser and the Makefile.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

type options struct {
	user       string
	configPath string
	baseURL    string
	output     string
	timeout    time.Duration
	noDetails  bool
	reveal     bool
	noColor    bool
}

type section int

const (
	secWishlist section = iota
	secLoans
	secReturns
	secSubscriptions
	secProfile
)

var allSections = []section{secWishlist, secLoans, secReturns, secSubscriptions, secProfile}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "bauduu:", err)
		os.Exit(1)
	}
}

func versionString() string {
	v := version
	if commit != "" {
		v += " (" + commit
		if date != "" {
			v += ", " + date
		}
		v += ")"
	}
	return v
}

func newRootCmd() *cobra.Command {
	opts := &options{}
	root := &cobra.Command{
		Use:   "bauduu",
		Short: "Show your bauduu.de LEGO rental account as tables",
		Long: `bauduu logs into the bauduu.de customer area and prints its sections as tables:
Ausleihliste, bisherige Sets, Retouren, Buchungen and Deine Daten.

Without a subcommand every section is printed.

Credentials come from --user, BAUDUU_USER/BAUDUU_PASSWORD, the config file
(see 'bauduu login --save'), or an interactive prompt.`,
		Version:       versionString(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, opts, allSections)
		},
	}
	f := root.PersistentFlags()
	f.StringVarP(&opts.user, "user", "u", "", "bauduu.de username (default $BAUDUU_USER or config file)")
	f.StringVar(&opts.configPath, "config", defaultConfigPath(), "config file with user/password")
	f.StringVar(&opts.baseURL, "base-url", bauduu.DefaultBaseURL, "site base URL")
	f.StringVarP(&opts.output, "output", "o", "table", "output format: table or json")
	f.DurationVar(&opts.timeout, "timeout", 30*time.Second, "HTTP timeout per request")
	f.BoolVar(&opts.noDetails, "no-details", false, "skip product pages (article number, theme, package) for the Ausleihliste")
	f.BoolVar(&opts.reveal, "reveal", false, "show sensitive values such as the IBAN unmasked")
	f.BoolVar(&opts.noColor, "no-color", false, "disable colors (also via NO_COLOR)")
	_ = f.MarkHidden("base-url")

	sectionCmd := func(use string, aliases []string, short string, secs ...section) *cobra.Command {
		return &cobra.Command{
			Use:     use,
			Aliases: aliases,
			Short:   short,
			Args:    cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return run(cmd, opts, secs)
			},
		}
	}
	root.AddCommand(
		sectionCmd("all", []string{"alles"}, "Show every section (default)", allSections...),
		sectionCmd("ausleihliste", []string{"wishlist", "liste", "queue"}, "Show the Ausleihliste (queue of sets to ship next)", secWishlist),
		sectionCmd("sets", []string{"bisherige", "meine-sets", "history"}, "Show bisherige Sets (full loan history)", secLoans),
		sectionCmd("retouren", []string{"retoure", "returns"}, "Show returned sets and the set currently at home", secReturns),
		sectionCmd("buchungen", []string{"buchung", "abo", "subscriptions"}, "Show bookings/subscriptions", secSubscriptions),
		sectionCmd("daten", []string{"deine-daten", "profile", "me"}, "Show Deine Daten (account and customer data)", secProfile),
		newLoginCmd(opts),
	)
	return root
}

func newLoginCmd(opts *options) *cobra.Command {
	var save bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Check the credentials and optionally save them to the config file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			creds, err := resolveCredentials(opts.user, opts.configPath, true)
			if err != nil {
				return err
			}
			if _, err := login(cmd.Context(), opts, creds); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Login OK als %s\n", creds.User)
			if save {
				if err := writeConfig(opts.configPath, creds); err != nil {
					return fmt.Errorf("save config: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Zugangsdaten gespeichert in %s (0600)\n", opts.configPath)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&save, "save", false, "write user and password to the config file")
	return cmd
}

func login(ctx context.Context, opts *options, creds credentials) (*bauduu.Client, error) {
	client, err := bauduu.New(opts.baseURL, opts.timeout, "bauduu-cli/"+version+" (+https://github.com/dgrieser/bauduu-cli)")
	if err != nil {
		return nil, err
	}
	client.Warn = func(err error) { fmt.Fprintln(os.Stderr, "bauduu: warning:", err) }
	if err := client.Login(ctx, creds.User, creds.Password); err != nil {
		return nil, err
	}
	return client, nil
}

func needs(secs []section, want ...section) bool {
	for _, s := range secs {
		for _, w := range want {
			if s == w {
				return true
			}
		}
	}
	return false
}

func run(cmd *cobra.Command, opts *options, secs []section) error {
	if opts.output != "table" && opts.output != "json" {
		return fmt.Errorf("unknown output format %q (want table or json)", opts.output)
	}
	creds, err := resolveCredentials(opts.user, opts.configPath, true)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	client, err := login(ctx, opts, creds)
	if err != nil {
		return err
	}
	acct, err := fetch(ctx, client, opts, secs)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if opts.output == "json" {
		return writeJSON(out, acct, secs, opts.reveal)
	}
	color, width := terminalInfo(out, opts.noColor)
	now := time.Now()
	for i, s := range secs {
		if i > 0 {
			fmt.Fprintln(out)
		}
		var t *table
		switch s {
		case secWishlist:
			t = wishlistTable(acct.Wishlist)
		case secLoans:
			t = loansTable(acct.Loans, now)
		case secReturns:
			t = returnsTable(acct.Loans, acct.customerNumber, now)
		case secSubscriptions:
			t = subscriptionsTable(acct.Subscriptions)
		case secProfile:
			t = profileTable(acct.Profile, opts.reveal)
		}
		t.render(out, color, width)
	}
	return nil
}

// account is what one run fetched; customerNumber is filled for Retouren
// even when the full profile is not loaded.
type account struct {
	bauduu.Account
	customerNumber string
}

// fetch loads only the pages the requested sections need, concurrently.
func fetch(ctx context.Context, c *bauduu.Client, opts *options, secs []section) (*account, error) {
	var (
		a    account
		errs = make(chan error, 4)
		n    int
	)
	start := func(f func() error) {
		n++
		go func() { errs <- f() }()
	}
	if needs(secs, secWishlist) {
		start(func() (err error) { a.Wishlist, err = c.Wishlist(ctx, !opts.noDetails); return })
	}
	if needs(secs, secLoans, secReturns) {
		start(func() (err error) { a.Loans, err = c.Loans(ctx); return })
	}
	if needs(secs, secSubscriptions) {
		start(func() (err error) { a.Subscriptions, err = c.Subscriptions(ctx); return })
	}
	switch {
	case needs(secs, secProfile):
		start(func() (err error) {
			if a.Profile, err = c.Profile(ctx); err == nil {
				a.customerNumber = a.Profile.CustomerNumber
			}
			return err
		})
	case needs(secs, secReturns):
		// Retouren only adds the number as a hint, so its page may fail.
		start(func() error {
			var err error
			if a.customerNumber, err = c.CustomerNumber(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "bauduu: warning: customer number:", err)
			}
			return nil
		})
	}
	var all []error
	for range n {
		all = append(all, <-errs)
	}
	return &a, errors.Join(all...)
}

func writeJSON(out io.Writer, a *account, secs []section, reveal bool) error {
	profile := a.Profile
	if profile != nil && !reveal {
		profile = maskedProfile(profile)
	}
	returns := func() any {
		return map[string]any{
			"customer_number": a.customerNumber,
			"returned":        nonNil(returnedLoans(a.Loans)),
			"active":          nonNil(activeLoans(a.Loans)),
		}
	}
	var v any
	if len(secs) == 1 {
		switch secs[0] {
		case secWishlist:
			v = nonNil(a.Wishlist)
		case secLoans:
			v = nonNil(a.Loans)
		case secReturns:
			v = returns()
		case secSubscriptions:
			v = nonNil(a.Subscriptions)
		case secProfile:
			v = profile
		}
	} else {
		v = map[string]any{
			"wishlist":      nonNil(a.Wishlist),
			"loans":         nonNil(a.Loans),
			"returns":       returns(),
			"subscriptions": nonNil(a.Subscriptions),
			"profile":       profile,
		}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// nonNil makes empty lists encode as [] instead of null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// terminalInfo decides on colors and the width tables may use.
func terminalInfo(out io.Writer, noColor bool) (bool, int) {
	f, ok := out.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return false, 0
	}
	color := !noColor && os.Getenv("NO_COLOR") == "" && !strings.EqualFold(os.Getenv("TERM"), "dumb")
	width, _, err := term.GetSize(int(f.Fd()))
	if err != nil {
		width = 0
	}
	return color, width
}
