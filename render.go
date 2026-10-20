package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dgrieser/bauduu-cli/internal/bauduu"
)

// stripSetNumber drops a trailing " 60124" from a name when the set number
// already has its own column.
func stripSetNumber(name, number string) string {
	if number == "" {
		return name
	}
	if trimmed, ok := strings.CutSuffix(name, " "+number); ok && trimmed != "" {
		return trimmed
	}
	return name
}

func intOrBlank(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func daysCell(n int) string {
	if n < 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// maskValue keeps the first and last four characters of a sensitive value.
func maskValue(s string) string {
	r := []rune(strings.ReplaceAll(s, " ", ""))
	if len(r) <= 8 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:4]) + " **** " + string(r[len(r)-4:])
}

func wishlistTable(items []bauduu.WishlistItem) *table {
	t := &table{
		Title: fmt.Sprintf("Ausleihliste (%d)", len(items)),
		Columns: []column{
			{Title: "#", Align: alignRight},
			{Title: "Set"},
			{Title: "Name", Flex: true, MinWidth: 20},
			{Title: "Thema", Drop: 4},
			{Title: "Paket"},
			{Title: "Teile", Align: alignRight},
			{Title: "Figuren", Align: alignRight, Drop: 2},
			{Title: "Jahr", Align: alignRight, Drop: 3},
			{Title: "Bewertung", Drop: 1},
		},
	}
	pieces := 0
	for _, it := range items {
		figs := ""
		if it.Minifigures != nil {
			figs = strconv.Itoa(*it.Minifigures)
		}
		rating := ""
		if it.RatingCount > 0 {
			rating = fmt.Sprintf("%.1f/5 (%d)", it.Rating, it.RatingCount)
		}
		t.add(strconv.Itoa(it.Position), it.SetNumber, stripSetNumber(it.Name, it.SetNumber),
			it.Theme, it.Package, intOrBlank(it.Pieces), figs, intOrBlank(it.Year), rating)
		pieces += it.Pieces
	}
	if len(items) > 0 {
		t.Footer = append(t.Footer, fmt.Sprintf("%d Sets in der Warteschlange · %d Teile gesamt", len(items), pieces))
	}
	return t
}

func loanStatusStyle(l bauduu.Loan) string {
	switch {
	case l.Active():
		return ansiBold + ansiYellow
	case l.Placeholder:
		return ""
	default:
		return ansiGreen
	}
}

func loansTable(loans []bauduu.Loan, now time.Time) *table {
	t := &table{
		Title: "Bisherige Sets",
		Columns: []column{
			{Title: "Set"},
			{Title: "Name", Flex: true, MinWidth: 12},
			{Title: "Versendet"},
			{Title: "Retour"},
			{Title: "Tage", Align: alignRight},
			{Title: "Status"},
			{Title: "Bonus", Align: alignRight, Drop: 1},
		},
	}
	sets, active, bonus := 0, 0, 0
	for _, l := range loans {
		set := l.SetNumber()
		if l.Placeholder {
			set = "–"
		}
		r := t.add(set, stripSetNumber(l.Name, set), l.Shipped.String(), l.Returned.String(),
			daysCell(l.Days(now)), l.Status, intOrBlank(l.BonusBricks))
		if l.Placeholder {
			r.Style = ansiDim
		} else {
			sets++
			r.CellStyles = []string{5: loanStatusStyle(l)}
		}
		if l.Active() {
			active++
			r.Style = ansiBold
		}
		bonus += l.BonusBricks
	}
	if len(loans) > 0 {
		t.Footer = append(t.Footer, fmt.Sprintf("%d Sets geliehen · %d aktuell ausgeliehen · %d Bonussteine", sets, active, bonus))
	}
	return t
}

// returns are the finished loans, i.e. everything except placeholders and
// the set currently at home.
func returnedLoans(loans []bauduu.Loan) []bauduu.Loan {
	var out []bauduu.Loan
	for _, l := range loans {
		if !l.Placeholder && !l.Returned.IsZero() {
			out = append(out, l)
		}
	}
	return out
}

func activeLoans(loans []bauduu.Loan) []bauduu.Loan {
	var out []bauduu.Loan
	for _, l := range loans {
		if l.Active() {
			out = append(out, l)
		}
	}
	return out
}

