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

// Config keys for the mailbox with Bauduu's shipping mails and the DHL
// postal code.
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
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Configure the mailbox for shipping mails and the DHL postal code",
		Long: `setup stores settings in the config file (mode 0600), next to the bauduu
login. Existing values are offered as defaults; other entries stay as they are.

  bauduu setup mail    mailbox (IMAP or POP3) with Bauduu's shipping mails
  bauduu setup dhl     postal code (PLZ) for full DHL tracking details`,
	}
	cmd.AddCommand(newSetupMailCmd(opts), newSetupDHLCmd(opts))
	return cmd
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
			return runSetupMail(cmd.Context(), cmd.OutOrStdout(), opts, f, cmd.Flags().Changed)
		},
	}
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
	return cmd
}

func runSetupMail(ctx context.Context, out io.Writer, opts *options, f *setupMailFlags, changed func(string) bool) error {
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
	if s.Host, err = p.ask("Server", pick("host", f.host, keyMailHost, "")); err != nil {
		return err
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

func newSetupDHLCmd(opts *options) *cobra.Command {
	var zip, testCode string
	var remove bool
	cmd := &cobra.Command{
		Use:   "dhl",
		Short: "Store the postal code (PLZ) that unlocks full DHL tracking details",
		Long: `DHL shows the full shipment history, the delivery time window and the
delivery location only after entering the recipient's postal code. setup dhl
stores it as dhl_zip in the config file; BAUDUU_DHL_ZIP or --zip on track and
watch override it.

  bauduu setup dhl --zip 44579
  bauduu setup dhl --zip 44579 --test 00340434698381831612`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if remove {
				if err := updateConfig(opts.configPath, "", []kv{{keyDHLZip, ""}}); err != nil {
					return err
				}
				fmt.Fprintln(out, "Postleitzahl entfernt")
				return nil
			}
			cur, err := readConfig(opts.configPath)
			if err != nil {
				return err
			}
			if zip == "" {
				if zip, err = newPrompter().ask("Postleitzahl der Lieferadresse", cur[keyDHLZip]); err != nil {
					return err
				}
			}
			zip = strings.TrimSpace(zip)
			if !validZip(zip) {
				return fmt.Errorf("invalid postal code %q (want 5 digits)", zip)
			}
			if testCode != "" {
				c, err := dhl.New(opts.dhlURL, opts.timeout)
				if err != nil {
					return err
				}
				s, err := c.Track(cmd.Context(), testCode, zip)
				if err != nil {
					return fmt.Errorf("%w (nothing saved)", err)
				}
				fmt.Fprintf(out, "DHL %s: %s\n", s.Code, orDash(s.Status))
				if !s.Complete {
					fmt.Fprintln(out, "Hinweis: DHL liefert noch keine Details zu dieser Sendung.")
				}
			}
			if err := updateConfig(opts.configPath, "# bauduu-cli settings", []kv{{keyDHLZip, zip}}); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			fmt.Fprintf(out, "Postleitzahl %s gespeichert in %s\n", zip, opts.configPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&zip, "zip", "", "postal code of the delivery address (5 digits)")
	cmd.Flags().StringVar(&testCode, "test", "", "query this tracking code with the postal code before saving")
	cmd.Flags().BoolVar(&remove, "remove", false, "remove the stored postal code")
	return cmd
}

var errNoMail = errors.New("no mailbox configured (run 'bauduu setup mail')")
