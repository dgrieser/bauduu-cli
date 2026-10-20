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

German aliases and English aliases work too (`wishlist`, `history`, `returns`,
`subscriptions`, `profile`, …).

The tool only reads. It never submits the Retoure, Pause or profile forms.

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
