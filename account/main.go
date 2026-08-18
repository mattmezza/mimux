// SPDX-License-Identifier: LicenseRef-Elastic-2.0

// Command account is the mimux licence shop. It runs on one VPS behind a
// reverse proxy at account.mimux.dev and nowhere else — it is never shipped to
// users and mimux itself never talks to it. It lives in the public repo so that
// claim can be checked rather than believed: licence verification in pro/ is
// pure ed25519 against an embedded public key, offline, with no callback here.
package main

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"embed"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	stripe "github.com/stripe/stripe-go/v86"
)

//go:embed templates
var templatesFS embed.FS

// The house fonts, embedded so this service serves its own and does not fetch
// anything from mimux.dev at runtime — a checkout page should not depend on a
// second origin being up. Copies of www/src/fonts; OFL licences ship beside
// them because the binary redistributes the fonts.
//
//go:embed static
var staticFS embed.FS

// fontsFS is staticFS rooted at static/, so request paths map straight onto it.
var fontsFS = func() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // embedded at build time: unreachable unless the embed changes
	}
	return sub
}()

type config struct {
	listenAddr     string
	baseURL        string
	currentVersion string
	dbPath         string

	stripeSecret   string
	webhookSecret  string

	smtpHost string
	smtpPort string
	smtpUser string
	smtpPass string
	smtpFrom string
}

type app struct {
	cfg  config
	db   *sql.DB
	key  ed25519.PrivateKey
	tmpl map[string]*template.Template
	send mailer
	// mail counts the sends still in flight, so shutdown can wait for them and
	// tests can assert on delivery without racing the goroutine.
	mail sync.WaitGroup

	ipLimit    *limiter
	emailLimit *limiter
}

