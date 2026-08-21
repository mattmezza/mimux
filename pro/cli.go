//go:build pro

// SPDX-License-Identifier: LicenseRef-Elastic-2.0

package pro

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mattmezza/mimux/internal/mail"
)

// `mimux mail <verb>` — the terminal client, for shells, scripts and agents
// that have a bash harness but no MCP client.
//
// Like the stdio bridge next door (mcp_bridge.go) it is a thin HTTP client of a
// *running* mimux: it never opens the database — that process owns it and is
// writing to it — and it authenticates with the same scoped tokens from
// Settings → API, so there is no second authorisation surface and no way to
// reach the pro layer without a licence.
//
// The verb is `mail`. Not `cli`, because `mimux cli search` names the plumbing
// rather than the thing being searched; and not a flat `mimux search`, because
// flat verbs share one namespace with `mcp` and `licence` and every future
// addition then risks a collision. It is a one-way door for anyone scripting
// against it, so: one noun, one namespace, and it reads like what it does.
//
// The commands mirror the MCP tools one for one — same nouns, same scopes, same
// semantics — because they are the same product wearing a different skin.
//
// `mimux mail login <url>` is the way in: it opens the browser, you approve the
// scopes in a session you already have, and the token lands in
// os.UserConfigDir()/mimux/credentials.json (0600) keyed by instance. Nothing
// is ever typed or pasted. The environment still works for CI and containers:
//
//	MIMUX_URL        base URL of the running mimux (default http://localhost:8083)
//	MIMUX_TOKEN      an API token from Settings → API (MIMUX_API_TOKEN also read)
//
// --url and --token override both. Where a command talks, in order: --url,
// MIMUX_URL, the `use` default, the only instance logged in, localhost:8083.
// Which token: --token, the environment, the stored entry for that instance.

const cliDefaultURL = "http://localhost:8083"

// cliClient is one invocation: where to talk, as whom, and how to print.
type cliClient struct {
	base  string
	token string
	json  bool
	scope string // the scope this verb needs, for a 403 with no envelope
	out   io.Writer
	errw  io.Writer
	in    io.Reader
	http  *http.Client

	credsDone bool // the stored credentials have been consulted for this invocation
}

type cliCommand struct {
	name  string
	args  string
	scope string
	help  string
	run   func(*cliClient, []string) error
}

// usageError exits 2 rather than 1: wrong invocation, not a failed operation.
// An empty message means the flag package already printed the reason.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

var cliCommands = []cliCommand{
	{"login", "[<url>]", "", "Approve a token in the browser and store it.", cliLogin},
	{"logout", "[<url>]", "", "Forget a stored token (revoke it in Settings → API).", cliLogout},
	{"use", "[<url>]", "", "Make an instance the default, or list them.", cliUse},
	{"whoami", "", "", "Show the token's label and scopes.", cliWhoami},
	{"accounts", "", "accounts:read", "List accounts with sync state and counts.", cliAccounts},
	{"folders", "", "mail:read", "List the folder tree per account.", cliFolders},
	{"list", "", "mail:read", "List messages newest-first (unified inbox by default).", cliList},
	{"search", "<query>", "mail:read", "Search mail in the client's query language.", cliSearch},
	{"read", "<id>", "mail:read", "Read one message: headers, flags, attachments, body.", cliRead},
	{"mark-read", "<id>", "mail:modify", "Mark a message read, or -unread.", cliMarkRead},
	{"star", "<id>", "mail:modify", "Star a message, or -off to unstar.", cliStar},
	{"move", "<id> <folder-id|archive|spam|trash>", "mail:modify", "Move a message.", cliMove},
	{"draft", "", "mail:send", "Save a draft — nothing is sent.", cliDraft},
	{"send", "", "mail:send", "Send a message now (-dry-run previews it).", cliSend},
	{"webhooks", "<subcommand>", "webhooks:manage", "Webhook tools; `listen` forwards live events to a local URL or program.", cliWebhooks},
}

