package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dgrieser/bauduu-cli/internal/bauduu"
	"github.com/dgrieser/bauduu-cli/internal/dhl"
	"github.com/dgrieser/bauduu-cli/internal/mail"
)

// trackedShipment links a set at home ("Ausgeliehen") to the parcel it came
// in, found in Bauduu's shipping mail.
type trackedShipment struct {
	LoanID   int       `json:"loan_id"`
	Set      string    `json:"set"`
	Carrier  string    `json:"carrier"`
	Code     string    `json:"code"`
	MailID   string    `json:"mail_id,omitempty"`
	MailDate time.Time `json:"mail_date,omitzero"`
	// Tracking is the last seen DHL state; nil before the first query.
	Tracking *dhl.Shipment `json:"tracking,omitempty"`
	Checked  time.Time     `json:"checked,omitzero"`
}

// trackable reports whether the carrier's tracking can be queried.
func (s *trackedShipment) trackable() bool {
	return strings.EqualFold(s.Carrier, "DHL") || s.Carrier == ""
}

// done means there is nothing left to watch.
func (s *trackedShipment) done() bool {
	return s.Tracking != nil && s.Tracking.Delivered
}

// noticeMail is one mail from the shipping sender; Notice is nil when it
// carries no tracking code.
type noticeMail struct {
	mail.Message
	Notice *bauduu.ShippingNotice `json:"notice,omitempty"`
}

func fetchNotices(ctx context.Context, s mailSettings, since time.Time, skip func(string) bool) ([]noticeMail, error) {
	msgs, err := mail.Fetch(ctx, s.Config, mail.Filter{From: s.From, Since: since, Skip: skip})
	if err != nil {
		return nil, err
	}
	out := make([]noticeMail, len(msgs))
	for i, m := range msgs {
		out[i].Message = m
		if n, ok := bauduu.ParseShippingNotice(m.Subject + "\n" + m.Text); ok {
			out[i].Notice = &n
		}
	}
	return out, nil
}

// pendingLoans are the sets at home without a known tracking code.
func pendingLoans(loans []bauduu.Loan, known []trackedShipment) []bauduu.Loan {
	have := make(map[int]bool, len(known))
	for _, s := range known {
		have[s.LoanID] = true
	}
	var out []bauduu.Loan
	for _, l := range loans {
		if l.Active() && !have[l.ID] {
			out = append(out, l)
		}
	}
	return out
}

// mailSince is the first day a shipping mail for the pending loans can
// have been sent.
func mailSince(pending []bauduu.Loan, now time.Time) time.Time {
	since := time.Time{}
	for _, l := range pending {
		if l.Shipped.IsZero() {
			return now.AddDate(0, 0, -30)
		}
		if d := l.Shipped.AddDate(0, 0, -maxMailOffsetDays); since.IsZero() || d.Before(since) {
			since = d
		}
	}
	return since
}

// maxMailOffsetDays is how far the mail date may lie from the shipping date
// the site shows.
const maxMailOffsetDays = 5

// assignNotices links shipping mails to pending loans: each loan gets the
// mail closest to its shipping date, within maxMailOffsetDays. Codes that
// are already tracked are ignored.
func assignNotices(pending []bauduu.Loan, known []trackedShipment, mails []noticeMail) []trackedShipment {
	codes := make(map[string]bool, len(known))
	for _, s := range known {
		codes[s.Code] = true
	}
	type pair struct {
		loan bauduu.Loan
		mail noticeMail
		dist int
	}
	var pairs []pair
	for _, m := range mails {
		if m.Notice == nil || codes[m.Notice.Code] {
			continue
		}
		for _, l := range pending {
			dist := 0
			if !l.Shipped.IsZero() && !m.Date.IsZero() {
				dist = l.Shipped.DaysUntil(m.Date.In(time.Local))
				if dist < 0 {
					dist = -dist
				}
				if dist > maxMailOffsetDays {
					continue
				}
			}
			pairs = append(pairs, pair{l, m, dist})
		}
	}
	// Closest first; on a tie the newer mail wins (a resent notice).
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].dist != pairs[j].dist {
			return pairs[i].dist < pairs[j].dist
		}
		return pairs[i].mail.Date.After(pairs[j].mail.Date)
	})
	usedLoan := map[int]bool{}
	usedCode := map[string]bool{}
	var out []trackedShipment
	for _, p := range pairs {
		if usedLoan[p.loan.ID] || usedCode[p.mail.Notice.Code] {
			continue
		}
		usedLoan[p.loan.ID] = true
		usedCode[p.mail.Notice.Code] = true
		out = append(out, trackedShipment{
			LoanID:   p.loan.ID,
			Set:      loanLabel(p.loan),
			Carrier:  p.mail.Notice.Carrier,
			Code:     p.mail.Notice.Code,
			MailID:   p.mail.ID,
			MailDate: p.mail.Date,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LoanID > out[j].LoanID })
	return out
}

