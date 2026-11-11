package bauduu

import (
	"regexp"
	"slices"
	"strings"
)

// ShippingSender is the address Bauduu sends its shipping notices from.
const ShippingSender = "bauduu@brickcoast.de"

// ShippingNotice is what a "neues LEGO Set wurde von uns versendet" mail
// tells: the carrier and the tracking code of the parcel.
type ShippingNotice struct {
	Carrier string `json:"carrier"`
	Code    string `json:"code"`
	// URL is the carrier link from the mail (often just a generic page).
	URL string `json:"url,omitempty"`
}

var (
	// "Trackingcode: 0034…", also "Sendungsnummer", "Tracking-Nummer", …
	trackingCodeRe = regexp.MustCompile(`(?i)(?:tracking[\s-]*(?:code|nummer|number|id)|sendungs(?:nummer|code)|paketnummer)\s*[:#]?\s*([A-Z0-9]{8,40})\b`)
	// "verfolgen via DHL:" or "an den Versanddienstleister DHL übergeben"
	carrierRe = regexp.MustCompile(`(?i)(?:verfolgen\s+(?:via|bei|über)|versanddienstleister|versand\s+mit)\s+(DHL|DPD|GLS|UPS|Hermes|Deutsche\s+Post)\b`)
	urlLineRe = regexp.MustCompile(`(?im)^\s*URL\s*:\s*(https?://\S+)`)
)

// ParseShippingNotice extracts the tracking code from a shipping mail text.
// ok is false when the text carries no tracking code.
func ParseShippingNotice(text string) (n ShippingNotice, ok bool) {
	m := trackingCodeRe.FindStringSubmatch(text)
	if m == nil {
		return n, false
	}
	n.Code = strings.ToUpper(m[1])
	if c := carrierRe.FindStringSubmatch(text); c != nil {
		n.Carrier = normalizeCarrier(c[1])
	} else if isDHLCode(n.Code) {
		n.Carrier = "DHL"
	}
	if u := urlLineRe.FindStringSubmatch(text); u != nil {
		n.URL = strings.TrimRight(u[1], ".,;)>")
	}
	return n, true
}

func normalizeCarrier(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	switch strings.ToUpper(s) {
	case "DHL", "DPD", "GLS", "UPS":
		return strings.ToUpper(s)
	case "HERMES":
		return "Hermes"
	default:
		return "Deutsche Post"
	}
}

// isDHLCode recognizes the common DHL Paket piece codes: 12 or 20 digits
// (the latter starting with 00340), or JJD… numbers.
func isDHLCode(code string) bool {
	digits := strings.Trim(code, "0123456789") == ""
	return digits && (len(code) == 12 || len(code) == 20 && strings.HasPrefix(code, "00340")) ||
		strings.HasPrefix(code, "JJD")
}

// ReturnSender is the address DHL sends its Packstation drop-off receipts
// ("Einlieferungsbeleg: Ihr Versand an der Packstation") from.
const ReturnSender = "packstation@dhl.de"

// Defaults for the address of Bauduu's returns.
const (
	ReturnRecipient = "Bauduu"
	ReturnZip       = "44579"
)

var (
	// "Sendungsnummer(n):", "Sendungsnummer:", "Sendungsnummern"
	returnLabelRe = regexp.MustCompile(`(?i)sendungsnummer(?:\(n\)|n)?`)
	// Piece codes: 20 digits starting with 00340, 12 digits, or JJD….
	pieceCodeRe = regexp.MustCompile(`(?i)\b(00340\d{15}|\d{12}|JJD[0-9A-Z]{10,30})\b`)
	// Where the list of codes ends in a drop-off receipt.
	returnEndRe = regexp.MustCompile(`(?i)abgabe(?:zeitpunkt|stelle)|viele grüße`)
)

// ParseReturnReceipt extracts the tracking codes from a drop-off receipt:
// the piece codes after "Sendungsnummer(n)", in order, without duplicates.
// It returns nil when the text names no Sendungsnummer.
func ParseReturnReceipt(text string) []string {
	loc := returnLabelRe.FindStringIndex(text)
	if loc == nil {
		return nil
	}
	rest := text[loc[1]:]
	if end := returnEndRe.FindStringIndex(rest); end != nil {
		rest = rest[:end[0]]
	}
	var codes []string
	for _, m := range pieceCodeRe.FindAllStringSubmatch(rest, -1) {
		code := strings.ToUpper(m[1])
		if !slices.Contains(codes, code) {
			codes = append(codes, code)
		}
	}
	return codes
}
