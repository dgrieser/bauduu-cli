package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/dgrieser/bauduu-cli/internal/bauduu"
	"github.com/dgrieser/bauduu-cli/internal/dhl"
	"github.com/dgrieser/bauduu-cli/internal/mail"
)

// Config keys for the mailbox with Bauduu's shipping mails, the DHL postal
// code and the key for DHL's API.
const (
	keyMailProtocol = "mail_protocol"
	keyMailHost     = "mail_host"
	keyMailPort     = "mail_port"
	keyMailSecurity = "mail_security"
	keyMailUser     = "mail_user"
	keyMailPassword = "mail_password"
	keyMailMailbox  = "mail_mailbox"
	keyMailFrom     = "mail_from"
	keyDHLZip       = "dhl_zip"
	keyDHLAPIKey    = "dhl_api_key"
)

// mailSettings is the mailbox configuration plus the sender to look for.
type mailSettings struct {
	mail.Config
	From string
}

// loadMailSettings reads the mailbox settings from the config file;
// BAUDUU_MAIL_PASSWORD overrides the stored password. ok is false when no
// mailbox is configured at all.
func loadMailSettings(configPath string, timeout time.Duration) (s mailSettings, ok bool, err error) {
	cfg, err := readConfig(configPath)
	if err != nil {
		return s, false, err
	}
	return mailSettingsFrom(cfg, timeout)
}

func mailSettingsFrom(cfg map[string]string, timeout time.Duration) (s mailSettings, ok bool, err error) {
	if cfg[keyMailHost] == "" {
		return s, false, nil
	}
	s.Protocol = cfg[keyMailProtocol]
	s.Host = cfg[keyMailHost]
	s.Security = cfg[keyMailSecurity]
	s.User = cfg[keyMailUser]
	s.Password = cfg[keyMailPassword]
	if pw := os.Getenv("BAUDUU_MAIL_PASSWORD"); pw != "" {
		s.Password = pw
	}
	s.Mailbox = cfg[keyMailMailbox]
	s.Timeout = timeout
	if p := cfg[keyMailPort]; p != "" {
		if s.Port, err = strconv.Atoi(p); err != nil {
			return s, true, fmt.Errorf("config %s: bad port %q", keyMailPort, p)
		}
	}
	s.From = cfg[keyMailFrom]
	if s.From == "" {
		s.From = bauduu.ShippingSender
	}
	s.Config, err = s.Config.Normalize()
	return s, true, err
}

// resolveZip picks the DHL postal code: flag, BAUDUU_DHL_ZIP, config file.
func resolveZip(flagZip, configPath string) (string, error) {
	zip := strings.TrimSpace(flagZip)
	if zip == "" {
		zip = strings.TrimSpace(os.Getenv("BAUDUU_DHL_ZIP"))
	}
	if zip == "" {
		cfg, err := readConfig(configPath)
		if err != nil {
			return "", err
		}
		zip = cfg[keyDHLZip]
	}
	if zip != "" && !validZip(zip) {
		return "", fmt.Errorf("invalid postal code %q (want 5 digits)", zip)
	}
	return zip, nil
}

var zipRe = regexp.MustCompile(`^[0-9]{5}$`)

func validZip(s string) bool { return zipRe.MatchString(s) }

// prompter asks for values on the terminal, offering the current value as
// the default.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
	tty bool
}

func newPrompter() *prompter {
	return &prompter{in: bufio.NewReader(os.Stdin), out: os.Stderr, tty: term.IsTerminal(int(os.Stdin.Fd()))}
}

func (p *prompter) ask(label, def string) (string, error) {
	if !p.tty {
		return def, nil
	}
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", label)
	}
	line, err := p.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	if v := strings.TrimSpace(line); v != "" {
		return v, nil
	}
	return def, nil
}