// trackEnv bundles what shipment tracking needs.
type trackEnv struct {
	mail     *mailSettings // nil: no mailbox configured or disabled
	zip      string
	dhl      *dhl.Client // nil: tracking disabled
	warn     func(error)
	verbose  io.Writer
	mailSeen map[string]bool
}

// shipmentUpdate is the outcome of one tracking round.
type shipmentUpdate struct {
	Shipments []trackedShipment
	MailSeen  []string
	Changes   []loanChange
}

// maxMailSeen bounds the remembered mail IDs.
const maxMailSeen = 200

// updateShipments keeps the shipments of sets still at home, looks for
// tracking codes of new ones in the mailbox and queries DHL for every parcel
// not yet delivered. Failures are passed to env.warn; what worked is kept.
func updateShipments(ctx context.Context, env *trackEnv, loans []bauduu.Loan, prev []trackedShipment, seen []string, now time.Time) shipmentUpdate {
	byID := make(map[int]bauduu.Loan, len(loans))
	for _, l := range loans {
		byID[l.ID] = l
	}
	var u shipmentUpdate
	for _, s := range prev {
		if l, ok := byID[s.LoanID]; ok && l.Active() {
			s.Set = loanLabel(l)
			u.Shipments = append(u.Shipments, s)
		}
	}
	u.MailSeen = seen
	seenSet := make(map[string]bool, len(seen))
	for _, id := range seen {
		seenSet[id] = true
	}

	if pending := pendingLoans(loans, u.Shipments); len(pending) > 0 && env.mail != nil {
		mails, err := fetchNotices(ctx, *env.mail, mailSince(pending, now), func(id string) bool { return seenSet[id] })
		if err != nil {
			env.warn(fmt.Errorf("mail: %w", err))
		} else {
			found := assignNotices(pending, u.Shipments, mails)
			assigned := map[string]bool{}
			for _, s := range found {
				assigned[s.MailID] = true
				u.Changes = append(u.Changes, loanChange{
					Title:    "Trackingnummer für " + shortSet(s.Set),
					Line:     fmt.Sprintf("Trackingnummer für %s: %s %s %s", s.Set, orDash(s.Carrier), s.Code, trackingURL(s)),
					Shipment: true,
				})
			}
			u.Shipments = append(found, u.Shipments...)
			// Mails without a code or already used need no second look;
			// unassigned notices are retried while a set lacks its code.
			for _, m := range mails {
				if m.ID != "" && (m.Notice == nil || assigned[m.ID]) && !seenSet[m.ID] {
					seenSet[m.ID] = true
					u.MailSeen = append(u.MailSeen, m.ID)
				}
			}
			if len(u.MailSeen) > maxMailSeen {
				u.MailSeen = u.MailSeen[len(u.MailSeen)-maxMailSeen:]
			}
			if env.verbose != nil && len(found) == 0 {
				fmt.Fprintf(env.verbose, "Keine neue Trackingnummer (%d Mails von %s geprüft)\n", len(mails), env.mail.From)
			}
		}
	}

	if env.dhl == nil {
		return u
	}
	for i := range u.Shipments {
		s := &u.Shipments[i]
		if !s.trackable() || s.done() {
			continue
		}
		cur, err := env.dhl.Track(ctx, s.Code, env.zip)
		if err != nil {
			env.warn(fmt.Errorf("%s (%s): %w", s.Code, shortSet(s.Set), err))
			if cur == nil {
				continue
			}
		}
		u.Changes = append(u.Changes, trackingChanges(s, s.Tracking, cur)...)
		s.Tracking = cur
		s.Checked = now.UTC().Truncate(time.Second)
	}
	return u
}

