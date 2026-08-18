# account — the mimux licence shop

This is the service behind <https://account.mimux.dev>. It sells `pro/` licence
keys, emails them, and re-sends them when someone loses one. That is the whole
job.

It runs on one VPS, it is mine, and it is **never distributed**. It is not in
any mimux release and not in any binary you can download. It is built into a
Docker image so the VPS has something to pull, but that image is **private** on
ghcr.io and exists only to move this service onto my own box. It is licensed
ELv2 (see [`LICENSE`](LICENSE)) like `pro/`, and it imports nothing from the
parent Go module — `account/` is its own module on purpose.

## Why it is in a public repo

Because "mimux never phones home" is a claim, and a claim you can read the code
for is worth more than one you can't.

A licence key is an ed25519 signature over a small JSON payload. `pro/` verifies
it against a public key compiled into the binary — locally, offline, with no
network call. This service is the only thing that ever holds the private half.
It has no endpoint that a mimux instance talks to, because mimux never talks to
it. There is no activation, no heartbeat, no usage report, no seat count.

The same is true of cancellation. When a subscription lapses, this service marks
its own row `lapsed` for bookkeeping and stops there. There is no revocation
channel — the key you were issued keeps verifying until it expires on its own
terms, and a perpetual key never expires at all. Building a kill switch would
require the phone-home this project exists without.

Card details never reach this service either. Checkout is hosted by Stripe; what
comes back is an email address, a customer id and a subscription id. The
database schema in `store.go` is the whole of what is stored.

## Routes

| Route | What it does |
|-------|--------------|
| `GET /` | The two plans and their Buy buttons. |
| `POST /checkout` | Creates a Stripe Checkout Session, redirects to Stripe. |
| `GET /success` | "Your key is on its way." |
| `GET /cancel` | "Nothing was charged." |
| `POST /stripe/webhook` | Signature-verified Stripe events; issues and lapses licences. |
| `GET /retrieve`, `POST /retrieve` | Re-send a key, or a billing-portal link, by email. |
| `GET /healthz` | `ok`, for the compose healthcheck. |

`/retrieve` never displays a key and never says whether an address is a
customer — the answer is the same either way. It is limited to 3 requests an
hour per IP and 3 per address, which is enough for someone who mistyped and
useless for walking a list of addresses.

Self-service cancellation and card updates go through the Stripe billing portal.
Since there is no login here, the portal link is emailed to the purchase address
under the same limits rather than opened directly.

## Webhook events

| Event | Effect |
|-------|--------|
| `checkout.session.completed` | Sign a licence, store it, email it. Idempotent on the Stripe event id — claiming the id and inserting the licence are one transaction, so a redelivery issues nothing. |
| `invoice.payment_succeeded` | Re-issue the subscription's annual key with the period just paid for, mark it active, email it. |
| `customer.subscription.deleted` | Mark the matching annual licence `lapsed`. |
| `invoice.payment_failed` | Same. |

Renewals keep the licence id and overwrite the key in place, so `/retrieve`
returns one current key per purchase rather than a pile of expired ones. The new
expiry comes from the invoice's line-item period (falling back to the invoice
period, then to a year out — a paid invoice must always buy time). Two cases
re-issue nothing: an invoice for a subscription that has no licence yet, and an
invoice whose period end is not later than what the current key already covers
(that one still clears a `lapsed` mark — the customer has paid). Between them
they cover the race with
`checkout.session.completed` on a new subscription without assuming which event
Stripe delivers first.

Email is never on the response path. The licence is committed, the send is
handed to a background goroutine, and the webhook answers Stripe immediately —
a submission host that stalls used to hold the request open until Stripe called
it a failed delivery, and the retry finds the event id already claimed and
sends nothing, so a slow SMTP server could lose a paying customer's key. The
same goes for `/retrieve`, where a person is waiting.

The sender bounds itself: one deadline over dial and the whole SMTP
conversation, three attempts with a widening gap between them, every failure
logged against the licence id. If all three fail, that is logged too and the
customer can pull the key from `/retrieve`. Redelivering the event would not
help — the id is already claimed. On `SIGTERM` the process stops accepting
requests and waits, briefly, for sends still in flight; anything still retrying
past that bound is abandoned to `/retrieve` rather than held on to.

## Pricing and currency

[`pricing.json`](pricing.json) is the whole price list — the figures are not
repeated in this README, because a table here would be one more thing to drift.
[`pricing.go`](pricing.go) embeds that file and parses it at startup; each
Checkout Session is built with `price_data`, so amounts live in that one file
rather than in Stripe Price objects that can drift from it.

The marketing site reads the same file: `www/scripts/build.sh` substitutes
`{{price:…}}` tokens in `www/src` at build time and fails the build on a token
it cannot resolve, so mimux.dev cannot quote a price checkout will not honour.
That is also why the file lives in `account/` rather than at the repo root —
this service is its own Go module, built with `context: account`, and `go:embed`
cannot reach outside its own directory. The www build runs from the repo root
and can reach in.

The buyer picks the currency on the page; the choice is a `?currency=` link, so
it needs no JavaScript and each currency has its own URL. The forms submit the
currency being displayed — including the buy forms on mimux.dev, which POST
straight here — and `/checkout` **rejects** anything not in the table rather
than defaulting to euros: a silent fallback would charge someone in a currency
they never saw.