// secret reads a password without echo; Enter keeps the current one.
func (p *prompter) secret(label string, hasCurrent bool) (string, error) {
	if !p.tty {
		return "", nil
	}
	if hasCurrent {
		label += " (Enter: unverändert)"
	}
	fmt.Fprintf(p.out, "%s: ", label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(p.out)
	return string(b), err
}

func newSetupCmd(opts *options) *cobra.Command {
	mf := &setupMailFlags{}
	df := &setupDHLFlags{}
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Configure the mailbox for shipping mails and DHL tracking",
		Long: `setup stores settings in the config file (mode 0600), next to the bauduu
login. Existing values are offered as defaults; other entries stay as they are.

  bauduu setup         both, the mailbox first, then DHL
  bauduu setup mail    mailbox (IMAP or POP3) with Bauduu's shipping mails
  bauduu setup dhl     key for DHL's tracking API and the postal code (PLZ)

Without a subcommand an empty mail server skips the mailbox, and no API key
and postal code skip DHL. The flags of setup mail and setup dhl (--api-key,
--zip) preset the answers.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if err := runSetupMail(cmd.Context(), out, opts, mf, cmd.Flags().Changed, true); err != nil {
				return err
			}
			return runSetupDHL(cmd.Context(), out, opts, df, cmd.Flags().Changed)
		},
	}
	mf.register(cmd)
	df.register(cmd)
	cmd.AddCommand(newSetupMailCmd(opts), newSetupDHLCmd(opts))
	return cmd
}

// skipHint marks a prompt whose empty answer skips that part of setup.
func skipHint(label, def string, optional bool) string {
	if optional && def == "" {
		return label + " (leer: überspringen)"
	}
	return label
}

type setupMailFlags struct {
	protocol, host, security, user, mailbox, from string
	port                                          int
	noTest                                        bool
	days                                          int
}

func newSetupMailCmd(opts *options) *cobra.Command {
	f := &setupMailFlags{}
	cmd := &cobra.Command{
		Use:   "mail",
		Short: "Configure IMAP/POP3 access to the mailbox with Bauduu's shipping mails",
		Long: `setup mail asks for the mail server and login, tests the connection, looks
for shipping mails and stores the settings in the config file.

Flags preset the answers; without a terminal they are used as given. The
password comes from the prompt or BAUDUU_MAIL_PASSWORD. Mails are only read:
IMAP messages stay unread, POP3 messages are never deleted.

  bauduu setup mail --protocol imap --host imap.example.com --mail-user me@example.com`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSetupMail(cmd.Context(), cmd.OutOrStdout(), opts, f, cmd.Flags().Changed, false)
		},
	}
	f.register(cmd)
	return cmd
}

func (f *setupMailFlags) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.protocol, "protocol", "", "imap or pop3 (default imap)")
	fl.StringVar(&f.host, "host", "", "mail server host name")
	fl.IntVar(&f.port, "port", 0, "port (default 993 imap / 995 pop3 with TLS, 143 / 110 otherwise)")
	fl.StringVar(&f.security, "security", "", "tls, starttls or none (default tls)")
	fl.StringVar(&f.user, "mail-user", "", "mail login")
	fl.StringVar(&f.mailbox, "mailbox", "", "IMAP folder (default INBOX)")
	fl.StringVar(&f.from, "from", "", "sender of the shipping mails (default "+bauduu.ShippingSender+")")
	fl.BoolVar(&f.noTest, "no-test", false, "save without testing the connection")
	fl.IntVar(&f.days, "days", 60, "days of mail to search in the test")
}

// runSetupMail asks for the mailbox settings, tests and saves them. With
// optional set, an empty server skips the mailbox.
func runSetupMail(ctx context.Context, out io.Writer, opts *options, f *setupMailFlags, changed func(string) bool, optional bool) error {
	cur, err := readConfig(opts.configPath)
	if err != nil {
		return err
	}
	p := newPrompter()
	pick := func(flag, val, key, def string) string {
		if changed(flag) {
			return val
		}
		if cur[key] != "" {
			return cur[key]
		}
		return def
	}
	var s mailSettings
	if s.Protocol, err = p.ask("Protokoll (imap/pop3)", pick("protocol", f.protocol, keyMailProtocol, mail.IMAP)); err != nil {
		return err
	}
	host := pick("host", f.host, keyMailHost, "")
	if s.Host, err = p.ask(skipHint("Server", host, optional), host); err != nil {
		return err
	}
	if s.Host = strings.TrimSpace(s.Host); s.Host == "" && optional {
		fmt.Fprintln(out, "Kein Mailserver angegeben, Postfach übersprungen")
		return nil
	}
	if s.Security, err = p.ask("Verschlüsselung (tls/starttls/none)", pick("security", f.security, keyMailSecurity, mail.SecurityTLS)); err != nil {
		return err
	}
	// Normalize early, for the port default and to catch typos.
	probe, err := mail.Config{Protocol: s.Protocol, Host: s.Host, Security: s.Security, User: "-", Password: "-"}.Normalize()
	if err != nil {
		return err
	}
	s.Protocol, s.Security = probe.Protocol, probe.Security
	portDef := strconv.Itoa(mail.DefaultPort(s.Protocol, s.Security))
	if changed("port") {
		portDef = strconv.Itoa(f.port)
	} else if cur[keyMailPort] != "" {
		portDef = cur[keyMailPort]
	}
	port, err := p.ask("Port", portDef)
	if err != nil {
		return err
	}
	if s.Port, err = strconv.Atoi(port); err != nil || s.Port <= 0 || s.Port > 65535 {
		return fmt.Errorf("invalid port %q", port)
	}
	if s.User, err = p.ask("Benutzer", pick("mail-user", f.user, keyMailUser, "")); err != nil {
		return err
	}
	s.Password = os.Getenv("BAUDUU_MAIL_PASSWORD")
	if s.Password == "" {
		if s.Password, err = p.secret("Passwort für "+s.User, cur[keyMailPassword] != ""); err != nil {
			return err
		}
	}
	if s.Password == "" {
		s.Password = cur[keyMailPassword]
	}
	if s.Protocol == mail.IMAP {
		if s.Mailbox, err = p.ask("Ordner", pick("mailbox", f.mailbox, keyMailMailbox, "INBOX")); err != nil {
			return err
		}
	}
	if s.From, err = p.ask("Absender der Versandmails", pick("from", f.from, keyMailFrom, bauduu.ShippingSender)); err != nil {
		return err
	}
	s.Timeout = opts.timeout
	if s.Config, err = s.Config.Normalize(); err != nil {
		return err
	}
	if !f.noTest {
		n, err := mail.Check(ctx, s.Config)
		if err != nil {
			return fmt.Errorf("%w (nothing saved; --no-test saves anyway)", err)
		}
		fmt.Fprintf(out, "Login OK: %d Nachrichten in %s\n", n, mailboxName(s))
		notices, err := fetchNotices(ctx, s, time.Now().AddDate(0, 0, -f.days), nil)
		if err != nil {
			return fmt.Errorf("%w (nothing saved; --no-test saves anyway)", err)
		}
		withCode := 0
		for _, n := range notices {
			if n.Notice != nil {
				withCode++
			}
		}
		fmt.Fprintf(out, "%d Mails von %s in den letzten %d Tagen, %d mit Trackingnummer\n", len(notices), s.From, f.days, withCode)
	}
	mailbox := s.Mailbox
	if s.Protocol == mail.POP3 {
		mailbox = ""
	}
	err = updateConfig(opts.configPath, "# bauduu-cli settings", []kv{
		{keyMailProtocol, s.Protocol},
		{keyMailHost, s.Host},
		{keyMailPort, strconv.Itoa(s.Port)},
		{keyMailSecurity, s.Security},
		{keyMailUser, s.User},
		{keyMailPassword, s.Password},
		{keyMailMailbox, mailbox},
		{keyMailFrom, s.From},
	})
	if err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintf(out, "Mail-Einstellungen gespeichert in %s (0600)\n", opts.configPath)
	return nil
}

func mailboxName(s mailSettings) string {
	if s.Protocol == mail.POP3 {
		return "POP3-Postfach"
	}
	return s.Mailbox
}

type setupDHLFlags struct {
	zip, apiKey, testCode string
	remove                bool
}

func newSetupDHLCmd(opts *options) *cobra.Command {
	f := &setupDHLFlags{}
	cmd := &cobra.Command{
		Use:   "dhl",
		Short: "Store the key for DHL's tracking API and the delivery postal code",
		Long: `Without further setup, track and watch read DHL's public tracking page,
which gives the status and history but no places, sender or address: dhl.de
only hands those out to browsers.

DHL's official API gives them. Register for free at developer.dhl.com, add
an app with the API "Shipment Tracking - Unified" and enter its key here.
With the recipient's postal code the API also names the places. setup dhl
checks the key and stores both as dhl_api_key and dhl_zip in the config
file; BAUDUU_DHL_API_KEY, BAUDUU_DHL_ZIP and --zip on track and watch
override them. When the API fails, tracking falls back to dhl.de.

The free API plan allows 250 queries a day, one every five seconds.

  bauduu setup dhl
  bauduu setup dhl --api-key KEY --zip 44579 --test 00340434698381831612
  bauduu setup dhl --api-key ""      # remove the key`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSetupDHL(cmd.Context(), cmd.OutOrStdout(), opts, f, cmd.Flags().Changed)
		},
	}
	f.register(cmd)
	cmd.Flags().StringVar(&f.testCode, "test", "", "query this tracking code before saving")
	cmd.Flags().BoolVar(&f.remove, "remove", false, "remove the stored API key and postal code")
	return cmd
}