func returnsTable(loans []bauduu.Loan, customerNumber string, now time.Time) *table {
	returned := returnedLoans(loans)
	t := &table{
		Title: fmt.Sprintf("Retouren (%d)", len(returned)),
		Columns: []column{
			{Title: "#", Align: alignRight, Drop: 2},
			{Title: "Set"},
			{Title: "Name", Flex: true, MinWidth: 12},
			{Title: "Versendet"},
			{Title: "Retour"},
			{Title: "Tage", Align: alignRight},
			{Title: "Bonus", Align: alignRight, Drop: 1},
		},
	}
	days, bonus := 0, 0
	for i, l := range returned {
		d := l.Days(now)
		t.add(strconv.Itoa(len(returned)-i), l.SetNumber(), stripSetNumber(l.Name, l.SetNumber()),
			l.Shipped.String(), l.Returned.String(), daysCell(d), intOrBlank(l.BonusBricks))
		days += max(d, 0)
		bonus += l.BonusBricks
	}
	if len(returned) > 0 {
		t.Footer = append(t.Footer, fmt.Sprintf("%d Retouren · Ø %.0f Tage ausgeliehen · %d Bonussteine",
			len(returned), float64(days)/float64(len(returned)), bonus))
	}
	for _, l := range activeLoans(loans) {
		t.Footer = append(t.Footer, fmt.Sprintf("Aktuell zu Hause: %s (%s), versendet %s, seit %d Tagen",
			stripSetNumber(l.Name, l.SetNumber()), l.SetNumber(), l.Shipped, l.Days(now)))
	}
	if customerNumber != "" {
		t.Footer = append(t.Footer, "Kundennummer für die Retoure-Anmeldung: "+customerNumber)
	}
	return t
}

func subscriptionsTable(subs []bauduu.Subscription) *table {
	t := &table{
		Title: "Buchungen",
		Columns: []column{
			{Title: "Paket", Flex: true, MinWidth: 14},
			{Title: "Nr", Drop: 1},
			{Title: "Erstellt", Drop: 2},
			{Title: "Status", Flex: true, MinWidth: 12},
			{Title: "Preis", Flex: true, MinWidth: 20},
			{Title: "Zeitraum", Flex: true, MinWidth: 12},
		},
	}
	for _, s := range subs {
		period := s.Period
		if !s.Start.IsZero() {
			period = s.Start.String() + " – " + s.End.String()
		}
		status := s.Status
		if s.Expired && !s.End.IsZero() {
			status = "ABGELAUFEN (" + s.End.String() + ")"
		}
		r := t.add(s.Title, s.Key, s.Created.String(), status, strings.Join(s.Prices, "\n"), period)
		code := ansiGreen
		if s.Expired {
			code = ansiRed
			r.Style = ansiDim
		}
		r.CellStyles = []string{3: code}
	}
	return t
}

func profileTable(p *bauduu.Profile, reveal bool) *table {
	t := &table{
		Title: "Deine Daten",
		Columns: []column{
			{Title: "Bereich", Flex: true, MinWidth: 15},
			{Title: "Feld", Flex: true, MinWidth: 12},
			{Title: "Wert", Flex: true, MinWidth: 16},
		},
	}
	userID := ""
	if p.UserID > 0 {
		userID = strconv.Itoa(p.UserID)
	}
	rows := [][2]string{
		{"Name", p.Name},
		{"Benutzername", p.Username},
		{"E-Mail", p.Email},
		{"Kundennummer", p.CustomerNumber},
		{"Benutzer-ID", userID},
		{"Registriert", p.Registered.String()},
		{"Letzter Besuch", p.LastVisit.String()},
	}
	section := "Konto"
	for _, r := range rows {
		t.add(section, r[0], r[1])
		section = ""
	}
	last := "Konto"
	for _, f := range p.Fields {
		// Name, username and email are already in the Konto block.
		if f.Section == "Kontoeinstellungen" {
			continue
		}
		value := f.Value
		if f.Sensitive && !reveal && value != "" {
			value = maskValue(value)
		}
		section := f.Section
		if section == last {
			section = ""
		} else {
			last = section
		}
		t.add(section, f.Label, value)
	}
	if !reveal {
		for _, f := range p.Fields {
			if f.Sensitive && f.Value != "" {
				t.Footer = append(t.Footer, "Maskierte Werte mit --reveal anzeigen.")
				break
			}
		}
	}
	return t
}

// maskedProfile returns a copy of p with sensitive values masked.
func maskedProfile(p *bauduu.Profile) *bauduu.Profile {
	c := *p
	c.Fields = make([]bauduu.Field, len(p.Fields))
	for i, f := range p.Fields {
		if f.Sensitive && f.Value != "" {
			f.Value = maskValue(f.Value)
		}
		c.Fields[i] = f
	}
	return &c
}
