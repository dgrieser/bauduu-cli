package bauduu

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

var (
	csrfTokenRe   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	setNumberRe   = regexp.MustCompile(`(\d{4,7})\s*$`)
	piecesRe      = regexp.MustCompile(`([\d.]+)\s*(?:Bau)?[Tt]eile`)
	minifigsRe    = regexp.MustCompile(`(\d+|keine)\s+Minifigur`)
	yearRe        = regexp.MustCompile(`\b((?:19|20)\d{2})\b`)
	germanDateRe  = regexp.MustCompile(`(\d{1,2})\.\s*(\p{L}+)\s+(\d{4})`)
	shortDateRe   = regexp.MustCompile(`(-?\d{1,2})/(\d{1,2})/(-?\d{4})`)
	expiredDateRe = regexp.MustCompile(`\(([^)]+)\)`)
	spaceRe       = regexp.MustCompile(`[\s\x{00a0}]+`)
)

var germanMonths = map[string]time.Month{
	"januar": time.January, "februar": time.February, "märz": time.March,
	"april": time.April, "mai": time.May, "juni": time.June, "juli": time.July,
	"august": time.August, "september": time.September, "oktober": time.October,
	"november": time.November, "dezember": time.December,
}

// sensitiveKeys are masked by the CLI unless explicitly requested.
var sensitiveKeys = map[string]bool{"iban": true}

