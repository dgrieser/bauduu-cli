package mail

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"mime"
	netmail "net/mail"
	"regexp"
	"sort"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // ISO-8859-x, Windows-125x, …
	gomail "github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
	"golang.org/x/net/html"
)

// parsed adds fields only needed internally.
type parsed struct {
	Message
	messageID string
}

// maxBody bounds what is read of one message; shipping mails are small.
const maxBody = 4 << 20

// Parse reads an RFC 5322 message and extracts sender, date, subject and the
// text: the text/plain part, or the text of the HTML part if there is none.
func Parse(r io.Reader) (*Message, error) {
	p, err := parse(r)
	if p == nil {
		return nil, err
	}
	return &p.Message, err
}

func parse(r io.Reader) (*parsed, error) {
	mr, err := gomail.CreateReader(io.LimitReader(r, maxBody))
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		return nil, err
	}
	if mr == nil {
		return nil, errors.New("unreadable message")
	}
	defer mr.Close()
	p := headerInfo(mr.Header)
	var plain, htm string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if message.IsUnknownCharset(err) || message.IsUnknownEncoding(err) {
				continue
			}
			// Headers are usable even when a body part is broken.
			if plain == "" && htm == "" {
				return p, fmt.Errorf("read body: %w", err)
			}
			break
		}
		ih, ok := part.Header.(*gomail.InlineHeader)
		if !ok {
			continue
		}
		ct, _, _ := ih.ContentType()
		if ct == "" {
			ct = "text/plain"
		}
		b, err := io.ReadAll(part.Body)
		if err != nil && len(b) == 0 {
			continue
		}
		switch ct {
		case "text/plain":
			if plain == "" {
				plain = string(b)
			}
		case "text/html":
			if htm == "" {
				htm = string(b)
			}
		}
	}
	switch {
	case strings.TrimSpace(plain) != "":
		p.Text = normalizeText(plain)
	case htm != "":
		p.Text = normalizeText(HTMLText(htm))
	}
	return p, nil
}

// parseHeader reads only the header block, e.g. the answer to POP3 TOP n 0,
// where a multipart body is missing and a full parse would fail.
func parseHeader(r io.Reader) (*parsed, error) {
	h, err := textproto.ReadHeader(bufio.NewReader(r))
	if err != nil {
		return nil, err
	}
	return headerInfo(gomail.Header{Header: message.Header{Header: h}}), nil
}

func headerInfo(h gomail.Header) *parsed {
	p := &parsed{}
	p.Date, _ = h.Date()
	if p.Date.IsZero() {
		// Some servers only keep the Received date.
		if v := h.Get("Received"); v != "" {
			if i := strings.LastIndex(v, ";"); i >= 0 {
				p.Date, _ = netmail.ParseDate(strings.TrimSpace(v[i+1:]))
			}
		}
	}
	if from, err := h.AddressList("From"); err == nil && len(from) > 0 {
		p.From = strings.ToLower(from[0].Address)
	} else {
		p.From = strings.ToLower(extractAddress(h.Get("From")))
	}
	if s, err := h.Subject(); err == nil {
		p.Subject = s
	} else {
		p.Subject = h.Get("Subject")
	}
	p.messageID = strings.Trim(h.Get("Message-Id"), "<> ")
	return p
}

var addrRe = regexp.MustCompile(`[^\s<>"]+@[^\s<>"]+`)

func extractAddress(s string) string {
	if dec, err := new(mime.WordDecoder).DecodeHeader(s); err == nil {
		s = dec
	}
	return addrRe.FindString(s)
}

var blockTags = map[string]bool{
	"p": true, "div": true, "tr": true, "li": true, "table": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"blockquote": true, "pre": true, "section": true, "article": true,
}

// HTMLText converts an HTML mail body to plain text with line breaks for
// block elements, so "label: value" lines stay intact.
func HTMLText(s string) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(s))
	if err != nil {
		return s
	}
	doc.Find("script, style, head").Remove()
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			b.WriteString(n.Data)
		case html.ElementNode:
			if n.Data == "a" {
				// Keep link targets: they often carry the tracking URL.
				for _, a := range n.Attr {
					if a.Key == "href" && strings.HasPrefix(a.Val, "http") {
						defer func(href string) {
							if !strings.Contains(textOf(n), href) {
								b.WriteString(" <" + href + ">")
							}
						}(a.Val)
					}
				}
			}
			if n.Data == "br" {
				b.WriteString("\n")
			}
			if n.Data == "td" || n.Data == "th" {
				defer b.WriteString(" ")
			}
			if blockTags[n.Data] {
				b.WriteString("\n")
				defer b.WriteString("\n")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	for _, n := range doc.Nodes {
		walk(n)
	}
	return b.String()
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(n)
	return b.String()
}

// normalizeText unifies line endings, non-breaking spaces and runs of blank
// lines.
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, " ", " ")
	lines := strings.Split(s, "\n")
	out := lines[:0]
	blank := 0
	for _, l := range lines {
		l = strings.TrimRight(strings.Join(strings.Fields(l), " "), " ")
		if l == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func sortByDate(msgs []Message) {
	sort.SliceStable(msgs, func(i, j int) bool {
		return msgs[i].Date.Before(msgs[j].Date)
	})
}
