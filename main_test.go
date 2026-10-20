package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/dgrieser/bauduu-cli/internal/bauduu"
)

func TestTableRender(t *testing.T) {
	tb := &table{
		Title:   "Test",
		Columns: []column{{Title: "Name", Flex: true, MinWidth: 6}, {Title: "Tage", Align: alignRight}},
	}
	tb.add("Zurückgegeben Größe", "7")
	tb.add("zwei\nZeilen", "42")
	tb.Footer = []string{"Fußzeile"}

	var buf bytes.Buffer
	tb.render(&buf, false, 0)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	want := []string{
		"Test",
		"┌─────────────────────┬──────┐",
		"│ Name                │ Tage │",
		"├─────────────────────┼──────┤",
		"│ Zurückgegeben Größe │    7 │",
		"│ zwei                │   42 │",
		"│ Zeilen              │      │",
		"└─────────────────────┴──────┘",
		"Fußzeile",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("render:\n%s\nwant:\n%s", buf.String(), strings.Join(want, "\n"))
	}

	// A narrow terminal shrinks the flex column and wraps its cells.
	buf.Reset()
	tb.render(&buf, false, 24)
	lines = strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	for _, l := range lines[1 : len(lines)-1] {
		if w := runewidth.StringWidth(l); w != 24 {
			t.Errorf("line %q width %d, want 24", l, w)
		}
	}
	if !strings.Contains(buf.String(), "│ Zurückgegeben │    7 │\n│ Größe         │      │") {
		t.Errorf("expected wrapped cell:\n%s", buf.String())
	}

	// Below the minimal layout, droppable columns go and a note says so.
	tb.Columns[1].Drop = 1
	buf.Reset()
	tb.render(&buf, false, 9)
	if strings.Contains(buf.String(), "Tage │") || !strings.Contains(buf.String(), "Ausgeblendet (Terminal zu schmal): Tage") {
		t.Errorf("expected dropped column:\n%s", buf.String())
	}
}

func TestEmptyTable(t *testing.T) {
	tb := &table{Columns: []column{{Title: "#"}, {Title: "Name"}}}
	var buf bytes.Buffer
	tb.render(&buf, false, 0)
	if !strings.Contains(buf.String(), "(keine Einträge)") {
		t.Errorf("empty table:\n%s", buf.String())
	}
}

func TestHelpers(t *testing.T) {
	if got := maskValue("DE02 1203 0000 0000 2020 51"); got != "DE02 **** 2051" {
		t.Errorf("maskValue = %q", got)
	}
	if got := maskValue("short"); got != "*****" {
		t.Errorf("maskValue short = %q", got)
	}
	if got := stripSetNumber("Hauptbahnhof 60469", "60469"); got != "Hauptbahnhof" {
		t.Errorf("stripSetNumber = %q", got)
	}
	if got := stripSetNumber("60469", "60469"); got != "60469" {
		t.Errorf("stripSetNumber bare = %q", got)
	}
}

func testLoans() []bauduu.Loan {
	d := func(s string) bauduu.Date {
		t, _ := time.Parse(time.DateOnly, s)
		return bauduu.Date{Time: t}
	}
	return []bauduu.Loan{
		{ID: 3, SKU: "31119-1", Name: "Riesenrad 3in1 31119", Shipped: d("2026-10-14"), Status: "Ausgeliehen"},
		{ID: 2, SKU: "W", Name: "Nächster Versand ab 2026-06-16", Shipped: d("2026-06-16"), Returned: d("2026-06-16"), Status: "Zurückgegeben", Placeholder: true},
		{ID: 1, SKU: "60036-1", Name: "Arktis Basislager 60036", Shipped: d("2026-04-28"), Returned: d("2026-06-09"), Status: "Zurückgegeben", BonusBricks: 3},
	}
}

func TestReturnsTable(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	var buf bytes.Buffer
	returnsTable(testLoans(), "10001", now).render(&buf, false, 0)
	out := buf.String()
	for _, want := range []string{
		"Retouren (1)",
		"│ 1 │ 60036 │ Arktis Basislager │ 2026-04-28 │ 2026-06-09 │   42 │     3 │",
		"1 Retouren · Ø 42 Tage ausgeliehen · 3 Bonussteine",
		"Aktuell zu Hause: Riesenrad 3in1 (31119), versendet 2026-10-14, seit 6 Tagen",
		"Kundennummer für die Retoure-Anmeldung: 10001",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Nächster Versand") {
		t.Errorf("placeholder rows must not count as returns:\n%s", out)
	}
}

func TestWriteJSONMasksProfile(t *testing.T) {
	a := &account{
		Account: bauduu.Account{
			Loans: testLoans(),
			Profile: &bauduu.Profile{
				CustomerNumber: "10001",
				Fields:         []bauduu.Field{{Key: "iban", Value: "DE02120300000000202051", Sensitive: true}},
			},
		},
		customerNumber: "10001",
	}
	var buf bytes.Buffer
	if err := writeJSON(&buf, a, allSections, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "DE02120300000000202051") {
		t.Error("IBAN must be masked without --reveal")
	}
	var v struct {
		Wishlist []any `json:"wishlist"`
		Returns  struct {
			CustomerNumber string `json:"customer_number"`
			Returned       []any  `json:"returned"`
			Active         []any  `json:"active"`
		} `json:"returns"`
	}
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Wishlist == nil || v.Returns.CustomerNumber != "10001" || len(v.Returns.Returned) != 1 || len(v.Returns.Active) != 1 {
		t.Errorf("json = %s", buf.String())
	}
	// The caller's profile must not be modified by masking.
	if a.Profile.Fields[0].Value != "DE02120300000000202051" {
		t.Error("masking mutated the profile")
	}

	buf.Reset()
	if err := writeJSON(&buf, a, []section{secProfile}, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "DE02120300000000202051") {
		t.Error("--reveal should keep the IBAN")
	}
}

func TestConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config")
	if err := writeConfig(path, credentials{User: "erika", Password: " p=a#ss\" "}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, want 0600", info.Mode().Perm())
	}
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg["user"] != "erika" || cfg["password"] != " p=a#ss\" " {
		t.Errorf("config = %q", cfg)
	}
	// Hand-written unquoted values are trimmed.
	hand := filepath.Join(t.TempDir(), "hand")
	if err := os.WriteFile(hand, []byte("# c\n  User =  bob  \npassword=x y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := readConfig(hand); err != nil || cfg["user"] != "bob" || cfg["password"] != "x y" {
		t.Errorf("hand config = %q, %v", cfg, err)
	}
	if err := os.WriteFile(hand, []byte("password = \"open\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfig(hand); err == nil {
		t.Error("unterminated quote must fail")
	}

	t.Setenv("BAUDUU_USER", "")
	t.Setenv("BAUDUU_PASSWORD", "")
	c, err := resolveCredentials("", path, false)
	if err != nil || c.User != "erika" || c.Password != " p=a#ss\" " {
		t.Errorf("from config: %+v, %v", c, err)
	}
	// The stored password belongs to the stored user only.
	if _, err := resolveCredentials("someone-else", path, false); err == nil {
		t.Error("stored password must not be used for another user")
	}
	t.Setenv("BAUDUU_PASSWORD", "envpw")
	c, err = resolveCredentials("someone-else", path, false)
	if err != nil || c.User != "someone-else" || c.Password != "envpw" {
		t.Errorf("from env: %+v, %v", c, err)
	}
}

func TestConfigOverwriteIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("user = old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(path, credentials{User: "erika", Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}
}

func TestExplicitCredentialsIgnoreBrokenConfig(t *testing.T) {
	broken := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(broken, []byte("not a key value line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BAUDUU_USER", "erika")
	t.Setenv("BAUDUU_PASSWORD", "pw")
	if c, err := resolveCredentials("", broken, false); err != nil || c.User != "erika" || c.Password != "pw" {
		t.Errorf("explicit credentials = %+v, %v", c, err)
	}
	t.Setenv("BAUDUU_PASSWORD", "")
	if _, err := resolveCredentials("", broken, false); err == nil {
		t.Error("broken config must fail when it is needed")
	}
}

func TestMissingConfigIsFine(t *testing.T) {
	cfg, err := readConfig(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(cfg) != 0 {
		t.Errorf("readConfig missing = %v, %v", cfg, err)
	}
}

func TestWrap(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  []string
	}{
		{"Stunttruck Feuerreifen-Challenge", 12, []string{"Stunttruck", "Feuerreifen-", "Challenge"}},
		{"ABGELAUFEN (2026-06-16)", 12, []string{"ABGELAUFEN", "(2026-06-16)"}},
		{"Größenänderung", 6, []string{"Größen", "änderu", "ng"}},
		{"a\nb c", 10, []string{"a", "b c"}},
		{"", 5, []string{""}},
	}
	for _, c := range cases {
		if got := wrap(c.in, c.width); strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("wrap(%q, %d) = %q, want %q", c.in, c.width, got, c.want)
		}
	}
}

func TestTablesFit80Columns(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	figs := 6
	wishlist := []bauduu.WishlistItem{{
		Position: 10, SetNumber: "60357", Name: "Stunttruck Feuerreifen-Challenge 60357", Theme: "ARCHITECTURE",
		Package: "PREMIUM", Pieces: 3929, Minifigures: &figs, Year: 2016, Rating: 4, RatingCount: 12,
	}}
	d := func(s string) bauduu.Date {
		t, _ := time.Parse(time.DateOnly, s)
		return bauduu.Date{Time: t}
	}
	subs := []bauduu.Subscription{{
		Title: "PREMIUM Flatrate 6 Monate", Key: "8O56WJC0F9ND", Created: d("2026-10-14"), Status: "ABGELAUFEN",
		Expired: true, Prices: []string{"95,85 € für die ersten 4 Monate", "95,85 € für jede(s) 3 Monate"},
		Start: d("2026-10-14"), End: d("2027-01-14"),
	}}
	profile := &bauduu.Profile{Name: "Erika Mustermann", Email: "erika.mustermann@example.com", Fields: []bauduu.Field{
		{Section: "Abweichende Lieferanschrift", Label: "Vor- u. Nachname / Postnummer", Value: "Erika Mustermann"},
	}}
	tables := map[string]*table{
		"wishlist": wishlistTable(wishlist),
		"loans":    loansTable(testLoans(), now),
		"returns":  returnsTable(testLoans(), "10001", now),
		"subs":     subscriptionsTable(subs),
		"profile":  profileTable(profile, false),
	}
	for name, tb := range tables {
		var buf bytes.Buffer
		tb.render(&buf, false, 80)
		for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
			if strings.HasPrefix(l, "│") || strings.HasPrefix(l, "┌") {
				if w := runewidth.StringWidth(l); w > 80 {
					t.Errorf("%s: line width %d > 80: %q", name, w, l)
				}
			}
		}
		if strings.Contains(buf.String(), "…") {
			t.Errorf("%s: unexpected truncation at 80 columns:\n%s", name, buf.String())
		}
	}
}