// CLIVerbs lists mimux mail's verb names, for cmd/mimux's `mimux completion`
// registration hook (main.go's mailVerbs var) — the free build never imports
// this package, so it fills that hook itself from cli.go's init().
func CLIVerbs() []string {
	verbs := make([]string, len(cliCommands))
	for i, cmd := range cliCommands {
		verbs[i] = cmd.name
	}
	return verbs
}

// CLISubverbs is the same hook one level down, for the verbs that nest:
// verb name → its subcommands. Only `webhooks` has any today.
func CLISubverbs() map[string][]string {
	return map[string][]string{"webhooks": webhookSubverbs()}
}

// RunCLI is the `mimux mail` subcommand. Returns a process exit code: 0 fine,
// 1 the operation failed, 2 wrong usage.
func RunCLI(args []string) int {
	return newCLIClient(os.Stdout, os.Stderr, os.Stdin, &http.Client{Timeout: 2 * time.Minute}).dispatch(args)
}

// newCLIClient resolves where to talk before any flag is parsed: the
// environment, then the instance `use` pinned (or the only one logged in),
// then localhost. --url overrides it afterwards, and the token that goes with
// whatever it settles on is looked up later in do(). A broken credential store
// is ignored here — `login` is how you fix it, and it must still run.
func newCLIClient(out, errw io.Writer, in io.Reader, h *http.Client) *cliClient {
	stored, _ := loadCreds()
	return &cliClient{
		base:  cmp.Or(os.Getenv("MIMUX_URL"), storedBase(stored), cliDefaultURL),
		token: cmp.Or(os.Getenv("MIMUX_TOKEN"), os.Getenv("MIMUX_API_TOKEN")),
		out:   out,
		errw:  errw,
		in:    in,
		http:  h,
	}
}

func (c *cliClient) dispatch(args []string) int {
	if len(args) == 0 {
		cliUsage(c.errw)
		return 2
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		cliUsage(c.out)
		return 0
	}
	for _, cmd := range cliCommands {
		if cmd.name != args[0] {
			continue
		}
		c.scope = cmd.scope
		err := cmd.run(c, args[1:])
		switch {
		case err == nil:
			return 0
		case errors.Is(err, flag.ErrHelp):
			return 0
		}
		var ue usageError
		if errors.As(err, &ue) {
			if ue.msg != "" {
				_, _ = fmt.Fprintf(c.errw, "mimux mail %s: %s\nusage: mimux mail %s %s\n", cmd.name, ue.msg, cmd.name, cmd.args)
			}
			return 2
		}
		_, _ = fmt.Fprintf(c.errw, "mimux mail %s: %v\n", cmd.name, err)
		return 1
	}
	_, _ = fmt.Fprintf(c.errw, "mimux mail: unknown command %q\n\n", args[0])
	cliUsage(c.errw)
	return 2
}

func cliUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: mimux mail <command> [flags]")
	_, _ = fmt.Fprintln(w, "\nTalks to a running mimux over its REST API. Commands:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, cmd := range cliCommands {
		scope := cmd.scope
		if scope == "" {
			scope = "-"
		}
		_, _ = fmt.Fprintf(tw, "  %s %s\t%s\t%s\n", cmd.name, cmd.args, scope, cmd.help)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintln(w, "\nStart with `mimux mail login <url>`: it approves a token in the browser and")
	_, _ = fmt.Fprintln(w, "stores it, so nothing has to be pasted anywhere.")
	_, _ = fmt.Fprintln(w, "\nCommon flags: --url, --token, --json (add -h to any command for its own).")
	_, _ = fmt.Fprintf(w, "Config: MIMUX_URL (default %s), MIMUX_TOKEN from Settings → API — both\n", cliDefaultURL)
	_, _ = fmt.Fprintln(w, "override what login stored.")
}

// --- transport ---