func clean(s string) string {
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

func atoiLoose(s string) int {
	n, _ := strconv.Atoi(strings.ReplaceAll(clean(s), ".", ""))
	return n
}

// parseISODate parses YYYY-MM-DD; 0000-00-00 and empty yield the zero Date.
func parseISODate(s string) Date {
	t, err := time.Parse(time.DateOnly, clean(s))
	if err != nil {
		return Date{}
	}
	return Date{t}
}

// parseShortDate parses the PayPlans DD/MM/YYYY format. Its null date is
// 30/11/-0001, which this rejects along with anything else malformed.
func parseShortDate(s string) Date {
	m := shortDateRe.FindStringSubmatch(s)
	if m == nil || strings.HasPrefix(m[3], "-") {
		return Date{}
	}
	t, err := time.Parse("2/1/2006", m[1]+"/"+m[2]+"/"+m[3])
	if err != nil {
		return Date{}
	}
	return Date{t}
}

// parseGermanDate parses "Dienstag, 11. November 2025".
func parseGermanDate(s string) Date {
	m := germanDateRe.FindStringSubmatch(s)
	if m == nil {
		return Date{}
	}
	month, ok := germanMonths[strings.ToLower(m[2])]
	if !ok {
		return Date{}
	}
	day, _ := strconv.Atoi(m[1])
	year, _ := strconv.Atoi(m[3])
	return Date{time.Date(year, month, day, 0, 0, 0, 0, time.UTC)}
}

// isLoggedIn reports whether the page carries the logout form, which Joomla
// only renders for an authenticated session.
func isLoggedIn(doc *goquery.Document) bool {
	return doc.Find(`input[name="task"][value="user.logout"]`).Length() > 0
}

// loginToken returns the Joomla CSRF token field name of the login form.
func loginToken(doc *goquery.Document) (string, error) {
	var token string
	doc.Find(`form`).EachWithBreak(func(_ int, form *goquery.Selection) bool {
		if form.Find(`input[name="task"][value="user.login"]`).Length() == 0 &&
			!strings.Contains(form.AttrOr("action", ""), "user.login") {
			return true
		}
		form.Find(`input[type="hidden"][value="1"]`).EachWithBreak(func(_ int, in *goquery.Selection) bool {
			if name := in.AttrOr("name", ""); csrfTokenRe.MatchString(name) {
				token = name
				return false
			}
			return true
		})
		return token == ""
	})
	if token == "" {
		return "", errors.New("login form token not found")
	}
	return token, nil
}

// systemMessage returns Joomla's flash message (e.g. a failed login).
func systemMessage(doc *goquery.Document) string {
	return clean(doc.Find(`#system-message-container .alert p, #system-message-container .alert-message`).First().Text())
}

// ParseWishlist parses the Ausleihliste page. base resolves relative links.
func ParseWishlist(doc *goquery.Document, base *url.URL) ([]WishlistItem, error) {
	list := doc.Find(`.wishlist-page`)
	if list.Length() == 0 {
		return nil, errors.New("wishlist container not found")
	}
	var items []WishlistItem
	list.Find(`li.product-item`).Each(func(i int, li *goquery.Selection) {
		link := li.Find(`.product-name a`).First()
		item := WishlistItem{
			Position:    i + 1,
			Name:        clean(link.Text()),
			URL:         resolve(base, link.AttrOr("href", "")),
			ImageURL:    resolve(base, li.Find(`img`).First().AttrOr("src", "")),
			Description: clean(li.Find(`.product-s-desc`).Text()),
			ProductID:   atoiLoose(li.Find(`input[name="product_id"]`).AttrOr("value", "")),
		}
		if item.ProductID == 0 {
			item.ProductID = atoiLoose(li.Find(`input[name="virtuemart_product_id[]"]`).AttrOr("value", ""))
		}
		if m := setNumberRe.FindStringSubmatch(item.Name); m != nil {
			item.SetNumber = m[1]
		}
		item.Theme = themeFromURL(link.AttrOr("href", ""))
		applyDescription(&item, item.Description)
		item.Rating, _ = strconv.ParseFloat(li.Find(`meta[itemprop="ratingValue"]`).AttrOr("content", ""), 64)
		item.RatingCount = atoiLoose(li.Find(`meta[itemprop="ratingCount"]`).AttrOr("content", ""))
		item.InStock = strings.HasSuffix(li.Find(`meta[itemprop="availability"]`).AttrOr("content", ""), "/InStock")
		items = append(items, item)
	})
	return items, nil
}

// themeFromURL extracts "CITY" from /lego/themenwelt/city/<slug>-detail,
// upper-cased like the Themenwelt field on the product page.
func themeFromURL(href string) string {
	parts := strings.Split(strings.Trim(href, "/"), "/")
	for i, p := range parts {
		if p == "themenwelt" && i+2 < len(parts) {
			return strings.ToUpper(strings.ReplaceAll(parts[i+1], "-", " "))
		}
	}
	return ""
}

// applyDescription pulls piece count, minifigures and release year out of the
// free-text product blurb, which phrases them in several ways.
func applyDescription(item *WishlistItem, desc string) {
	if m := piecesRe.FindStringSubmatch(desc); m != nil {
		item.Pieces = atoiLoose(m[1])
	}
	if m := minifigsRe.FindStringSubmatch(desc); m != nil {
		n := 0
		if m[1] != "keine" {
			n, _ = strconv.Atoi(m[1])
		}
		item.Minifigures = &n
	}
	if m := yearRe.FindStringSubmatch(desc); m != nil {
		item.Year, _ = strconv.Atoi(m[1])
	}
}

func resolve(base *url.URL, href string) string {
	if href == "" || base == nil {
		return href
	}
	u, err := base.Parse(href)
	if err != nil {
		return href
	}
	return u.String()
}

// ProductDetails holds the fields only the product page shows.
type ProductDetails struct {
	SKU     string
	Theme   string
	Package string
	Pieces  int
}

// ParseProduct parses a product detail page.
func ParseProduct(doc *goquery.Document) ProductDetails {
	d := ProductDetails{SKU: clean(doc.Find(`[itemprop="sku"]`).First().Text())}
	doc.Find(`.product-field`).Each(func(_ int, f *goquery.Selection) {
		title := strings.TrimSuffix(clean(f.Find(`.product-fields-title`).Text()), ":")
		value := clean(f.Find(`.product-field-display`).Text())
		switch title {
		case "Themenwelt":
			d.Theme = value
		case "Ausleihpaket":
			d.Package = value
		}
	})
	doc.Find(`.product-box`).EachWithBreak(func(_ int, s *goquery.Selection) bool {
		if _, after, ok := strings.Cut(clean(s.Text()), "Produkteinheiten pro Packung:"); ok {
			if fields := strings.Fields(after); len(fields) > 0 {
				d.Pieces = atoiLoose(fields[0])
			}
			return false
		}
		return true
	})
	return d
}

// Apply merges product page details into a wishlist item.
func (d ProductDetails) Apply(item *WishlistItem) {
	if d.SKU != "" {
		item.SKU = d.SKU
		if n, _, _ := strings.Cut(d.SKU, "-"); n != "" {
			item.SetNumber = n
		}
	}
	if d.Theme != "" {
		item.Theme = d.Theme
	}
	if d.Package != "" {
		item.Package = d.Package
	}
	if item.Pieces == 0 && d.Pieces > 0 {
		item.Pieces = d.Pieces
	}
}

// ParseLoans parses the "Bisherige Sets" table (meine-sets).
func ParseLoans(doc *goquery.Document) ([]Loan, error) {
	table := doc.Find(`table#verleihdetailList`)
	if table.Length() == 0 {
		return nil, errors.New("loan table not found")
	}
	// Map header titles to column indexes so a reordered table still parses.
	cols := map[string]int{}
	table.Find(`thead th`).Each(func(i int, th *goquery.Selection) {
		title := clean(th.Find(`a`).AttrOr("title", ""))
		if title == "" {
			title = clean(th.Text())
		}
		cols[title] = i
	})
	for _, want := range []string{"Artikel Nr", "Artikel Name", "Versendet Datum", "Retour Datum", "Status"} {
		if _, ok := cols[want]; !ok {
			return nil, fmt.Errorf("loan table: column %q not found", want)
		}
	}
	var loans []Loan
	table.Find(`tbody tr`).Each(func(_ int, tr *goquery.Selection) {
		cells := tr.Find(`td`)
		cell := func(title string) string {
			i, ok := cols[title]
			if !ok || i >= cells.Length() {
				return ""
			}
			return clean(cells.Eq(i).Text())
		}
		l := Loan{
			ID:          atoiLoose(cell("Ids")),
			SKU:         cell("Artikel Nr"),
			Name:        cell("Artikel Name"),
			Internal:    cell("Artikel Intern"),
			Shipped:     parseISODate(cell("Versendet Datum")),
			Returned:    parseISODate(cell("Retour Datum")),
			Status:      cell("Status"),
			BonusBricks: atoiLoose(cell("Meine Bonussteine")),
		}
		l.Placeholder = l.SKU == "W"
		if l.SKU == "" && l.Name == "" {
			return
		}
		loans = append(loans, l)
	})
	return loans, nil
}

// ParseSubscriptions parses the PayPlans booking dashboard.
func ParseSubscriptions(doc *goquery.Document) ([]Subscription, error) {
	orders := doc.Find(`.pp-orders`)
	if orders.Length() == 0 {
		return nil, errors.New("subscription list not found")
	}
	var subs []Subscription
	orders.Find(`.o-card`).Each(func(_ int, card *goquery.Selection) {
		title := clean(card.Find(`.o-card__title`).First().Text())
		if title == "" {
			return
		}
		s := Subscription{Title: title}
		card.Find(`.o-card__desc span`).EachWithBreak(func(_ int, span *goquery.Selection) bool {
			if t := clean(span.Text()); strings.HasPrefix(t, "#") {
				s.Key = strings.TrimPrefix(t, "#")
				return false
			}
			return true
		})
		s.Created = parseShortDate(card.Find(`.pp-subscription-date`).Text())
		s.Status = clean(card.Find(`.o-label`).First().Text())
		s.Expired = strings.HasPrefix(strings.ToUpper(s.Status), "ABGELAUFEN")
		card.Find(`.o-card--meta`).Each(func(_ int, meta *goquery.Selection) {
			label := clean(meta.Find(`.flex-grow`).First().Text())
			body := meta.Find(`.flex-grow`).First().Next()
			switch label {
			case "Preis":
				lines := body.Find(`div`).FilterFunction(func(_ int, d *goquery.Selection) bool {
					return d.Children().Filter(`span.pp-amount`).Length() > 0 ||
						d.Children().Filter(`b`).Find(`span.pp-amount`).Length() > 0
				})
				lines.Each(func(_ int, d *goquery.Selection) {
					s.Prices = append(s.Prices, strings.TrimSuffix(clean(d.Text()), ","))
				})
				if len(s.Prices) == 0 {
					if t := clean(body.Text()); t != "" {
						s.Prices = []string{t}
					}
				}
			case "Abonnementzeitraum":
				dates := body.Find(`b`)
				if dates.Length() >= 2 {
					s.Start = parseShortDate(dates.Eq(0).Text())
					s.End = parseShortDate(dates.Eq(1).Text())
				} else {
					s.Period = clean(body.Text())
				}
			}
		})
		if s.Expired && s.End.IsZero() {
			if m := expiredDateRe.FindStringSubmatch(s.Status); m != nil {
				s.End = parseShortDate(m[1])
			}
		}
		subs = append(subs, s)
	})
	return subs, nil
}

// parseProfileCore reads the Joomla user profile page (login/profile).
func parseProfileCore(doc *goquery.Document, p *Profile) {
	doc.Find(`#users-profile-core dt`).Each(func(_ int, dt *goquery.Selection) {
		value := clean(dt.NextFiltered(`dd`).Text())
		switch clean(dt.Text()) {
		case "Name":
			p.Name = value
		case "Benutzername":
			p.Username = value
		case "Registrierungsdatum":
			p.Registered = parseGermanDate(value)
		case "Datum des letzten Besuchs":
			p.LastVisit = parseGermanDate(value)
		}
	})
	if href, ok := doc.Find(`a[href*="profile.edit"]`).Attr("href"); ok {
		if u, err := url.Parse(href); err == nil {
			p.UserID = atoiLoose(u.Query().Get("user_id"))
		}
	}
}

// parseCustomerNumber reads the read-only Kundennummer field of the
// Retoure/Pause forms.
func parseCustomerNumber(doc *goquery.Document) string {
	return clean(doc.Find(`input#kundennr, input[name="form[kundennr]"]`).First().AttrOr("value", ""))
}

// parseProfileForm reads "Deine Daten" (PayPlans preferences form). Fields
// are grouped under the nearest preceding section heading; passwords and the
// fixed SEPA mandate text are skipped.
func parseProfileForm(doc *goquery.Document, p *Profile) error {
	form := doc.Find(`#pp`)
	if form.Length() == 0 || form.Find(`[data-pp-form-group]`).Length() == 0 {
		return errors.New("customer data form not found")
	}
	section := ""
	form.Find(`div.text-lg.font-bold, [data-pp-form-group]`).Each(func(_ int, s *goquery.Selection) {
		if s.Is(`div.text-lg.font-bold`) {
			section = titleCase(clean(s.Text()))
			return
		}
		label := strings.TrimPrefix(clean(s.Find(`label`).First().Text()), "*")
		label = strings.TrimSuffix(strings.TrimSpace(label), "(optional)")
		label = strings.TrimSpace(label)
		var key, value string
		if in := s.Find(`input`).First(); in.Length() > 0 {
			if in.AttrOr("type", "text") == "password" {
				return
			}
			key, value = fieldKey(in), in.AttrOr("value", "")
		} else if sel := s.Find(`select`).First(); sel.Length() > 0 {
			key = fieldKey(sel)
			opt := sel.Find(`option[selected]`).First()
			if opt.Length() == 0 {
				opt = sel.Find(`option`).First()
			}
			value = opt.Text()
		} else {
			return
		}
		value = clean(value)
		f := Field{Section: section, Key: key, Label: label, Value: value, Sensitive: sensitiveKeys[strings.ToLower(key)]}
		p.Fields = append(p.Fields, f)
		switch key {
		case "name":
			p.Name = value
		case "username":
			p.Username = value
		case "email":
			p.Email = value
		}
	})
	return nil
}

// fieldKey prefers the inner key of userparams[key]-style ids.
func fieldKey(s *goquery.Selection) string {
	id := s.AttrOr("id", s.AttrOr("name", ""))
	if inner, ok := strings.CutPrefix(id, "userparams["); ok {
		return strings.TrimSuffix(inner, "]")
	}
	return id
}

// titleCase turns "ABWEICHENDE LIEFERANSCHRIFT" into "Abweichende Lieferanschrift"
// and leaves mixed-case headings alone.
func titleCase(s string) string {
	if s != strings.ToUpper(s) {
		return s
	}
	words := strings.Fields(strings.ToLower(s))
	for i, w := range words {
		parts := strings.Split(w, "-")
		for j, part := range parts {
			r := []rune(part)
			if len(r) > 0 {
				r[0] = []rune(strings.ToUpper(string(r[0])))[0]
			}
			parts[j] = string(r)
		}
		words[i] = strings.Join(parts, "-")
	}
	return strings.Join(words, " ")
}