Adding a currency is one entry in `currencies` and one per plan in `plans`. The
amounts are deliberately the same numerals in both: the aim is a round, familiar
price in each, not a tracked exchange rate.

## Configuration

Everything is an environment variable. There is no config file.

| Variable | Required | Default | What |
|----------|----------|---------|------|
| `STRIPE_SECRET_KEY` | yes | — | `sk_live_...` |
| `STRIPE_WEBHOOK_SECRET` | yes | — | `whsec_...`, from the webhook endpoint you create below |
| `LICENCE_SIGNING_KEY_B64` | yes | — | base64 of the 64-byte ed25519 private key. The service refuses to start without it. |
| `BASE_URL` | yes | — | `https://account.mimux.dev` — used to build Stripe return URLs |
| `CURRENT_VERSION` | yes | — | e.g. `v0.20`; becomes the licence watermark |
| `SMTP_HOST` | yes | — | submission host |
| `SMTP_FROM` | yes | — | envelope and header From |
| `SMTP_PORT` | no | `587` | STARTTLS submission port. Implicit TLS (465) is not supported. |
| `SMTP_USER` | no | — | omit to send unauthenticated |
| `SMTP_PASS` | no | — | |
| `DB_PATH` | no | `/data/account.db` | SQLite file |
| `LISTEN_ADDR` | no | `:8080` | |

## Generating the signing key

Once, ever:

```sh
go run . -genkey
```

It prints two lines:

```
LICENCE_SIGNING_KEY_B64=<private — VPS env only, never in git>
public key (pro/ licencePubKeyB64)=<public — safe to commit and ship>
```

The private half goes into `account.env` on the VPS and nowhere else. The public
half is the default value of `licencePubKeyB64` in `pro/`, or is injected at
build time:

```sh
go build -tags pro -ldflags "-X github.com/mattmezza/mimux/pro.licencePubKeyB64=<public>" ./cmd/mimux
```

Losing the private key means every future key must be signed with a new one, and
every already-issued key stops verifying against a rebuilt binary. Back it up
somewhere that is not this machine.

## Stripe dashboard setup

1. **Products: none to create.** Amounts are built into each Checkout Session
   with `price_data` (see [`pricing.json`](pricing.json)), so there is no
   Product or Price object in the dashboard and nothing to keep in sync.
   Changing a price is an edit to that file, a redeploy, and a www rebuild. Stripe records the amount on each
   subscription it creates, so existing annual subscribers keep renewing at the
   price they signed up at; a change applies to new checkouts only.
2. **Webhook endpoint.** Developers → Webhooks → Add endpoint,
   `https://account.mimux.dev/stripe/webhook`, subscribed to exactly:
   - `checkout.session.completed`
   - `invoice.payment_succeeded`
   - `customer.subscription.deleted`
   - `invoice.payment_failed`

   Copy the signing secret into `STRIPE_WEBHOOK_SECRET`.
3. **Customer portal.** Settings → Billing → Customer portal: turn it on, allow
   cancellation and payment-method updates. Without this, the "manage
   subscription" flow errors.
4. **Test mode first.** Do the whole thing with `sk_test_` keys and a test-mode
   webhook secret, then repeat in live mode. `stripe listen
   --forward-to localhost:8080/stripe/webhook` gives you a local webhook secret
   for development.

## Deploying

TLS is terminated by the reverse proxy already on the box (Caddy/nginx); this
service speaks plain HTTP on loopback and handles no certificates.

The VPS holds two files — `docker-compose.yml` and `account.env` — and no
checkout. The image is pulled from ghcr.io, where the Account workflow publishes
it on every `account-v*` release (`make release-account name=account-v0.1`).

```sh
# on the VPS, first time. The package is private, so authenticate once with a
# GitHub PAT that has read:packages — it is stored in ~/.docker/config.json.
echo "$GHCR_PAT" | docker login ghcr.io -u mattmezza --password-stdin

mkdir -p ~/apps/account.mimux.dev && cd ~/apps/account.mimux.dev
# copy docker-compose.yml and account.env here by hand
chmod 600 account.env
docker compose up -d

# subsequently, after an account-v* release has published a new :latest
docker compose pull && docker compose up -d
docker compose logs -f account
```

Reverse proxy: [`nginx/account.mimux.dev.conf`](nginx/account.mimux.dev.conf) is
ready to copy into `sites-available`, with the certbot invocation in its header
comment. In Caddy terms it is:

```
account.mimux.dev {
    reverse_proxy 127.0.0.1:8080
}
```

Both proxies **append** to `X-Forwarded-For`, so the last value is the hop the
proxy itself added and the only one a caller cannot forge; `clientIP` reads that
end deliberately. Compose also binds the port to `127.0.0.1` so the container is
not reachable directly. Both halves matter: the bind stops someone skipping the
proxy, and reading the last hop stops someone who goes *through* it from setting
their own header and rotating past the per-IP limit.

Bump `CURRENT_VERSION` in `account.env` and `docker compose up -d` on every
mimux release: it is the watermark stamped into new perpetual licences, so a
stale value under-sells them.

The SQLite file in the `account-data` volume is the only state. Back it up —
`docker compose exec account sh -c 'sqlite3 /data/account.db .dump'` or just
copy the volume while stopped. Losing it loses the ability to re-send keys that
customers have already been emailed.

## Known gaps

- **No admin UI.** Refunds, comps and corrections are `sqlite3` by hand.