// flags returns a flag set carrying the three flags every command takes. Flags
// win over the environment because they are the more specific statement.
func (c *cliClient) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("mimux mail "+name, flag.ContinueOnError)
	fs.SetOutput(c.errw)
	fs.StringVar(&c.base, "url", c.base, "base URL of the running mimux")
	fs.StringVar(&c.token, "token", c.token, "API token from Settings → API")
	fs.BoolVar(&c.json, "json", false, "print the API's JSON response verbatim")
	return fs
}

// parseFlags parses args and returns the positional ones. It permutes: the
// flag package stops at the first non-flag argument, so `search receipt -json`
// would silently fold the flag into the query, which is exactly what everyone
// types. A `--` still ends flag parsing for good — the query language negates
// with a leading `-`, so `search -- -from:noreply` has to keep working.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var operands []string
	for i, a := range args {
		if a == "--" {
			operands, args = append(operands, args[i+1:]...), args[:i]
			break
		}
	}
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usageError{} // the flag package already said what was wrong
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return operands, nil
		}
		operands, args = append(operands, rest[0]), rest[1:]
	}
}

func (c *cliClient) get(path string) (json.RawMessage, error) { return c.do("GET", path, nil) }

// do performs one API call and returns the response body, turning the API's
// error envelope into an error a human (or an agent reading stderr) can act on.
func (c *cliClient) do(method, path string, body any) (json.RawMessage, error) {
	c.applyCreds()
	if strings.TrimSpace(c.token) == "" {
		return nil, errors.New("not signed in — run `mimux mail login " + strings.TrimSuffix(c.base, "/") + "`, or set MIMUX_TOKEN to a token from Settings → API")
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimSuffix(c.base, "/")+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach %s: %w (is mimux running?)", c.base, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return raw, nil
	}
	return nil, c.apiFailure(res, raw)
}

// apiFailure renders a non-2xx answer. The API names the missing scope itself
// on a 403, which is the whole point — an agent told "forbidden" cannot fix
// itself, one told "this needs mail:modify" can tell its operator.
func (c *cliClient) apiFailure(res *http.Response, raw []byte) error {
	var env struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	msg := env.Error.Message
	switch res.StatusCode {
	case http.StatusUnauthorized:
		return errors.New("the token was rejected (unknown, revoked or expired) — mint one in Settings → API")
	case http.StatusForbidden:
		if msg == "" {
			msg = "this token is missing the " + c.scope + " scope"
		}
		return errors.New(strings.TrimSuffix(msg, ".") + " — tick it in Settings → API and mint a new token")
	case http.StatusPaymentRequired:
		return errors.New(strings.TrimSuffix(msg, ".") + " — run `mimux licence status` on the server")
	case http.StatusTooManyRequests:
		return errors.New(strings.TrimSuffix(msg, ".") + " (MIMUX_API_RATE_LIMIT sets the budget)")
	}
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if msg == "" {
		msg = res.Status
	}
	return fmt.Errorf("%s: %s", res.Status, msg)
}

// emit prints the raw JSON under --json, and otherwise hands over to the
// command's own rendering. Raw rather than re-encoded, so `| jq` sees exactly
// what the API said.
func (c *cliClient) emit(raw json.RawMessage, human func()) {
	if c.json {
		c.printf("%s\n", strings.TrimSpace(string(raw)))
		return
	}
	human()
}

// printf writes one line of human output. The error is dropped on purpose:
// the destination is a terminal or a pipe, and there is nowhere left to report
// a failure to write to it.
func (c *cliClient) printf(format string, a ...any) {
	_, _ = fmt.Fprintf(c.out, format, a...)
}

func (c *cliClient) tab() *tabwriter.Writer {
	return tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
}

// --- read commands ---

