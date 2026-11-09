# bauduu-cli

Command line client for the [bauduu.de](https://www.bauduu.de/lego/) LEGO rental
customer area. It logs in and prints the account sections as tables (or JSON):

| Command        | Site section                       | Content                                                         |
|----------------|------------------------------------|-----------------------------------------------------------------|
| `ausleihliste` | Deine Ausleihliste                 | Queue of sets to ship next: set no., theme, package, pieces, … |
| `sets`         | Bisherige Sets                     | Full loan history with shipping/return dates and bonus bricks   |
| `retouren`     | Retoure / Bisherige Sets           | Returned sets, loan duration, set currently at home, Kundennr.  |
| `buchungen`    | Deine Buchung                      | Bookings/subscriptions with price and period                    |
| `daten`        | Deine Daten                        | Account, customer, delivery and SEPA data (IBAN masked)         |
| `all` / none   | everything                         | All of the above                                                |
| `track`        | DHL Sendungsverfolgung             | Parcel status and history of the sets at home                   |
| `mail`         | your mailbox                       | Bauduu's shipping mails and their tracking codes                |
| `setup`        | –                                  | Configure mailbox (`setup mail`) and DHL API key (`setup dhl`)  |
| `watch`        | Bisherige Sets + DHL               | Detect changes since the last run and notify (for cron)         |

German aliases and English aliases work too (`wishlist`, `history`, `returns`,
`subscriptions`, `profile`, …).

The tool only reads. It never submits the Retoure, Pause or profile forms,
never marks mails as read and never deletes them.

## Install

Download a release archive (linux amd64/arm64/armv7, darwin arm64), or build:

```sh
make build        # ./bin/bauduu
make install      # /usr/local/bin/bauduu
make cross        # static binaries for all release platforms in ./bin
make snapshot     # same via goreleaser
```

## Credentials

Resolved in this order:

1. `--user` flag
2. `BAUDUU_USER` / `BAUDUU_PASSWORD` environment variables
3. config file (`~/.config/bauduu/config` on Linux,
   `~/Library/Application Support/bauduu/config` on macOS), written by
   `bauduu login --save` with mode 0600:

   ```
   user = "myname"
   password = "secret"
   ```

   Quoted values use Go string syntax and keep spaces at the edges; unquoted
   values are trimmed.

4. interactive prompt when stdin is a terminal

The stored password is used only for the stored user.

The same file holds the mailbox and DHL settings written by `bauduu setup`
(see below). Every `setup`/`login --save` only replaces its own keys and keeps
the other lines.

## Usage

```sh
bauduu                       # every section
bauduu ausleihliste          # queue incl. product details (one request per set)
bauduu ausleihliste --no-details
bauduu retouren
bauduu daten --reveal        # show the IBAN unmasked
bauduu -o json | jq '.loans[] | select(.status == "Ausgeliehen")'
```

Flags: `-o table|json`, `--no-details`, `--reveal`, `--no-color` (or `NO_COLOR`),
`--timeout`, `--config`, `-u/--user`.

Example:

```
Retouren (6)
┌───┬───────┬────────────────────────────────────┬────────────┬────────────┬──────┬───────┐
│ # │ Set   │ Name                               │ Versendet  │ Retour     │ Tage │ Bonus │
├───┼───────┼────────────────────────────────────┼────────────┼────────────┼──────┼───────┤
│ 6 │ 60036 │ Arktis Basislager                  │ 2026-04-28 │ 2026-06-09 │   42 │     3 │
│ 5 │ 60228 │ Weltraumrakete mit Kontrollzentrum │ 2026-03-16 │ 2026-04-27 │   42 │     3 │
└───┴───────┴────────────────────────────────────┴────────────┴────────────┴──────┴───────┘
2 Retouren · Ø 42 Tage ausgeliehen · 6 Bonussteine
Aktuell zu Hause: Riesenrad 3in1 (31119), versendet 2026-10-14, seit 6 Tagen
```

## Shipping mails and DHL tracking

When Bauduu ships a set, it mails the DHL tracking code from
`bauduu@brickcoast.de` ("Sie können den Sendungsverlauf verfolgen via DHL:
Trackingcode: 00340434698381831612"). The site itself only shows the set as
"Ausgeliehen" until the set is back, so bauduu-cli reads that mail and
follows the parcel on dhl.de.

1. Mailbox access (IMAP or POP3, read-only: IMAP mails stay unread, POP3
   mails are never deleted). `setup mail` asks for server, port, encryption,
   login and folder, tests the login, counts the shipping mails and saves:

   ```sh
   bauduu setup mail
   bauduu setup mail --protocol imap --host imap.example.com --mail-user me@example.com
   bauduu setup mail --protocol pop3 --host pop.example.com --security starttls --mail-user me
   ```

   Flags preset the answers (`--protocol imap|pop3`, `--host`, `--port`,
   `--security tls|starttls|none`, `--mail-user`, `--mailbox`, `--from`,
   `--no-test`). The password is prompted (hidden) or taken from
   `BAUDUU_MAIL_PASSWORD`, which also overrides the stored one at runtime.

2. DHL tracking works without setup: dhl.de's public tracking gives the
   status, progress and history, product, destination country and who took
   the parcel. The places of the events, the sender and the address need
   the recipient's postal code, and dhl.de only answers that query for
   browsers. DHL's official API gives them: register for free at
   [developer.dhl.com](https://developer.dhl.com), create an app with the API
   **Shipment Tracking - Unified** (not "Unified Push") and store its key
   together with the postal code (Postleitzahl):

   ```sh
   bauduu setup dhl                                    # asks for key and postal code
   bauduu setup dhl --api-key KEY --zip 44579 --test 00340434698381831612
   bauduu setup dhl --api-key ""                       # remove the key
   ```

   setup checks the key before saving it. With a key, `track` and `watch`
   ask the API and fall back to dhl.de when it fails (with a warning, unless
   the API just does not know the code). The free plan allows 250 queries a
   day and one every five seconds; bauduu-cli waits between queries.
   `BAUDUU_DHL_API_KEY`, `BAUDUU_DHL_ZIP` and `--zip` on `track`/`watch`
   override the stored values.

`bauduu setup` without a subcommand does both in a row, the mailbox first.
An empty mail server skips the mailbox, no API key and postal code skip DHL;
the flags of `setup mail` and `setup dhl` (`--api-key`, `--zip`) preset the
answers:

```sh
bauduu setup
bauduu setup --host imap.example.com --mail-user me@example.com --api-key KEY --zip 44579
```

Then:

```sh
bauduu mail                          # shipping mails of the last 90 days
bauduu track                         # sets at home: tracking code from the mail, DHL status
bauduu track 00340434698381831612    # any DHL tracking code
bauduu track -o json
```

With an API key:

```
Riesenrad 3in1 (31119) · DHL 00340434698381831612
┌──────────────────┬───────────┬─────────────────────────────────────────────────────┐
│ Zeit             │ Ort       │ Status                                              │
├──────────────────┼───────────┼─────────────────────────────────────────────────────┤
│ 2026-11-06 16:41 │           │ Die Sendung wurde zugestellt.                       │
│ 2026-11-06 00:32 │ Bielefeld │ Die Sendung ist in der Region des Empfängers        │
│                  │           │ angekommen.                                         │
│ 2026-11-05 16:31 │ Bochum    │ Die Sendung wurde von DHL bearbeitet.               │
└──────────────────┴───────────┴─────────────────────────────────────────────────────┘
Die Sendung wurde zugestellt. (2026-11-06 16:41)
Produkt: DHL Paket
Absender: Bauduu & BrickCoast Steinebande gGmbH
Adressiert an: Erika Mustermann, 44579 Castrop-Rauxel
Zugestellt an: Empfänger:in
https://www.dhl.de/de/privatkunden/pakete-empfangen/verfolgen.html?piececode=00340434698381831612
```

A mail is linked to the set whose shipping date ("Versendet") is closest to
the mail date, at most 5 days apart. Config keys: `mail_protocol`,
`mail_host`, `mail_port`, `mail_security`, `mail_user`, `mail_password`,
`mail_mailbox`, `mail_from`, `dhl_api_key` and `dhl_zip`.

Without an API key the DHL data comes from the JSON endpoints behind dhl.de's
tracking page (`/int-verfolgen/data/…`), which are undocumented and protected
against bots; DHL may refuse some networks (e.g. data centers) or rate-limit
queries.

## Watch for changes (cron)

`bauduu watch` compares Bisherige Sets with the previous run and reports
newly shipped sets, booked returns, status and bonus brick changes. The first
run stores a baseline. Without changes it prints nothing, so cron stays quiet.

With a mailbox configured, watch also follows the parcels: while a set is
"Ausgeliehen" without a known tracking code, it searches the mailbox for
Bauduu's shipping mail and notifies as soon as the code is known
(`Bauduu: Trackingnummer für Riesenrad 3in1`). From then on every run queries
DHL and reports each new status or history event
(`Bauduu: Sendung Riesenrad 3in1: Die Sendung wurde im Start-Paketzentrum
bearbeitet.`) until the parcel is delivered (`Bauduu: Zugestellt: Riesenrad
3in1`). The mailbox is not touched while every set at home has its code, and
DHL is not queried after delivery (unless the last answer came from dhl.de
because the API failed). `--no-mail`, `--no-tracking` and `--zip` adjust
this. Mailbox or DHL failures, and broken mail or DHL settings, are printed
as warnings on stderr; the other checks still run and the failed one is
retried next time. Switching between the API and dhl.de reports only events
after the last known one, as both word the history differently.

`--notify` takes a `/bin/sh` command template. `{{title}}` and
`{{description}}` (alias `{{desc}}`) become quoted references to the
environment variables `BAUDUU_TITLE` and `BAUDUU_DESCRIPTION`, which carry
the values. Text from the site is therefore never parsed as shell syntax. The
quoting adapts to where the placeholder stands (bare, inside `"..."`, `'...'`,
`$(...)` or backticks), so titles and descriptions with quotes, `$`,
backticks or line breaks reach the command unchanged as one argument each.
`--dry-run` prints the equivalent standalone command:

```sh
$ bauduu watch --test --dry-run --notify 'notify -t {{title}} -d {{description}}'
BAUDUU_TITLE='Bauduu: Testbenachrichtigung' BAUDUU_DESCRIPTION='…' /bin/sh -c 'notify -t "$BAUDUU_TITLE" -d "$BAUDUU_DESCRIPTION"'
```

Send a test notification:

```sh
bauduu watch --test --notify 'notify -t {{title}} -d {{description}}'
```

Crontab entry (more variants in [`examples/crontab`](examples/crontab)):

```cron
*/30 * * * * /usr/local/bin/bauduu watch --notify '/usr/local/bin/notify -t {{title}} -d {{description}}'
```

Example notification:

```
Title:       Bauduu: Retour verbucht: Arktis Basislager
Description: Retour verbucht: Arktis Basislager (60036) am 2026-06-09 nach 42 Tagen; Bonussteine Arktis Basislager (60036): +3 (jetzt 3)
```

Several changes in one run are combined: the title then reads
`Bauduu: 3 Änderungen bei Bisherige Sets` (or `… Neuigkeiten zur Sendung`,
`… Änderungen` when mixed), with one description line each.

The state (sets, tracking codes, last DHL status) lives in
`$XDG_STATE_HOME/bauduu/sets-<user>-<hash>.json` (default
`~/.local/state/...`, override with `--state`); a state file of another
account is refused. It is saved only after the
notify command succeeded, so a failed notification is retried next run.
Remember that crontab treats `%` as a newline: write `\%`.

## Notes

- The site is Joomla (com_users login, VirtueMart wishlist, PayPlans bookings).
  Pages are scraped; markup changes on the site can break parsing. The test
  fixtures in `internal/bauduu/testdata` are real pages with personal data
  replaced, so `go test` shows quickly which parser broke.
- "W" rows in Bisherige Sets ("Nächster Versand ab …") are shipping
  placeholders, not sets. They show dimmed in `sets` and are excluded from
  `retouren` and all counts.
- The site does not track stock for rentals (every set reports OutOfStock), so
  availability is only in the JSON (`in_stock`), not in the tables.