func (f *setupDHLFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.zip, "zip", "", "postal code of the delivery address (5 digits)")
	cmd.Flags().StringVar(&f.apiKey, "api-key", "", `key for DHL's API "Shipment Tracking - Unified" (empty: remove)`)
}

// runSetupDHL asks for the API key and the postal code, checks the key and
// saves both. Neither given nor stored skips it.
func runSetupDHL(ctx context.Context, out io.Writer, opts *options, f *setupDHLFlags, changed func(string) bool) error {
	if f.remove {
		if err := updateConfig(opts.configPath, "", []kv{{keyDHLAPIKey, ""}, {keyDHLZip, ""}}); err != nil {
			return err
		}
		fmt.Fprintln(out, "DHL-API-Schlüssel und Postleitzahl entfernt")
		return nil
	}
	cur, err := readConfig(opts.configPath)
	if err != nil {
		return err
	}
	p := newPrompter()
	key := cur[keyDHLAPIKey]
	if changed("api-key") {
		key = f.apiKey
	} else {
		if p.tty {
			fmt.Fprintln(p.out, "DHL-API-Schlüssel: developer.dhl.com, App mit \"Shipment Tracking - Unified\"")
		}
		v, err := p.secret(skipHint("DHL-API-Schlüssel", key, true), key != "")
		if err != nil {
			return err
		}
		if v != "" {
			key = v
		}
	}
	key = strings.TrimSpace(key)
	zip := cur[keyDHLZip]
	if changed("zip") {
		zip = f.zip
	} else if zip, err = p.ask(skipHint("Postleitzahl der Lieferadresse", zip, true), zip); err != nil {
		return err
	}
	zip = strings.TrimSpace(zip)
	if zip != "" && !validZip(zip) {
		return fmt.Errorf("invalid postal code %q (want 5 digits)", zip)
	}
	if key == "" && zip == "" && cur[keyDHLAPIKey] == "" && cur[keyDHLZip] == "" {
		fmt.Fprintln(out, "Kein DHL-API-Schlüssel und keine Postleitzahl angegeben, DHL übersprungen")
		return nil
	}
	c, err := dhl.New(opts.dhlURL, opts.timeout)
	if err != nil {
		return err
	}
	c.APIURL, c.APIKey = opts.dhlAPIURL, key
	if key != "" && key != cur[keyDHLAPIKey] {
		switch err := c.CheckAPIKey(ctx); {
		case errors.Is(err, dhl.ErrAPIKey):
			return fmt.Errorf("%w (nothing saved)", err)
		case err != nil:
			fmt.Fprintf(out, "Schlüssel nicht geprüft: %v\n", err)
		default:
			fmt.Fprintln(out, "DHL-API-Schlüssel gültig")
		}
	}
	if f.testCode != "" {
		s, err := c.Track(ctx, f.testCode, zip)
		if err != nil {
			return fmt.Errorf("%w (nothing saved)", err)
		}
		fmt.Fprintf(out, "DHL %s (%s): %s\n", s.Code, sourceName(s), orDash(s.Status))
	}
	if err := updateConfig(opts.configPath, "# bauduu-cli settings", []kv{{keyDHLAPIKey, key}, {keyDHLZip, zip}}); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	keyState := "kein API-Schlüssel (nur dhl.de)"
	if key != "" {
		keyState = "API-Schlüssel gesetzt"
	}
	fmt.Fprintf(out, "DHL-Einstellungen gespeichert in %s: %s, Postleitzahl %s\n", opts.configPath, keyState, orDash(zip))
	return nil
}

// resolveAPIKey returns the key for DHL's API: BAUDUU_DHL_API_KEY or the
// config file.
func resolveAPIKey(configPath string) (string, error) {
	if key := strings.TrimSpace(os.Getenv("BAUDUU_DHL_API_KEY")); key != "" {
		return key, nil
	}
	cfg, err := readConfig(configPath)
	if err != nil {
		return "", err
	}
	return cfg[keyDHLAPIKey], nil
}

func sourceName(s *dhl.Shipment) string {
	if s.Source == dhl.SourceAPI {
		return "DHL-API"
	}
	return "dhl.de"
}

var errNoMail = errors.New("no mailbox configured (run 'bauduu setup mail')")
