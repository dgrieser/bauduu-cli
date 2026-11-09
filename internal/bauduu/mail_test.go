package bauduu

import "testing"

const shippingText = `Guten Tag Erika Mustermann,

ein neues LEGO Set wurde von uns versendet und an den Versanddienstleister DHL übergeben. Ihr neues LEGO Set sollte Sie in den kommenden Tagen erreichen.

Sie können den Sendungsverlauf verfolgen via DHL:
Trackingcode: 00340434698381831612
URL: https://www.dhl.de/trackandtrace

Wir wünschen Ihnen viel Spass mit dem neuen Set.

E-Mail: support@bauduu.de
Tel: +49 2305 5808100`

func TestParseShippingNotice(t *testing.T) {
	n, ok := ParseShippingNotice(shippingText)
	if !ok || n.Code != "00340434698381831612" || n.Carrier != "DHL" || n.URL != "https://www.dhl.de/trackandtrace" {
		t.Errorf("notice = %+v, %v", n, ok)
	}
	cases := []struct {
		text, code, carrier string
	}{
		{"Tracking-Nummer: jjd0001234567890", "JJD0001234567890", "DHL"},
		{"Ihre Sendungsnummer 123456789012 bei DHL", "123456789012", "DHL"},
		{"Versand mit Hermes. Trackingcode: H1001234567890123456", "H1001234567890123456", "Hermes"},
		{"Trackingcode:\n00340434698381831612", "00340434698381831612", "DHL"},
	}
	for _, c := range cases {
		n, ok := ParseShippingNotice(c.text)
		if !ok || n.Code != c.code || n.Carrier != c.carrier {
			t.Errorf("%q = %+v, %v", c.text, n, ok)
		}
	}
	for _, text := range []string{"", "Ihre Rücksendung ist eingegangen.", "Tel: +49 2305 5808100"} {
		if n, ok := ParseShippingNotice(text); ok {
			t.Errorf("%q: unexpected notice %+v", text, n)
		}
	}
}
