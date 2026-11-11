package main

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dgrieser/bauduu-cli/internal/bauduu"
	"github.com/dgrieser/bauduu-cli/internal/dhl"
	"github.com/dgrieser/bauduu-cli/internal/mail"
)

// returnMail is one drop-off receipt from DHL ("Einlieferungsbeleg") with
// the tracking codes it names.
type returnMail struct {
	mail.Message
	Codes []string `json:"codes,omitempty"`
}

func fetchReturnMails(ctx context.Context, s mailSettings, since time.Time, skip func(string) bool) ([]returnMail, error) {
	if len(s.Return.From) == 0 {
		return nil, nil
	}
	msgs, err := mail.Fetch(ctx, s.Config, mail.Filter{Senders: s.Return.From, Since: since, Skip: skip})
	if err != nil {
		return nil, err
	}
	out := make([]returnMail, len(msgs))
	for i, m := range msgs {
		out[i] = returnMail{Message: m, Codes: bauduu.ParseReturnReceipt(m.Subject + "\n" + m.Text)}
	}
	return out, nil
}

// Results of the recipient check of a return.
const (
	recipientUnknown = iota
	recipientConfirmed
	recipientOther
)

var (
	zipInRe = regexp.MustCompile(`\b\d{5}\b`)
	// "Empfänger: Name" or "Empfänger:in" with the address on the next
	// lines; the block ends at the next label or an empty line.
	recipientBlockRe = regexp.MustCompile(`(?im)^[^\S\n]*empfänger(?:\*in|:in|in)?[^\S\n]*:[^\S\n]*((?:.*\n?){0,6})`)
	blockEndRe       = regexp.MustCompile(`(?im)^\s*$|^\s*(?:abgabe\w*|absender\w*|sendungsnummer\w*)`)
)

// recipientBlock is the address a text names after "Empfänger:", or "".
func recipientBlock(text string) string {
	m := recipientBlockRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	block := m[1]
	// The first line is part of the block even when it is empty.
	first, rest, _ := strings.Cut(block, "\n")
	if end := blockEndRe.FindStringIndex(rest); end != nil {
		rest = rest[:end[0]]
	}
	return strings.TrimSpace(first + "\n" + rest)
}

// matchAddress checks an address against the return settings.
func matchAddress(r returnSettings, addr string) int {
	zips := zipInRe.FindAllString(addr, -1)
	if len(zips) > 0 && !slices.Contains(zips, r.Zip) {
		return recipientOther
	}
	hasName := r.Recipient == "" || strings.Contains(strings.ToLower(addr), strings.ToLower(r.Recipient))
	switch {
	case !hasName:
		return recipientOther
	case len(zips) > 0:
		return recipientConfirmed
	}
	return recipientUnknown
}

// checkRecipient tells whether a parcel goes to the configured return
// address. The receipt does not name the recipient, and DHL names it only
// when asked with the recipient's postal code, so mostly nothing is known:
// only an address that names someone else or another postal code rules a
// parcel out.
func checkRecipient(r returnSettings, text string, tr *dhl.Shipment) int {
	// Both sources count; a mismatch in either rules the parcel out.
	verdicts := []int{receiptVerdict(r, text), trackingVerdict(r, tr)}
	switch {
	case slices.Contains(verdicts, recipientOther):
		return recipientOther
	case slices.Contains(verdicts, recipientConfirmed):
		return recipientConfirmed
	}
	return recipientUnknown
}

// receiptVerdict judges the receipt text. An address labeled as the
// recipient decides; the receipt's other places (the Packstation) say
// nothing about where the parcel goes.
func receiptVerdict(r returnSettings, text string) int {
	if block := recipientBlock(text); block != "" {
		return matchAddress(r, block)
	}
	name := strings.ToLower(r.Recipient)
	if name != "" && strings.Contains(strings.ToLower(text), name) && slices.Contains(zipInRe.FindAllString(text, -1), r.Zip) {
		return recipientConfirmed
	}
	return recipientUnknown
}

// trackingVerdict judges DHL's answer.
func trackingVerdict(r returnSettings, tr *dhl.Shipment) int {
	if tr == nil {
		return recipientUnknown
	}
	if zips := zipInRe.FindAllString(tr.Destination, -1); len(zips) > 0 && !slices.Contains(zips, r.Zip) {
		return recipientOther
	}
	if tr.Recipient != "" {
		// The API names the recipient only for the right postal code.
		name := strings.ToLower(r.Recipient)
		if name == "" || strings.Contains(strings.ToLower(tr.Recipient), name) {
			return recipientConfirmed
		}
		return recipientOther
	}
	return recipientUnknown
}

