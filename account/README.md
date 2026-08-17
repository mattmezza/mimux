# account — the mimux licence shop

This is the service behind <https://account.mimux.dev>. It sells `pro/` licence
keys, emails them, and re-sends them when someone loses one. That is the whole
job.

It runs on one VPS, it is mine, and it is **never distributed**. It is not in
any release, any Docker image, or any binary you can download. It is licensed
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

If the email fails after the licence is stored, the failure is logged and the
customer can pull the key from `/retrieve`. Redelivering the event would not
help — the id is already claimed.

## Configuration

Everything is an environment variable. There is no config file.

| Variable | Required | Default | What |
|----------|----------|---------|------|
| `STRIPE_SECRET_KEY` | yes | — | `sk_live_...` |
| `STRIPE_WEBHOOK_SECRET` | yes | — | `whsec_...`, from the webhook endpoint you create below |
| `STRIPE_PRICE_ANNUAL` | yes | — | `price_...` for €49/year recurring |
| `STRIPE_PRICE_PERPETUAL` | yes | — | `price_...` for €99 one-time |
| `LICENCE_SIGNING_KEY_B64` | yes | — | base64 of the 64-byte ed25519 private key. The service refuses to start without it. |
| `BASE_URL` | yes | — | `https://account.mimux.dev` — used to build Stripe return URLs |
| `CURRENT_VERSION` | yes | — | e.g. `v0.19`; becomes the licence watermark |
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

1. **Products.** Create two, in live mode:
   - `mimux pro — annual`, recurring, €49, yearly → note the price id for
     `STRIPE_PRICE_ANNUAL`.
   - `mimux pro — perpetual`, one-time, €99 → note the price id for
     `STRIPE_PRICE_PERPETUAL`.
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
4. **Test mode first.** Do the whole thing with `sk_test_` keys, test price ids
   and a test-mode webhook secret, then repeat in live mode. `stripe listen
   --forward-to localhost:8080/stripe/webhook` gives you a local webhook secret
   for development.

## Deploying

TLS is terminated by the reverse proxy already on the box (Caddy/nginx); this
service speaks plain HTTP on loopback and handles no certificates.

```sh
# on the VPS, first time
git clone https://github.com/mattmezza/mimux.git && cd mimux/account
cp account.env.example account.env    # or write it by hand from the table above
chmod 600 account.env
docker compose up -d --build

# subsequently
git pull && docker compose up -d --build
docker compose logs -f account
```

Reverse proxy, in Caddy terms:

```
account.mimux.dev {
    reverse_proxy 127.0.0.1:8080
}
```

Caddy sets `X-Forwarded-For`, which the per-IP limiter trusts. That trust is why
compose binds the port to `127.0.0.1` — if the container port were reachable
from the internet, the header would be forgeable and the limit meaningless.

Bump `CURRENT_VERSION` in `account.env` and `docker compose up -d` on every
mimux release: it is the watermark stamped into new perpetual licences, so a
stale value under-sells them.

The SQLite file in the `account-data` volume is the only state. Back it up —
`docker compose exec account sh -c 'sqlite3 /data/account.db .dump'` or just
copy the volume while stopped. Losing it loses the ability to re-send keys that
customers have already been emailed.

## Known gaps

- **No admin UI.** Refunds, comps and corrections are `sqlite3` by hand.