func cliWhoami(c *cliClient, args []string) error {
	fs := c.flags("whoami")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	raw, err := c.get("/api/v1/tokens/self")
	if err != nil {
		return err
	}
	var self struct {
		Label  string   `json:"label"`
		Scopes []string `json:"scopes"`
	}
	if err := json.Unmarshal(raw, &self); err != nil {
		return err
	}
	c.emit(raw, func() {
		c.printf("url:    %s\n", strings.TrimSuffix(c.base, "/"))
		c.printf("token:  %s\n", self.Label)
		c.printf("scopes: %s\n", strings.Join(self.Scopes, " "))
	})
	return nil
}

func cliAccounts(c *cliClient, args []string) error {
	fs := c.flags("accounts")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	raw, err := c.get("/api/v1/accounts")
	if err != nil {
		return err
	}
	var env struct {
		Data []accountJSON `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	c.emit(raw, func() {
		tw := c.tab()
		_, _ = fmt.Fprintln(tw, "ACCOUNT\tEMAIL\tSTATE\tMESSAGES\tUNREAD\tLAST SYNC")
		for _, a := range env.Data {
			last := "never"
			if a.LastSync != nil {
				last = a.LastSync.Local().Format("2006-01-02 15:04")
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\n", a.Name, a.Email, cmp.Or(a.State, "-"), a.Messages, a.Unread, last)
		}
		_ = tw.Flush()
	})
	return nil
}

func cliFolders(c *cliClient, args []string) error {
	fs := c.flags("folders")
	account := fs.String("account", "", "restrict to one account")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	path := "/api/v1/folders"
	if *account != "" {
		path += "?account=" + url.QueryEscape(*account)
	}
	raw, err := c.get(path)
	if err != nil {
		return err
	}
	var env struct {
		Data []accountFolders `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	c.emit(raw, func() {
		tw := c.tab()
		for _, acct := range env.Data {
			_, _ = fmt.Fprintf(tw, "%s\t\t\t\n", acct.Account)
			printFolders(tw, acct.Folders, "  ")
		}
		_ = tw.Flush()
	})
	return nil
}

func printFolders(w io.Writer, nodes []folderNodeJSON, indent string) {
	for _, n := range nodes {
		id := "-"
		if n.ID != 0 {
			id = strconv.FormatInt(n.ID, 10)
		}
		unread := ""
		if n.Unread > 0 {
			unread = strconv.Itoa(n.Unread) + " unread"
		}
		_, _ = fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\n", indent, n.Label, id, n.SpecialUse, unread)
		printFolders(w, n.Children, indent+"  ")
	}
}

func cliList(c *cliClient, args []string) error {
	fs := c.flags("list")
	account := fs.String("account", "", "that account's inbox instead of the unified one")
	folder := fs.Int64("folder", 0, "a folder id (from `mimux mail folders`)")
	limit := fs.Int("limit", 50, "messages per page, 1-500")
	unread := fs.Bool("unread", false, "only unread")
	starred := fs.Bool("starred", false, "only starred")
	cursor := fs.String("cursor", "", "next_cursor from a previous page")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	q := "limit=" + strconv.Itoa(*limit)
	if *account != "" {
		q += "&account=" + url.QueryEscape(*account)
	}
	if *folder != 0 {
		q += "&folder=" + strconv.FormatInt(*folder, 10)
	}
	if *unread {
		q += "&unread=true"
	}
	if *starred {
		q += "&starred=true"
	}
	if *cursor != "" {
		q += "&cursor=" + url.QueryEscape(*cursor)
	}
	raw, err := c.get("/api/v1/messages?" + q)
	if err != nil {
		return err
	}
	var env struct {
		Data       []messageJSON `json:"data"`
		NextCursor string        `json:"next_cursor"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	c.emit(raw, func() {
		printMessages(c, env.Data)
		if env.NextCursor != "" {
			c.printf("\nmore: --cursor %s\n", env.NextCursor)
		}
	})
	return nil
}

func cliSearch(c *cliClient, args []string) error {
	fs := c.flags("search")
	account := fs.String("account", "", "restrict to one account")
	folder := fs.Int64("folder", 0, "restrict to one folder id")
	limit := fs.Int("limit", 100, "max results (1-500)")
	deep := fs.Bool("deep", false, "ask the IMAP servers instead of the local index")
	wait := fs.Duration("wait", 30*time.Second, "how long to wait for a deep search")
	rest, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(rest, " "))
	if query == "" {
		return usageError{"a query is required"}
	}
	req := searchRequest{Query: query, Account: *account, FolderID: *folder, Limit: *limit}
	if *deep {
		req.Mode = "deep"
	}
	raw, err := c.do("POST", "/api/v1/messages/search", req)
	if err != nil {
		return err
	}
	if *deep {
		raw, err = c.pollSearch(raw, *wait)
		if err != nil {
			return err
		}
	}
	var env struct {
		Data    []messageJSON `json:"data"`
		Results []messageJSON `json:"results"`
		Status  string        `json:"status"`
		ID      string        `json:"id"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	c.emit(raw, func() {
		if env.Status == "running" {
			c.printf("still searching (job %s) — re-run the same query to pick up the cached result\n", env.ID)
			return
		}
		results := env.Data
		if results == nil {
			results = env.Results
		}
		printMessages(c, results)
	})
	return nil
}

// pollSearch follows a deep search's job until it finishes or the wait runs
// out, mirroring what search_mail does for MCP callers: results if they arrive
// in time, otherwise the job id, because the result is cached and an identical
// re-query answers instantly.
func (c *cliClient) pollSearch(raw json.RawMessage, wait time.Duration) (json.RawMessage, error) {
	var job struct{ ID, Status string }
	if err := json.Unmarshal(raw, &job); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for job.Status != "done" {
		if time.Now().After(deadline) {
			return raw, nil
		}
		time.Sleep(500 * time.Millisecond)
		var err error
		raw, err = c.get("/api/v1/search/jobs/" + job.ID)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &job); err != nil {
			return nil, err
		}
	}
	return raw, nil
}

func printMessages(c *cliClient, msgs []messageJSON) {
	if len(msgs) == 0 {
		c.printf("%s\n", "no messages")
		return
	}
	tw := c.tab()
	_, _ = fmt.Fprintln(tw, "ID\tDATE\tFLAGS\tFROM\tSUBJECT")
	for _, m := range msgs {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
			m.ID, m.Date.Local().Format("2006-01-02 15:04"), msgFlags(m), fromLabel(m.From), oneLine(m.Subject))
	}
	_ = tw.Flush()
}