// trackingChanges describes what is new in cur compared to old: every new
// history event, or the status when the history is not available.
func trackingChanges(s *trackedShipment, old, cur *dhl.Shipment) []loanChange {
	if old != nil && old.Fingerprint() == cur.Fingerprint() {
		return nil
	}
	title := func(status string) string {
		if cur.Delivered {
			return "Zugestellt: " + shortSet(s.Set)
		}
		return "Sendung " + shortSet(s.Set) + ": " + status
	}
	var lines []string
	oldEvents := 0
	if old != nil {
		oldEvents = len(old.Events)
	}
	switch {
	case oldEvents > 0 && len(cur.Events) > oldEvents:
		for _, e := range cur.Events[oldEvents:] {
			lines = append(lines, eventLine(e))
		}
	case cur.Status != "" && (old == nil || old.Status != cur.Status || cur.Delivered && !old.Delivered):
		// Without a known history (first query, details just unlocked by
		// the postal code) only the current status is news.
		line := cur.Status
		if !cur.StatusTime.IsZero() {
			line += " (" + cur.StatusTime.Local().Format("02.01. 15:04") + ")"
		}
		lines = append(lines, line)
	case cur.NotFound != "" && (old == nil || old.NotFound != cur.NotFound):
		lines = append(lines, cur.NotFound)
	}
	if len(lines) == 0 && old != nil && !old.DeliveryFrom.Equal(cur.DeliveryFrom) && !cur.DeliveryFrom.IsZero() {
		lines = append(lines, "Zustellung voraussichtlich "+deliveryWindow(cur))
	}
	if len(lines) == 0 {
		return nil
	}
	status := cur.Status
	if status == "" {
		status = lines[len(lines)-1]
	}
	desc := fmt.Sprintf("DHL %s (%s): %s", s.Code, shortSet(s.Set), strings.Join(lines, "; "))
	if w := deliveryWindow(cur); w != "" && !cur.Delivered && (old == nil || !old.DeliveryFrom.Equal(cur.DeliveryFrom)) && !strings.Contains(desc, w) {
		desc += "; Zustellung voraussichtlich " + w
	}
	return []loanChange{{Title: title(status), Line: desc, Shipment: true}}
}

func eventLine(e dhl.Event) string {
	var b strings.Builder
	b.WriteString(e.Status)
	var extra []string
	if e.Location != "" {
		extra = append(extra, e.Location)
	}
	if !e.Time.IsZero() {
		extra = append(extra, e.Time.Local().Format("02.01. 15:04"))
	}
	if len(extra) > 0 {
		b.WriteString(" (" + strings.Join(extra, ", ") + ")")
	}
	return b.String()
}

func deliveryWindow(s *dhl.Shipment) string {
	if s.DeliveryFrom.IsZero() {
		return ""
	}
	from := s.DeliveryFrom.Local()
	w := from.Format("02.01. 15:04")
	if !s.DeliveryTo.IsZero() && s.DeliveryTo.After(s.DeliveryFrom) {
		w += "–" + s.DeliveryTo.Local().Format("15:04")
	}
	return w
}

func trackingURL(s trackedShipment) string {
	if s.trackable() {
		return dhl.TrackingURL(s.Code)
	}
	return ""
}

// shortSet drops the set number from a "Name (12345)" label for titles.
func shortSet(label string) string {
	if i := strings.LastIndex(label, " ("); i > 0 && strings.HasSuffix(label, ")") {
		return label[:i]
	}
	return label
}

// newTrackEnv prepares mailbox and DHL access as far as configured.
func newTrackEnv(opts *options, zipFlag string, noMail, noTracking bool, verbose io.Writer) (*trackEnv, error) {
	env := &trackEnv{
		warn:    func(err error) { fmt.Fprintln(os.Stderr, "bauduu: warning:", err) },
		verbose: verbose,
	}
	if !noMail {
		s, ok, err := loadMailSettings(opts.configPath, opts.timeout)
		if err != nil {
			return nil, fmt.Errorf("mail settings: %w", err)
		}
		if ok {
			env.mail = &s
		}
	}
	if !noTracking {
		zip, err := resolveZip(zipFlag, opts.configPath)
		if err != nil {
			return nil, err
		}
		env.zip = zip
		if env.dhl, err = dhl.New(opts.dhlURL, opts.timeout); err != nil {
			return nil, err
		}
	}
	return env, nil
}