func main() {
	gen := flag.Bool("genkey", false, "generate an ed25519 licence signing keypair and exit")
	flag.Parse()
	if *gen {
		genkey()
		return
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	key, err := parseSigningKey(os.Getenv("LICENCE_SIGNING_KEY_B64"))
	if err != nil {
		return err
	}
	db, err := openDB(cfg.dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	stripe.Key = cfg.stripeSecret

	a := &app{
		cfg:  cfg,
		db:   db,
		key:  key,
		send: smtpSender(cfg),
		// 3 an hour each: enough for a customer who mistyped, useless for
		// walking a list of addresses to see which ones bought.
		ipLimit:    newLimiter(3, time.Hour),
		emailLimit: newLimiter(3, time.Hour),
	}
	if err := a.parseTemplates(); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           a.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	// SIGTERM is how the container stops. Without catching it the process dies
	// mid-send and a paid-for licence key never leaves the building.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	slog.Info("account listening", "addr", cfg.listenAddr, "base", cfg.baseURL, "version", cfg.currentVersion)
	err = srv.ListenAndServe()
	// Runs while Shutdown is still draining connections, so the two bounds
	// overlap rather than add up.
	a.drainMail(shutdownGrace)
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// shutdownGrace stays under Docker's default ten seconds between SIGTERM and
// SIGKILL: a drain longer than that is not a drain, it is a process being shot
// mid-sentence.
const shutdownGrace = 8 * time.Second

// drainMail waits for in-flight sends, but not for ever: a send that is still
// retrying past the bound is abandoned and the customer uses /retrieve.
// ponytail: no persistent queue — mail is only ever a resend away.
func (a *app) drainMail(limit time.Duration) {
	done := make(chan struct{})
	go func() { a.mail.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(limit):
		slog.Warn("exiting with email still in flight — those customers can use /retrieve")
	}
}

func loadConfig() (config, error) {
	c := config{
		listenAddr:     env("LISTEN_ADDR", ":8080"),
		baseURL:        strings.TrimRight(os.Getenv("BASE_URL"), "/"),
		currentVersion: os.Getenv("CURRENT_VERSION"),
		dbPath:         env("DB_PATH", "/data/account.db"),
		stripeSecret:   os.Getenv("STRIPE_SECRET_KEY"),
		webhookSecret:  os.Getenv("STRIPE_WEBHOOK_SECRET"),
		smtpHost:       os.Getenv("SMTP_HOST"),
		smtpPort:       env("SMTP_PORT", "587"),
		smtpUser:       os.Getenv("SMTP_USER"),
		smtpPass:       os.Getenv("SMTP_PASS"),
		smtpFrom:       os.Getenv("SMTP_FROM"),
	}
	required := map[string]string{
		"BASE_URL":                c.baseURL,
		"CURRENT_VERSION":         c.currentVersion,
		"STRIPE_SECRET_KEY":       c.stripeSecret,
		"STRIPE_WEBHOOK_SECRET":   c.webhookSecret,
		"LICENCE_SIGNING_KEY_B64": os.Getenv("LICENCE_SIGNING_KEY_B64"),
		"SMTP_HOST":               c.smtpHost,
		"SMTP_FROM":               c.smtpFrom,
	}
	var missing []string
	for k, v := range required {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("missing required env: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.page("index"))
	mux.HandleFunc("POST /checkout", a.handleCheckout)
	mux.HandleFunc("GET /success", a.page("success"))
	mux.HandleFunc("GET /cancel", a.page("cancel"))
	mux.HandleFunc("GET /retrieve", a.page("retrieve"))
	mux.HandleFunc("POST /retrieve", a.handleRetrieve)
	// Linked from emails, the marketing site and GitHub: keep this path stable.
	mux.HandleFunc("GET /support", a.page("support"))
	mux.HandleFunc("POST /stripe/webhook", a.handleWebhook)
	// The house fonts, served from the embedded FS. No StripPrefix: the sub-FS
	// is rooted at static/, so the request path /fonts/x.woff2 already names
	// fonts/x.woff2 inside it.
	mux.Handle("GET /fonts/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Immutable in practice — the filenames change when the fonts do.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.FileServerFS(fontsFS).ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// handleRetrieve resends licence keys, or a billing-portal link, to the address
// that bought them. It answers identically whether or not that address has ever
// bought anything — the page is not an oracle for who is a customer — and it
// never puts a key on screen, because the browser is not where the proof of
// purchase lives.
func (a *app) handleRetrieve(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.PostFormValue("email"))
	portal := r.PostFormValue("action") == "portal"
	now := time.Now()

	if validEmail(email) &&
		a.ipLimit.allow(clientIP(r), now) &&
		a.emailLimit.allow(strings.ToLower(email), now) {
		if err := a.resend(email, portal); err != nil {
			// Logged, never surfaced: the reply must not vary.
			slog.Error("retrieve", "portal", portal, "err", err)
		}
	}
	a.render(w, "retrieve", map[string]any{"Sent": true, "Portal": portal})
}

func (a *app) resend(email string, portal bool) error {
	lics, err := licencesByEmail(a.db, email)
	if err != nil || len(lics) == 0 {
		return err
	}
	if portal {
		for _, l := range lics {
			if l.CustomerID == "" {
				continue
			}
			url, err := a.portalURL(l.CustomerID)
			if err != nil {
				return err
			}
			a.dispatch("retrieve", email, "Manage your mimux pro subscription", portalEmail(url))
			return nil
		}
		return nil
	}
	var b strings.Builder
	b.WriteString("Here are the mimux pro licence keys issued to this address.\n")
	for _, l := range lics {
		fmt.Fprintf(&b, "\n%s (%s, issued %s)\n\n    %s\n",
			l.ID, l.Plan, time.Unix(l.CreatedAt, 0).UTC().Format("2006-01-02"), l.Key)
	}
	b.WriteString("\n" + emailFooter)
	a.dispatch("retrieve", email, "Your mimux pro licence key", b.String())
	return nil
}

// clientIP is the peer as reported by the reverse proxy in front of this
// service. It takes the LAST value in X-Forwarded-For, not the first: proxies
// append (nginx's $proxy_add_x_forwarded_for, Caddy's default), so the final
// entry is the hop our own proxy added and the only one a caller cannot forge.
// Anything before it arrived from the internet.
//
// Reading the first value instead would hand the per-IP limit to the attacker:
// /retrieve is limited to make licence-holder enumeration and mail-cannon abuse
// useless, and a client that sets its own X-Forwarded-For could rotate straight
// past it. The loopback bind in docker-compose.yml stops the port being reached
// directly, but the header still arrives through the proxy either way.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

const emailFooter = `Paste the key into mimux under Settings, or set MIMUX_LICENCE_KEY.

Verification is entirely offline: mimux checks the signature against a public
key compiled into the binary and never contacts us. Nothing about your usage
is reported anywhere.

Lost it again? https://account.mimux.dev/retrieve
Questions? support@mimux.dev, or https://account.mimux.dev/support
`

func licenceEmail(p licencePayload, key string) string {
	var b strings.Builder
	b.WriteString("Thanks for buying mimux pro.\n\nYour licence key:\n\n    " + key + "\n\n")
	switch p.Plan {
	case planAnnual:
		fmt.Fprintf(&b, "Plan: annual — valid until %s, and it renews automatically.\n",
			time.Unix(*p.ExpiresAt, 0).UTC().Format("2 January 2006"))
	default:
		fmt.Fprintf(&b, "Plan: perpetual — never expires, and entitles you to every\nrelease up to and including %s.\n", p.Watermark)
	}
	b.WriteString("\n" + emailFooter)
	return b.String()
}

func renewalEmail(p licencePayload, key string) string {
	return "Your mimux pro subscription renewed, so here is a fresh licence key:\n\n    " + key +
		"\n\nIt replaces the previous one — swap it in under Settings, or in\n" +
		"MIMUX_LICENCE_KEY. The old key keeps working until it expires, so there is\n" +
		"no rush and no downtime.\n\n" +
		fmt.Sprintf("Valid until %s.\n\n", time.Unix(*p.ExpiresAt, 0).UTC().Format("2 January 2006")) +
		emailFooter
}

func portalEmail(url string) string {
	return "Manage your mimux pro subscription — update the card on file or\n" +
		"cancel — here:\n\n    " + url + "\n\n" +
		"The link is single-use and expires shortly; request another from\n" +
		"https://account.mimux.dev/retrieve if it has gone stale.\n\n" +
		"Cancelling stops the renewal. Your key keeps working until the term\n" +
		"you already paid for runs out — there is no remote kill switch.\n\n" +
		"Questions? support@mimux.dev, or https://account.mimux.dev/support\n"
}

func (a *app) parseTemplates() error {
	a.tmpl = map[string]*template.Template{}
	for _, name := range []string{"index", "success", "cancel", "retrieve", "support"} {
		t, err := template.ParseFS(templatesFS, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		a.tmpl[name] = t
	}
	return nil
}

func (a *app) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { a.render(w, name, a.pageData(r)) }
}

// pageData is the currency the page is priced in, plus the rendered amounts.
// The choice rides in ?currency=, so switching it is a plain link and the page
// stays cacheable and JavaScript-free. An unknown or absent value falls back to
// the first listed currency — unlike checkout, showing euros to someone who
// asked for something we do not sell costs nothing and charges nobody.
func (a *app) pageData(r *http.Request) map[string]any {
	cur := currencies[0].Code
	if q := r.URL.Query().Get("currency"); validCurrency(q) {
		cur = q
	}
	amounts, err := priceList(cur)
	if err != nil {
		// Unreachable while pricing.go lists every plan in every currency; a
		// blank price is still better than a 500 on the page that sells things.
		slog.Error("price list", "currency", cur, "err", err)
		amounts = map[string]string{}
	}
	return map[string]any{
		"Currency":   cur,
		"Currencies": currencies,
		"Prices":     amounts,
	}
}

func (a *app) render(w http.ResponseWriter, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl[name].ExecuteTemplate(w, "layout", data); err != nil {
		slog.Error("render", "page", name, "err", err)
	}
}
