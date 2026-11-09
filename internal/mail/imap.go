package mail

import (
	"bytes"
	"context"
	"fmt"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func imapConnect(ctx context.Context, cfg Config) (*imapclient.Client, func(), error) {
	conn, stop, err := dial(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	opts := &imapclient.Options{TLSConfig: cfg.tlsConfig()}
	var c *imapclient.Client
	if cfg.Security == SecurityStartTLS {
		c, err = imapclient.NewStartTLS(conn, opts)
		if err != nil {
			stop()
			conn.Close()
			return nil, nil, fmt.Errorf("IMAP STARTTLS: %w", err)
		}
	} else {
		c = imapclient.New(conn, opts)
	}
	cleanup := func() {
		_ = c.Logout().Wait()
		c.Close()
		stop()
	}
	if err := c.WaitGreeting(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("IMAP greeting: %w", err)
	}
	if err := c.Login(cfg.User, cfg.Password).Wait(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("IMAP login as %s: %w", cfg.User, err)
	}
	return c, cleanup, nil
}

func checkIMAP(ctx context.Context, cfg Config) (int, error) {
	c, cleanup, err := imapConnect(ctx, cfg)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	sel, err := c.Select(cfg.Mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return 0, fmt.Errorf("IMAP select %s: %w", cfg.Mailbox, err)
	}
	return int(sel.NumMessages), nil
}

func fetchIMAP(ctx context.Context, cfg Config, f Filter) ([]Message, error) {
	c, cleanup, err := imapConnect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	// EXAMINE: nothing in the mailbox changes, flags included.
	sel, err := c.Select(cfg.Mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return nil, fmt.Errorf("IMAP select %s: %w", cfg.Mailbox, err)
	}
	criteria := &imap.SearchCriteria{}
	if !f.Since.IsZero() {
		criteria.Since = f.Since
	}
	if f.From != "" {
		criteria.Header = []imap.SearchCriteriaHeaderField{{Key: "From", Value: f.From}}
	}
	data, err := c.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("IMAP search: %w", err)
	}
	var uids []imap.UID
	for _, uid := range data.AllUIDs() {
		if !f.skip(imapID(sel.UIDValidity, uid)) {
			uids = append(uids, uid)
		}
	}
	if len(uids) == 0 {
		return nil, nil
	}
	section := &imap.FetchItemBodySection{Peek: true}
	bufs, err := c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{section},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("IMAP fetch: %w", err)
	}
	var msgs []Message
	for _, b := range bufs {
		raw := b.FindBodySection(section)
		if raw == nil {
			continue
		}
		m, err := parse(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("IMAP message %d: %w", b.UID, err)
		}
		m.ID = imapID(sel.UIDValidity, b.UID)
		// The server's search is a substring match; apply the exact one.
		if f.matchFrom(m.From) && f.matchDate(m.Date) {
			msgs = append(msgs, m.Message)
		}
	}
	return msgs, nil
}

func imapID(validity uint32, uid imap.UID) string {
	return fmt.Sprintf("%d/%d", validity, uid)
}