// returnLoan is the set a parcel dropped off at day sent back: the last one
// received before, still at home then.
func returnLoan(loans []bauduu.Loan, sent time.Time) (bauduu.Loan, bool) {
	var best bauduu.Loan
	found := false
	day := sent.In(time.Local)
	for _, l := range loans {
		// A set shipped that day cannot have arrived and gone back yet.
		if l.Placeholder || l.Shipped.IsZero() || l.Shipped.DaysUntil(day) < 1 {
			continue
		}
		if !l.Returned.IsZero() && l.Returned.DaysUntil(day) > 0 {
			continue
		}
		if !found || l.Shipped.DaysUntil(best.Shipped.Time) < 0 || l.Shipped == best.Shipped && l.ID > best.ID {
			best, found = l, true
		}
	}
	return best, found
}

// pendingReturns are the sets at home whose return is not yet known.
func pendingReturns(loans []bauduu.Loan, known []trackedShipment) []bauduu.Loan {
	have := make(map[int]bool, len(known))
	for _, s := range known {
		have[s.LoanID] = true
	}
	var out []bauduu.Loan
	for _, l := range loans {
		if l.Active() && !l.Shipped.IsZero() && !have[l.ID] {
			out = append(out, l)
		}
	}
	return out
}

// returnUpdate is the outcome of one round of return tracking.
type returnUpdate struct {
	Returns  []trackedShipment
	MailSeen []string
	Changes  []loanChange
}

// keepReturnDays is how long a delivered return stays in the state after
// Bauduu booked it.
const keepReturnDays = 60