// msgFlags is the compact state column: unread, starred, has an attachment.
func msgFlags(m messageJSON) string {
	f := []byte("...")
	if !m.IsRead {
		f[0] = 'u'
	}
	if m.IsStarred {
		f[1] = '*'
	}
	if m.HasAttachment {
		f[2] = '@'
	}
	return string(f)
}

func fromLabel(a addrJSON) string {
	if a.Name != "" {
		return a.Name
	}
	return a.Address
}

func oneLine(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\t", " ")
	if s == "" {
		return "(no subject)"
	}
	return s
}

func cliRead(c *cliClient, args []string) error {
	fs := c.flags("read")
	html := fs.Bool("html", false, "the sanitised HTML body instead of plain text")
	headers := fs.String("headers", "", "also print the message headers: raw, parsed or both")
	rest, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	id, err := oneID(rest)
	if err != nil {
		return err
	}
	form := "text"
	if *html {
		form = "html"
	}
	q := "?body=" + form
	if *headers != "" {
		q += "&headers=" + url.QueryEscape(*headers)
	}
	raw, err := c.get("/api/v1/messages/" + strconv.FormatInt(id, 10) + q)
	if err != nil {
		return err
	}
	var msg struct {
		messageJSON
		Body struct {
			Text string `json:"text"`
			HTML string `json:"html"`
		} `json:"body"`
		BodyError string `json:"body_error"`
		Headers   struct {
			Raw    string              `json:"raw"`
			Parsed map[string][]string `json:"parsed"`
		} `json:"headers"`
		HeadersError string `json:"headers_error"`
		Attachments  []struct {
			Index     int    `json:"index"`
			Filename  string `json:"filename"`
			MediaType string `json:"media_type"`
			Size      uint32 `json:"size"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return err
	}
	c.emit(raw, func() {
		c.printf("id:       %d\n", msg.ID)
		c.printf("account:  %s\n", msg.Account)
		c.printf("from:     %s <%s>\n", msg.From.Name, msg.From.Address)
		c.printf("to:       %s\n", strings.Join(msg.To, ", "))
		if len(msg.Cc) > 0 {
			c.printf("cc:       %s\n", strings.Join(msg.Cc, ", "))
		}
		c.printf("subject:  %s\n", oneLine(msg.Subject))
		c.printf("date:     %s\n", msg.Date.Local().Format(time.RFC1123))
		c.printf("flags:    %s\n", msgFlags(msg.messageJSON))
		if len(msg.Labels) > 0 {
			c.printf("labels:   %s\n", strings.Join(msg.Labels, ", "))
		}
		for _, at := range msg.Attachments {
			c.printf("attach:   [%d] %s (%s, %d bytes)\n", at.Index, at.Filename, at.MediaType, at.Size)
		}
		if msg.HeadersError != "" {
			c.printf("%s\n", msg.HeadersError)
		}
		if raw := strings.TrimRight(msg.Headers.Raw, "\r\n"); raw != "" {
			c.printf("\n%s\n", raw)
		}
		for _, k := range slices.Sorted(maps.Keys(msg.Headers.Parsed)) {
			for _, v := range msg.Headers.Parsed[k] {
				c.printf("%s: %s\n", k, v)
			}
		}
		_, _ = fmt.Fprintln(c.out)
		if msg.BodyError != "" {
			c.printf("%s\n", msg.BodyError)
			return
		}
		c.printf("%s\n", strings.TrimSpace(cmp.Or(msg.Body.Text, msg.Body.HTML)))
	})
	return nil
}

// --- modify commands ---

func cliMarkRead(c *cliClient, args []string) error {
	fs := c.flags("mark-read")
	unread := fs.Bool("unread", false, "mark unread instead")
	rest, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	id, err := oneID(rest)
	if err != nil {
		return err
	}
	raw, err := c.do("PATCH", "/api/v1/messages/"+strconv.FormatInt(id, 10), map[string]any{"read": !*unread})
	if err != nil {
		return err
	}
	c.emit(raw, func() {
		c.printf("%d marked %s\n", id, map[bool]string{true: "unread", false: "read"}[*unread])
	})
	return nil
}

func cliStar(c *cliClient, args []string) error {
	fs := c.flags("star")
	off := fs.Bool("off", false, "unstar instead")
	rest, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	id, err := oneID(rest)
	if err != nil {
		return err
	}
	raw, err := c.do("PATCH", "/api/v1/messages/"+strconv.FormatInt(id, 10), map[string]any{"starred": !*off})
	if err != nil {
		return err
	}
	c.emit(raw, func() {
		c.printf("%d %s\n", id, map[bool]string{true: "unstarred", false: "starred"}[*off])
	})
	return nil
}

func cliMove(c *cliClient, args []string) error {
	fs := c.flags("move")
	// Same guard as the MCP tool: trash is how mail disappears, so it takes a
	// second, explicit statement of intent.
	yes := fs.Bool("yes", false, "required for trash — it deletes mail from the human's point of view")
	rest, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 2 {
		return usageError{"want a message id and a destination"}
	}
	id, err := oneID(rest[:1])
	if err != nil {
		return err
	}
	path := "/api/v1/messages/" + strconv.FormatInt(id, 10)
	dest := rest[1]

	var raw json.RawMessage
	switch dest {
	case "archive", "spam":
		raw, err = c.do("POST", path+"/"+dest, nil)
	case "trash":
		if !*yes {
			return usageError{"moving to trash needs -yes — it deletes mail from the human's point of view"}
		}
		raw, err = c.do("DELETE", path, nil)
	default:
		fid, perr := strconv.ParseInt(dest, 10, 64)
		if perr != nil {
			return usageError{"destination must be a folder id, or archive, spam or trash"}
		}
		raw, err = c.do("PATCH", path, map[string]any{"folder_id": fid})
	}
	if err != nil {
		return err
	}
	c.emit(raw, func() { c.printf("%d moved to %s\n", id, dest) })
	return nil
}

// --- send commands ---

// composeFlags are the fields shared by `draft` and `send`.
type composeFlags struct {
	to, cc, bcc, subject, body, account string
	inReplyTo                           int64
	replyAll                            bool
}

func (f *composeFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.to, "to", "", "recipients, comma-separated")
	fs.StringVar(&f.cc, "cc", "", "cc recipients, comma-separated")
	fs.StringVar(&f.bcc, "bcc", "", "bcc recipients, comma-separated")
	fs.StringVar(&f.subject, "subject", "", "subject")
	fs.StringVar(&f.body, "body", "", "message text; read from stdin when omitted")
	fs.StringVar(&f.account, "account", "", "sending account; derived when there is only one")
	fs.Int64Var(&f.inReplyTo, "in-reply-to", 0, "a message id to reply to")
	fs.BoolVar(&f.replyAll, "reply-all", false, "with -in-reply-to: keep every original recipient")
}

// resolve fills in the body from stdin and, for a reply, derives recipients and
// subject from the original the way draft_reply does for MCP callers.
func (f *composeFlags) resolve(c *cliClient) error {
	if f.body == "" {
		b, err := readPipedBody(c.in)
		if err != nil {
			return err
		}
		f.body = b
	}
	if strings.TrimSpace(f.body) == "" {
		return usageError{"empty body — pass -body or pipe the text on stdin"}
	}
	if f.inReplyTo == 0 {
		if f.to == "" {
			return usageError{"-to is required (or -in-reply-to, to derive it)"}
		}
		return nil
	}

	raw, err := c.get("/api/v1/messages/" + strconv.FormatInt(f.inReplyTo, 10))
	if err != nil {
		return fmt.Errorf("in-reply-to: %w", err)
	}
	var orig messageJSON
	if err := json.Unmarshal(raw, &orig); err != nil {
		return err
	}
	if f.account == "" {
		f.account = orig.Account
	}
	if f.subject == "" {
		f.subject = mail.PrefixSubject("reply", orig.Subject)
	}
	if f.to == "" {
		self := c.selfAddress(f.account)
		if f.replyAll {
			to, cc := mail.ReplyAllRecipients(self, []string{orig.From.Address}, orig.To, orig.Cc)
			f.to, f.cc = strings.Join(to, ", "), strings.Join(cc, ", ")
		} else {
			f.to = strings.Join(mail.ReplyRecipients(self, orig.From.Address), ", ")
		}
	}
	return nil
}

// selfAddress is the reply's own address, so a reply-all does not mail you
// back. It needs accounts:read, which a mail:send token need not carry — a
// failure here leaves you in your own recipient list, which is worth far less
// than failing the command outright.
func (c *cliClient) selfAddress(account string) string {
	raw, err := c.get("/api/v1/accounts")
	if err != nil {
		return ""
	}
	var env struct {
		Data []accountJSON `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return ""
	}
	for _, a := range env.Data {
		if a.Name == account {
			return a.Email
		}
	}
	return ""
}

// readPipedBody reads stdin, but only when something is actually piped in:
// waiting on a terminal for a body nobody is typing just looks like a hang.
func readPipedBody(in io.Reader) (string, error) {
	if f, ok := in.(*os.File); ok {
		st, err := f.Stat()
		if err != nil || st.Mode()&os.ModeCharDevice != 0 {
			return "", nil
		}
	}
	b, err := io.ReadAll(in)
	return string(b), err
}

func cliDraft(c *cliClient, args []string) error {
	fs := c.flags("draft")
	var f composeFlags
	f.bind(fs)
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := f.resolve(c); err != nil {
		return err
	}
	req := draftRequest{
		Account: f.account, To: f.to, Cc: f.cc, Bcc: f.bcc,
		Subject: f.subject, Body: f.body, Mode: "plain", Kind: draftKind(f),
		InReplyTo: f.inReplyTo,
	}
	raw, err := c.do("POST", "/api/v1/drafts", req)
	if err != nil {
		return err
	}
	var d draftJSON
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	c.emit(raw, func() {
		printCompose(c, d.Account, d.To, d.Cc, d.Bcc, d.Subject, d.Body)
		c.printf("\nDraft %d saved, NOT sent. It is in Drafts in the web UI; send it with\n"+
			"`mimux mail send` once you are happy with the text above.\n", d.ID)
	})
	return nil
}

func draftKind(f composeFlags) string {
	switch {
	case f.inReplyTo != 0 && f.replyAll:
		return "reply_all"
	case f.inReplyTo != 0:
		return "reply"
	}
	return "new"
}

func cliSend(c *cliClient, args []string) error {
	fs := c.flags("send")
	var f composeFlags
	f.bind(fs)
	from := fs.String("from", "", "the From address (picks the account that owns it)")
	dry := fs.Bool("dry-run", false, "print exactly what would go out, and send nothing")
	at := fs.String("at", "", "schedule for an RFC3339 time instead of sending now")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := f.resolve(c); err != nil {
		return err
	}
	req := sendRequest{
		Account: f.account, From: *from,
		To: splitList(f.to), Cc: splitList(f.cc), Bcc: splitList(f.bcc),
		Subject: f.subject, Body: f.body, Mode: "plain", InReplyTo: f.inReplyTo,
	}
	if *at != "" {
		t, err := time.Parse(time.RFC3339, *at)
		if err != nil {
			return usageError{"-at wants an RFC3339 time, e.g. 2026-08-18T09:00:00Z"}
		}
		req.ScheduleAt = &t
	}
	if *dry {
		if c.json {
			b, _ := json.Marshal(req)
			c.printf("%s\n", string(b))
			return nil
		}
		printCompose(c, req.Account, f.to, f.cc, f.bcc, req.Subject, req.Body)
		c.printf("%s\n", "\nDry run — nothing was sent. Re-run without -dry-run to send it.")
		return nil
	}
	raw, err := c.do("POST", "/api/v1/messages/send", req)
	if err != nil {
		return err
	}
	var out struct {
		Status    string `json:"status"`
		MessageID string `json:"message_id"`
		SendAt    string `json:"send_at"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	c.emit(raw, func() {
		if out.Status == "scheduled" {
			c.printf("scheduled for %s\n", out.SendAt)
			return
		}
		c.printf("sent %s\n", out.MessageID)
	})
	return nil
}

func printCompose(c *cliClient, account, to, cc, bcc, subject, body string) {
	if account != "" {
		c.printf("account:  %s\n", account)
	}
	c.printf("to:       %s\n", to)
	if cc != "" {
		c.printf("cc:       %s\n", cc)
	}
	if bcc != "" {
		c.printf("bcc:      %s\n", bcc)
	}
	c.printf("subject:  %s\n", subject)
	c.printf("\n%s\n", strings.TrimSpace(body))
}

// --- small shared bits ---

func oneID(args []string) (int64, error) {
	if len(args) != 1 {
		return 0, usageError{"want exactly one message id"}
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return 0, usageError{"message id must be a number, got " + args[0]}
	}
	return id, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