func newMailCmd(opts *options) *cobra.Command {
	var days int
	cmd := &cobra.Command{
		Use:     "mail",
		Aliases: []string{"mails", "versandmails"},
		Short:   "List Bauduu's shipping mails and their tracking codes",
		Long: `mail searches the configured mailbox (see 'bauduu setup mail') for mails from
Bauduu's shipping address and prints their tracking codes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkOutput(opts); err != nil {
				return err
			}
			s, ok, err := loadMailSettings(opts.configPath, opts.timeout)
			if err != nil {
				return err
			}
			if !ok {
				return errNoMail
			}
			notices, err := fetchNotices(cmd.Context(), s, time.Now().AddDate(0, 0, -days), nil)
			if err != nil {
				return err
			}
			// Newest first, like the site's lists.
			for i, j := 0, len(notices)-1; i < j; i, j = i+1, j-1 {
				notices[i], notices[j] = notices[j], notices[i]
			}
			out := cmd.OutOrStdout()
			if opts.output == "json" {
				return encodeJSON(out, nonNil(notices))
			}
			color, width := terminalInfo(out, opts.noColor)
			noticesTable(notices, s.From, days).render(out, color, width)
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 90, "search mails of the last n days")
	return cmd
}

func noticesTable(notices []noticeMail, from string, days int) *table {
	t := &table{
		Title: fmt.Sprintf("Versandmails von %s (%d Tage)", from, days),
		Columns: []column{
			{Title: "Datum"},
			{Title: "Versand"},
			{Title: "Trackingnummer"},
			{Title: "Betreff", Flex: true, MinWidth: 10, Drop: 1},
		},
	}
	for _, n := range notices {
		carrier, code := "", ""
		if n.Notice != nil {
			carrier, code = n.Notice.Carrier, n.Notice.Code
		}
		r := t.add(n.Date.Local().Format("2006-01-02 15:04"), carrier, code, n.Subject)
		if n.Notice == nil {
			r.Style = ansiDim
		}
	}
	if len(notices) == 0 {
		t.Footer = append(t.Footer, "Keine Mails gefunden")
	}
	return t
}

type trackFlags struct {
	zip    string
	noMail bool
}

func newTrackCmd(opts *options) *cobra.Command {
	f := &trackFlags{}
	cmd := &cobra.Command{
		Use:     "track [tracking-code...]",
		Aliases: []string{"sendung", "sendungen", "dhl", "tracking"},
		Short:   "Show the DHL tracking of the sets on their way or at home",
		Long: `track shows the DHL shipment status and history.

Without arguments it loads Bisherige Sets, takes every set at home
("Ausgeliehen"), finds its tracking code in the watch state or in Bauduu's
shipping mail (see 'bauduu setup mail') and queries DHL. With arguments it
queries the given tracking codes directly.

DHL shows the full history only with the recipient's postal code: --zip,
BAUDUU_DHL_ZIP or 'bauduu setup dhl'.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTrack(cmd.Context(), cmd.OutOrStdout(), opts, f, args)
		},
	}
	cmd.Flags().StringVar(&f.zip, "zip", "", "postal code of the delivery address (default $BAUDUU_DHL_ZIP or dhl_zip from the config)")
	cmd.Flags().BoolVar(&f.noMail, "no-mail", false, "do not search the mailbox for tracking codes")
	return cmd
}