// updateReturns follows the parcels sent back to Bauduu: while a set is at
// home without a known return, it looks for DHL's drop-off receipt in the
// mailbox and links its code to the last set received; then it queries DHL
// until the parcel is delivered. inbound are the codes of parcels from
// Bauduu, which are never returns.
func updateReturns(ctx context.Context, env *trackEnv, loans []bauduu.Loan, prev []trackedShipment, seen []string, inbound []trackedShipment, now time.Time) returnUpdate {
	if env.returnsOff {
		// Broken settings must not drop or misjudge known returns.
		return returnUpdate{Returns: prev, MailSeen: seen}
	}
	byID := make(map[int]bauduu.Loan, len(loans))
	for _, l := range loans {
		byID[l.ID] = l
	}
	var u returnUpdate
	for _, s := range prev {
		l, ok := byID[s.LoanID]
		if !ok {
			continue
		}
		if !l.Active() && (s.done() || now.Sub(s.MailDate) > keepReturnDays*24*time.Hour) {
			continue
		}
		s.Set = loanLabel(l)
		s.Return = true
		u.Returns = append(u.Returns, s)
	}
	u.MailSeen = seen
	seenSet := make(map[string]bool, len(seen))
	for _, id := range seen {
		seenSet[id] = true
	}
	markSeen := func(id string) {
		if id != "" && !seenSet[id] {
			seenSet[id] = true
			u.MailSeen = append(u.MailSeen, id)
		}
	}
	queried := map[string]bool{}

	if pending := pendingReturns(loans, u.Returns); len(pending) > 0 && env.mail != nil {
		since := pending[0].Shipped.Time
		for _, l := range pending[1:] {
			if l.Shipped.Time.Before(since) {
				since = l.Shipped.Time
			}
		}
		mails, err := fetchReturnMails(ctx, *env.mail, since, func(id string) bool { return seenSet[id] })
		if err != nil {
			env.warn(fmt.Errorf("mail (returns): %w", err))
		} else {
			known := map[string]bool{}
			for _, s := range append(append([]trackedShipment(nil), inbound...), u.Returns...) {
				known[s.Code] = true
			}
			has := map[int]bool{}
			for _, s := range u.Returns {
				has[s.LoanID] = true
			}
			// Oldest first: the first receipt after a set arrived is its
			// return, as in a run right after each drop-off.
			found := 0
			for _, m := range mails {
				// A receipt is read once its codes are decided; one for a
				// set that already has a return is kept for the case that
				// the return turns out to go elsewhere.
				decided := true
				for _, code := range m.Codes {
					if known[code] {
						continue
					}
					l, ok := returnLoan(loans, m.Date)
					if ok && l.Active() && has[l.ID] {
						decided = false
						continue
					}
					known[code] = true
					if !ok || !l.Active() {
						continue
					}
					s := trackedShipment{
						LoanID: l.ID, Set: loanLabel(l), Carrier: "DHL", Code: code,
						MailID: m.ID, MailDate: m.Date, Return: true,
					}
					var cur *dhl.Shipment
					var terr error
					if env.dhl != nil {
						cur, terr = env.dhl.Track(ctx, code, env.returns.Zip)
						queried[code] = true
					}
					verdict := checkRecipient(env.returns, m.Text, cur)
					if verdict == recipientOther {
						if env.verbose != nil {
							fmt.Fprintf(env.verbose, "Sendung %s geht nicht an %s %s\n", code, env.returns.Recipient, env.returns.Zip)
						}
						continue
					}
					if terr != nil {
						env.warn(fmt.Errorf("%s (Rücksendung %s): %w", code, shortSet(s.Set), terr))
					}
					s.Confirmed = verdict == recipientConfirmed
					has[l.ID] = true
					found++
					line := fmt.Sprintf("Rücksendung von %s eingeliefert: DHL %s %s", s.Set, code, dhl.TrackingURL(code))
					if !s.Confirmed {
						line += fmt.Sprintf(" (Empfänger %s %s nicht bestätigt)", env.returns.Recipient, env.returns.Zip)
					}
					u.Changes = append(u.Changes, loanChange{
						Title:    "Rücksendung " + shortSet(s.Set) + " eingeliefert",
						Line:     line,
						Shipment: true,
						Set:      returnLabel(s),
						Note:     "Sendungsnummer da",
					})
					if cur != nil {
						u.Changes = append(u.Changes, trackingChanges(&s, nil, cur)...)
						s.Tracking = cur
						s.Fallback = terr != nil
						s.Checked = now.UTC().Truncate(time.Second)
					}
					u.Returns = append(u.Returns, s)
				}
				if decided {
					markSeen(m.ID)
				}
			}
			if len(u.MailSeen) > maxMailSeen {
				u.MailSeen = u.MailSeen[len(u.MailSeen)-maxMailSeen:]
			}
			if env.verbose != nil && found == 0 {
				fmt.Fprintf(env.verbose, "Keine neue Rücksendung (%d Mails von %s geprüft)\n", len(mails), strings.Join(env.returns.From, ", "))
			}
		}
	}
	sort.SliceStable(u.Returns, func(i, j int) bool { return u.Returns[i].LoanID > u.Returns[j].LoanID })

	if env.dhl == nil {
		return u
	}
	kept := u.Returns[:0]
	for i := range u.Returns {
		s := &u.Returns[i]
		if queried[s.Code] || !s.trackable() || s.done() {
			kept = append(kept, *s)
			continue
		}
		cur, err := env.dhl.Track(ctx, s.Code, env.returns.Zip)
		if err != nil {
			env.warn(fmt.Errorf("%s (Rücksendung %s): %w", s.Code, shortSet(s.Set), err))
			if cur == nil {
				kept = append(kept, *s)
				continue
			}
		}
		switch checkRecipient(env.returns, "", cur) {
		case recipientConfirmed:
			s.Confirmed = true
		case recipientOther:
			// Not the return after all; the set waits for its real one.
			u.Changes = append(u.Changes, loanChange{
				Title:    returnLabel(*s) + ": falsche Sendung",
				Line:     fmt.Sprintf("DHL %s geht nicht an %s %s, keine Rücksendung von %s", s.Code, env.returns.Recipient, env.returns.Zip, s.Set),
				Shipment: true,
			})
			continue
		}
		u.Changes = append(u.Changes, trackingChanges(s, s.Tracking, cur)...)
		s.Tracking = cur
		s.Fallback = err != nil
		s.Checked = now.UTC().Truncate(time.Second)
		kept = append(kept, *s)
	}
	u.Returns = kept
	return u
}

// returnLabel names a return in titles: "Rücksendung Riesenrad 3in1".
func returnLabel(s trackedShipment) string {
	return "Rücksendung " + shortSet(s.Set)
}
