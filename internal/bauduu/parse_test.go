package bauduu

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// The fixtures are real pages from the site with all personal data replaced.

func fixture(t *testing.T, name string) *goquery.Document {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	doc, err := goquery.NewDocumentFromReader(f)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func date(s string) Date {
	return parseISODate(s)
}

func TestLoginDetection(t *testing.T) {
	for name, want := range map[string]bool{
		"login.html":        false,
		"login_failed.html": false,
		"profile.html":      true,
		"meine_sets.html":   true,
		"myuser.html":       true,
	} {
		if got := isLoggedIn(fixture(t, name)); got != want {
			t.Errorf("isLoggedIn(%s) = %v, want %v", name, got, want)
		}
	}
}

func TestLoginToken(t *testing.T) {
	token, err := loginToken(fixture(t, "login.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !csrfTokenRe.MatchString(token) {
		t.Errorf("token = %q", token)
	}
}

func TestSystemMessage(t *testing.T) {
	got := systemMessage(fixture(t, "login_failed.html"))
	if !strings.Contains(got, "Benutzername und Passwort falsch") {
		t.Errorf("systemMessage = %q", got)
	}
}

func TestParseWishlist(t *testing.T) {
	base, _ := url.Parse(DefaultBaseURL)
	items, err := ParseWishlist(fixture(t, "ausleihliste.html"), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 11 {
		t.Fatalf("got %d items, want 11", len(items))
	}
	first := items[0]
	if first.Position != 1 || first.ProductID != 243 || first.SetNumber != "60124" ||
		first.Name != "Vulkan-Forscherstation 60124" || first.Theme != "CITY" ||
		first.Pieces != 824 || first.Minifigures == nil || *first.Minifigures != 6 ||
		first.Year != 2016 || first.Rating != 4 || first.RatingCount != 1 {
		t.Errorf("first item = %+v", first)
	}
	if first.URL != "https://www.bauduu.de/lego/themenwelt/city/vulkan-forscherstation-60124-detail" {
		t.Errorf("URL = %q", first.URL)
	}
	byName := map[string]WishlistItem{}
	for _, it := range items {
		byName[it.Name] = it
	}
	// "aus 2125 Teilen", "3929 Bauteile", "2.382 Bauteile", "keine Minifiguren".
	cases := []struct {
		name     string
		pieces   int
		minifigs int
	}{
		{"Burg Himeji 21060", 2125, -1},
		{"Schaufelradbagger 42055", 3929, 0},
		{"Steine-Bank 10251", 2382, 5},
		{"The White House 21054", 1483, 0},
	}
	for _, c := range cases {
		it, ok := byName[c.name]
		if !ok {
			t.Errorf("%s missing", c.name)
			continue
		}
		if it.Pieces != c.pieces {
			t.Errorf("%s pieces = %d, want %d", c.name, it.Pieces, c.pieces)
		}
		got := -1
		if it.Minifigures != nil {
			got = *it.Minifigures
		}
		if got != c.minifigs {
			t.Errorf("%s minifigures = %d, want %d", c.name, got, c.minifigs)
		}
	}
}

func TestParseProduct(t *testing.T) {
	d := ParseProduct(fixture(t, "product.html"))
	want := ProductDetails{SKU: "60124-1", Theme: "CITY", Package: "PROFI", Pieces: 824}
	if d != want {
		t.Errorf("ParseProduct = %+v, want %+v", d, want)
	}
	item := WishlistItem{SetNumber: "x", Theme: "x"}
	d.Apply(&item)
	if item.SKU != "60124-1" || item.SetNumber != "60124" || item.Package != "PROFI" || item.Theme != "CITY" {
		t.Errorf("Apply = %+v", item)
	}
}

func TestParseLoans(t *testing.T) {
	loans, err := ParseLoans(fixture(t, "meine_sets.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(loans) != 11 {
		t.Fatalf("got %d loans, want 11", len(loans))
	}
	want := Loan{
		ID: 90338, SKU: "31119-1", Name: "Riesenrad 3in1 31119", Internal: "D",
		Shipped: date("2026-10-14"), Status: "Ausgeliehen",
	}
	if loans[0] != want {
		t.Errorf("loans[0] = %+v, want %+v", loans[0], want)
	}
	if !loans[0].Active() || loans[0].SetNumber() != "31119" {
		t.Errorf("loans[0] active/set = %v/%s", loans[0].Active(), loans[0].SetNumber())
	}
	if !loans[1].Placeholder || loans[1].Active() || loans[1].Days(time.Now()) != -1 {
		t.Errorf("loans[1] should be a placeholder: %+v", loans[1])
	}
	returned := loans[2]
	if returned.Name != "Arktis Basislager 60036" || returned.BonusBricks != 3 ||
		returned.Returned != date("2026-06-09") || returned.Days(time.Now()) != 42 {
		t.Errorf("loans[2] = %+v days=%d", returned, returned.Days(time.Now()))
	}
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.Local)
	if got := loans[0].Days(now); got != 6 {
		t.Errorf("active loan days = %d, want 6", got)
	}
}

func TestParseSubscriptions(t *testing.T) {
	subs, err := ParseSubscriptions(fixture(t, "dashboard.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 3 {
		t.Fatalf("got %d subscriptions, want 3", len(subs))
	}
	s := subs[0]
	if s.Title != "PREMIUM Flatrate 6 Monate" || s.Key != "8O56WJC0F9ND" || s.Status != "AKTIV" || s.Expired ||
		s.Created != date("2026-10-14") || s.Start != date("2026-10-14") || s.End != date("2027-01-14") {
		t.Errorf("subs[0] = %+v", s)
	}
	if strings.Join(s.Prices, "|") != "95,85 € für die ersten 4 Monate|95,85 € für jede(s) 3 Monate" {
		t.Errorf("subs[0].Prices = %q", s.Prices)
	}
	reg := subs[1]
	if reg.Title != "Registrierung" || !reg.Created.IsZero() || !reg.Start.IsZero() ||
		strings.Join(reg.Prices, "|") != "0,00 €" {
		t.Errorf("subs[1] = %+v", reg)
	}
	old := subs[2]
	if !old.Expired || old.Period != "Abgelaufen" || old.End != date("2026-06-16") || old.Created != date("2025-11-16") {
		t.Errorf("subs[2] = %+v", old)
	}
}

func TestParseProfile(t *testing.T) {
	p := &Profile{}
	if err := parseProfileForm(fixture(t, "myuser.html"), p); err != nil {
		t.Fatal(err)
	}
	parseProfileCore(fixture(t, "profile.html"), p)
	p.CustomerNumber = parseCustomerNumber(fixture(t, "retour.html"))

	if p.Name != "Erika Mustermann" || p.Username != "EMuster" || p.Email != "erika@example.com" ||
		p.UserID != 20002 || p.CustomerNumber != "10001" ||
		p.Registered != date("2025-11-11") || p.LastVisit != date("2026-10-20") {
		t.Errorf("profile = %+v", p)
	}
	fields := map[string]Field{}
	for _, f := range p.Fields {
		fields[f.Key] = f
		if strings.Contains(strings.ToLower(f.Key), "password") || f.Key == "sepahinweis" {
			t.Errorf("field %q should be skipped", f.Key)
		}
	}
	want := map[string][2]string{
		"vorname":    {"Kundendaten", "Erika"},
		"anschrift":  {"Kundendaten", "Musterweg 1"},
		"land":       {"Kundendaten", "Deutschland"},
		"kind":       {"Kundendaten", "Paul"},
		"liefername": {"Abweichende Lieferanschrift", ""},
		"iban":       {"Sepa-Lastschrift", "DE02120300000000202051"},
		"einzug":     {"Sepa-Lastschrift", "15. des Monats"},
		"username":   {"Kontoeinstellungen", "EMuster"},
	}
	for key, w := range want {
		f, ok := fields[key]
		if !ok {
			t.Errorf("field %q missing", key)
			continue
		}
		if f.Section != w[0] || f.Value != w[1] {
			t.Errorf("field %q = %+v, want section %q value %q", key, f, w[0], w[1])
		}
	}
	if !fields["iban"].Sensitive || fields["iban"].Label != "IBAN" {
		t.Errorf("iban field = %+v", fields["iban"])
	}
	if fields["kind"].Label != "Name des Kindes" {
		t.Errorf("kind label = %q", fields["kind"].Label)
	}
}

func TestParseDates(t *testing.T) {
	if !parseISODate("0000-00-00").IsZero() || !parseShortDate("30/11/-0001").IsZero() {
		t.Error("null dates should be zero")
	}
	if got := parseGermanDate("Montag, 3. März 2025"); got != date("2025-03-03") {
		t.Errorf("parseGermanDate = %v", got)
	}
	if got := parseShortDate("ABGELAUFEN (16/06/2026)"); got != date("2026-06-16") {
		t.Errorf("parseShortDate = %v", got)
	}
}