func runTrack(ctx context.Context, out io.Writer, opts *options, f *trackFlags, codes []string) error {
	if err := checkOutput(opts); err != nil {
		return err
	}
	env, err := newTrackEnv(opts, f.zip, f.noMail, false, nil)
	if err != nil {
		return err
	}
	var shipments []trackedShipment
	if len(codes) > 0 {
		for _, c := range codes {
			shipments = append(shipments, trackedShipment{Code: strings.ToUpper(strings.TrimSpace(c)), Carrier: "DHL"})
		}
		var errs []error
		for i := range shipments {
			s := &shipments[i]
			cur, err := env.dhl.Track(ctx, s.Code, env.zip)
			if err != nil {
				errs = append(errs, err)
			}
			s.Tracking = cur
		}
		if err := renderShipments(out, opts, shipments); err != nil {
			return err
		}
		return errors.Join(errs...)
	}

	creds, err := resolveCredentials(opts.user, opts.configPath, true)
	if err != nil {
		return err
	}
	// Known codes come from the watch state, if there is one.
	var prev []trackedShipment
	var seen []string
	if path := defaultStatePath(creds.User); path != "" {
		if st, err := loadState(path); err == nil && st != nil && strings.EqualFold(st.User, creds.User) {
			prev, seen = st.Shipments, st.MailSeen
		}
	}
	// track always queries, delivered or not.
	for i := range prev {
		prev[i].Tracking = nil
	}
	client, err := login(ctx, opts, creds)
	if err != nil {
		return err
	}
	loans, err := client.Loans(ctx)
	if err != nil {
		return err
	}
	var failed []error
	env.warn = func(err error) { failed = append(failed, err) }
	u := updateShipments(ctx, env, loans, prev, seen, time.Now())
	pending := pendingLoans(loans, u.Shipments)
	if err := renderShipments(out, opts, u.Shipments); err != nil {
		return err
	}
	if opts.output == "table" {
		for _, l := range pending {
			hint := "keine Versandmail gefunden"
			if env.mail == nil {
				hint = "kein Postfach eingerichtet ('bauduu setup mail')"
			}
			fmt.Fprintf(out, "%s: keine Trackingnummer, %s\n", loanLabel(l), hint)
		}
		if len(u.Shipments) == 0 && len(pending) == 0 {
			fmt.Fprintln(out, "Kein Set ausgeliehen.")
		}
	}
	return errors.Join(failed...)
}

func renderShipments(out io.Writer, opts *options, shipments []trackedShipment) error {
	if opts.output == "json" {
		return encodeJSON(out, nonNil(shipments))
	}
	color, width := terminalInfo(out, opts.noColor)
	for i, s := range shipments {
		if i > 0 {
			fmt.Fprintln(out)
		}
		shipmentTable(s).render(out, color, width)
	}
	return nil
}

func shipmentTable(s trackedShipment) *table {
	title := orDash(s.Carrier) + " " + s.Code
	if s.Set != "" {
		title = s.Set + " · " + title
	}
	t := &table{
		Title:     title,
		HideEmpty: true,
		Columns: []column{
			{Title: "Zeit"},
			{Title: "Ort", Flex: true, MinWidth: 8, Drop: 1},
			{Title: "Status", Flex: true, MinWidth: 20},
		},
	}
	tr := s.Tracking
	if tr == nil {
		if !s.trackable() {
			t.Footer = append(t.Footer, "Sendungsverfolgung nur für DHL")
		} else {
			t.Footer = append(t.Footer, "Kein DHL-Status abrufbar")
		}
		return t
	}
	for i := len(tr.Events) - 1; i >= 0; i-- {
		e := tr.Events[i]
		when := ""
		if !e.Time.IsZero() {
			when = e.Time.Local().Format("2006-01-02 15:04")
		}
		r := t.add(when, e.Location, e.Status)
		if i < len(tr.Events)-1 {
			r.Style = ansiDim
		}
	}
	status := orDash(tr.Status)
	if tr.MaxProgress > 0 {
		status = progressBar(tr.Progress, tr.MaxProgress) + " " + status
	}
	if !tr.StatusTime.IsZero() {
		status += " (" + tr.StatusTime.Local().Format("2006-01-02 15:04") + ")"
	}
	t.Footer = append(t.Footer, status)
	if w := deliveryWindow(tr); w != "" && !tr.Delivered {
		t.Footer = append(t.Footer, "Zustellung voraussichtlich "+w)
	}
	if tr.NotFound != "" {
		t.Footer = append(t.Footer, "DHL: "+tr.NotFound)
	}
	if tr.ZipRequired || (!tr.Complete && tr.NotFound == "") {
		t.Footer = append(t.Footer, "Details nur mit Postleitzahl: --zip oder 'bauduu setup dhl'")
	}
	t.Footer = append(t.Footer, tr.URL)
	return t
}

func progressBar(n, total int) string {
	n = min(max(n, 0), total)
	return strings.Repeat("■", n) + strings.Repeat("□", total-n)
}

func checkOutput(opts *options) error {
	if opts.output != "table" && opts.output != "json" {
		return fmt.Errorf("unknown output format %q (want table or json)", opts.output)
	}
	return nil
}

func encodeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
