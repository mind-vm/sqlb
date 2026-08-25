package codegen

// clientRuntime is the generated Go client: the request type, the transport
// seam, the problem document, and the two things a caller would otherwise
// hand-write badly — the cursor walk and the rendering of an error that names
// what would have been accepted.
//
// It imports the standard library and nothing else. That is the whole point of
// it being its own package: a sync job or an admin tool that wants the typed
// encoder should not take a command-line framework to get it (#97).
//
// It is emitted rather than published as a package so that the generated
// command tree is a directory a project owns and can read, with no import of
// sqlb on the request path. That is the same call the TypeScript client makes,
// and it is what keeps sqlb out of a binary whose only job is to talk HTTP.
//
// Backquotes appear as concatenations because a raw string literal cannot
// contain one, and the struct tags below need them.
const clientRuntime = `
// ------------------------------------------------------------------- runtime

// Request is one call to the API, as the generated commands describe it.
type Request struct {
	Method string
	// Path is measured from the API root and already encoded, e.g. "/tasks/6b1e".
	Path string
	// Query is the source of the query string. Repeating a key conjoins filter
	// conditions, which is why this is url.Values rather than a map of strings.
	Query url.Values
	// Body is marshalled as JSON when it is not nil.
	Body any
	// Header carries what a schema cannot derive: tenant selection, an
	// idempotency key, a trace id propagated from whatever called this binary.
	// Do applies it last, so a header set here replaces Accept, Content-Type
	// or Authorization rather than being dropped behind them — the same
	// precedence Client.Token already has over nothing, extended to a caller
	// whose auth is not a bearer token at all (#254). Nothing generates this;
	// it is for a hand-written command built on top of Client.Run.
	Header http.Header
}

// Transport issues one request and returns the decoded response body, or nil
// where the response carried none.
//
// Everything the schema cannot derive lives behind this: the base URL, the
// credential, retry, and what a 401 does. Client implements one over net/http,
// and setting Client.Transport replaces it — which is the seam to use for a
// test that must not open a socket, or for a caller whose auth is a signature
// rather than a bearer token.
type Transport func(ctx context.Context, req Request) (json.RawMessage, error)

// Client is the configuration a schema cannot supply. The root command binds
// its fields to persistent flags.
type Client struct {
	// BaseURL is the root of the API, without a trailing slash.
	BaseURL string
	// Token is sent as an Authorization: Bearer header when it is set.
	Token string
	// Timeout bounds a single request. Zero means no timeout.
	Timeout time.Duration
	// Compact writes each response on one line rather than indenting it.
	Compact bool
	// Verbose logs each request's method and URL to Stderr.
	Verbose bool

	// HTTP is the client the built-in transport issues requests through.
	// Leaving it nil builds one from Timeout.
	HTTP *http.Client
	// Transport replaces the built-in one entirely. Set it and BaseURL, Token,
	// Timeout and HTTP are yours to honour or ignore.
	//
	// Do is still reachable, and does not consult this field, so wrapping the
	// built-in transport rather than replacing it is a closure that ends in a
	// call to it:
	//
	//	c.Transport = func(ctx context.Context, req Request) (json.RawMessage, error) {
	//	    req.Query.Set("trace", traceID(ctx))
	//	    return c.Do(ctx, req)
	//	}
	Transport Transport
	// Stderr receives verbose logging. Defaults to os.Stderr.
	Stderr io.Writer
}

// maxResponseBytes caps what one response may occupy in memory. A page is
// bounded by the resource's ceiling, so reaching this means something upstream
// is not the API this client was generated against.
const maxResponseBytes = 64 << 20

func (c *Client) stderr() io.Writer {
	if c.Stderr != nil {
		return c.Stderr
	}
	return os.Stderr
}

func (c *Client) transport() Transport {
	if c.Transport != nil {
		return c.Transport
	}
	return c.Do
}

// Do is the built-in transport, over net/http.
//
// It is exported so that a caller replacing Transport can still delegate to
// it; it does not consult Transport itself, so doing so cannot recurse.
func (c *Client) Do(ctx context.Context, req Request) (json.RawMessage, error) {
	base := strings.TrimSuffix(c.BaseURL, "/")
	if base == "" {
		return nil, errors.New("no API address: pass --base-url")
	}
	u := base + req.Path
	if len(req.Query) > 0 {
		// Encode sorts by key, so the same flags always produce the same URL —
		// which is what makes a request comparable in a log and cacheable by a
		// proxy in front of the API.
		u += "?" + req.Query.Encode()
	}

	var payload io.Reader
	if req.Body != nil {
		raw, err := json.Marshal(req.Body)
		if err != nil {
			return nil, fmt.Errorf("encoding the request body: %w", err)
		}
		payload = bytes.NewReader(raw)
	}

	// Bound by context rather than only by client.Timeout below, so that a
	// caller who supplies HTTP — the seam a header with no other way in pushes
	// a caller toward — does not silently lose --timeout along with it (#254).
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}

	r, err := http.NewRequestWithContext(ctx, req.Method, u, payload)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Accept", "application/json")
	if req.Body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		r.Header.Set("Authorization", "Bearer "+c.Token)
	}
	// req.Header is applied last and replaces rather than adds, so a caller
	// whose auth is not Client.Token — a signature, a second identity header —
	// can override anything derived above it (#254).
	for k, vs := range req.Header {
		r.Header.Del(k)
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	if c.Verbose {
		fmt.Fprintln(c.stderr(), req.Method, u)
	}

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: c.Timeout}
	}
	resp, err := client.Do(r)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("reading the response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, problemFrom(resp.StatusCode, body)
	}
	if resp.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	return json.RawMessage(body), nil
}

// Run issues one request and writes the response to out.
//
// It takes a context and a writer rather than a *cobra.Command, which is what
// lets this package compile without cobra: the command tree passes cmd.Context
// and cmd.OutOrStdout, and a server-to-server caller passes its own (#97).
func (c *Client) Run(ctx context.Context, out io.Writer, req Request, all bool) error {
	if ctx == nil {
		ctx = context.Background()
	}

	var (
		raw json.RawMessage
		err error
	)
	if all {
		raw, err = listAll(ctx, c.transport(), req)
	} else {
		raw, err = c.transport()(ctx, req)
	}
	if err != nil {
		return err
	}
	// A delete answers 204, and there is nothing to print. Writing "null" would
	// make a shell test for emptiness fail.
	if len(raw) == 0 {
		return nil
	}
	return writeJSON(out, raw, c.Compact)
}

// listAll walks a collection by cursor and returns every row as one page.
//
// This is the loop a caller writes by hand, and it pages with ` + "`?cursor=`" + ` rather
// than ` + "`?page=`" + `: a walk that counts to its position costs more with every page
// and can read a row twice when the table is written to underneath it, which is
// exactly what a long walk makes likely.
func listAll(ctx context.Context, t Transport, req Request) (json.RawMessage, error) {
	type page struct {
		Items      []json.RawMessage ` + "`json:\"items\"`" + `
		HasMore    bool              ` + "`json:\"has_more\"`" + `
		NextCursor *string           ` + "`json:\"next_cursor,omitempty\"`" + `
		Total      *int64            ` + "`json:\"total,omitempty\"`" + `
	}

	out := page{Items: []json.RawMessage{}}
	q := cloneValues(req.Query)
	seen := map[string]bool{}

	for {
		req.Query = q
		raw, err := t(ctx, req)
		if err != nil {
			return nil, err
		}
		var p page
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("walking the collection: the response is not a list page: %w", err)
		}
		out.Items = append(out.Items, p.Items...)
		// The total describes the whole result set rather than the page, so the
		// first response's answer is the answer.
		if out.Total == nil {
			out.Total = p.Total
		}
		if p.NextCursor == nil || *p.NextCursor == "" {
			break
		}
		// A server that answered with the cursor it was handed would otherwise
		// spin here forever, reading one page over and over.
		if seen[*p.NextCursor] {
			return nil, fmt.Errorf("the server repeated cursor %q; stopping rather than paging forever", *p.NextCursor)
		}
		seen[*p.NextCursor] = true

		q = cloneValues(q)
		q.Set("cursor", *p.NextCursor)
		// A cursor names a position, so a page number or an offset alongside it
		// is a second, contradictory answer to the same question.
		q.Del("page")
		q.Del("offset")
	}
	return json.Marshal(out)
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, values := range v {
		out[k] = append([]string(nil), values...)
	}
	return out
}

// writeJSON writes a response body, indented unless asked otherwise.
func writeJSON(w io.Writer, raw json.RawMessage, compact bool) error {
	var buf bytes.Buffer
	if compact {
		if err := json.Compact(&buf, raw); err != nil {
			return err
		}
	} else if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return err
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}

// Problem is the RFC 9457 document every rejection returns.
type Problem struct {
	Type   string           ` + "`json:\"type,omitempty\"`" + `
	Title  string           ` + "`json:\"title,omitempty\"`" + `
	Status int              ` + "`json:\"status,omitempty\"`" + `
	Detail string           ` + "`json:\"detail,omitempty\"`" + `
	// Errors lists every problem found, not only the first, so a malformed
	// request takes one round trip to fix rather than one per mistake.
	Errors []*ProblemDetail ` + "`json:\"errors,omitempty\"`" + `
}

// ProblemDetail is one rejected parameter or field.
type ProblemDetail struct {
	Message  string   ` + "`json:\"message\"`" + `
	Location string   ` + "`json:\"location,omitempty\"`" + `
	Value    any      ` + "`json:\"value,omitempty\"`" + `
	// Allowed is what would have been accepted instead, where the set is
	// finite. It is the half of a rejection that turns a dead end into a fix,
	// and printing it is most of the reason this client renders errors itself
	// rather than echoing the response body.
	Allowed []string ` + "`json:\"allowed,omitempty\"`" + `
}

// Error renders the document as the message the caller sees on stderr.
func (p *Problem) Error() string {
	head := p.Detail
	if head == "" {
		head = p.Title
	}
	if head == "" {
		head = "the request was rejected"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s (HTTP %d)", head, p.Status)
	for _, d := range p.Errors {
		b.WriteString("\n  ")
		if d.Location != "" {
			b.WriteString(d.Location + ": ")
		}
		b.WriteString(d.Message)
		if len(d.Allowed) > 0 {
			fmt.Fprintf(&b, "\n    allowed: %s", strings.Join(d.Allowed, ", "))
		}
	}
	return b.String()
}

// problemFrom decodes an error response, falling back to the status line for a
// body this client did not produce — a proxy's 502 page, say.
func problemFrom(status int, body []byte) error {
	var p Problem
	if err := json.Unmarshal(body, &p); err == nil && (p.Status != 0 || p.Detail != "" || len(p.Errors) > 0) {
		if p.Status == 0 {
			p.Status = status
		}
		return &p
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 512 {
		text = text[:512] + "..."
	}
	if text == "" {
		return errors.New("HTTP " + strconv.Itoa(status))
	}
	return errors.New("HTTP " + strconv.Itoa(status) + ": " + text)
}

// ItemPath addresses one row. The path template is always {id} whatever the
// primary key column is called, because the URL names the resource's identity
// rather than its storage.
func ItemPath(collection, id string) string {
	return collection + "/" + url.PathEscape(id)
}

`

// cliCobra is the part of the emitted CLI that needs cobra. It is the whole of
// what kept the generated Go client from being importable on its own: three
// helpers, against a runtime that never needed the dependency (#97).
const cliCobra = `
// --------------------------------------------------------------------- cobra

// nullableColumn is a column --set-null accepts, in its two spellings: the one
// the flag is written with, which is the column's own and does not move when
// the schema declares a WireCase, and the one the request body has to use,
// which does. Under the default they are the same string.
type nullableColumn struct {
	Column string
	Wire   string
}

// setNullFields records an explicit null for each named column.
//
// A value flag can say that a column is now empty; it cannot say that it is now
// null, and the two write different SQL. This is the command-line form of the
// presence map the generated patch body keeps for the same reason.
func setNullFields(body map[string]any, names []string, nullable []nullableColumn) error {
	allowed := make([]string, 0, len(nullable))
	for _, c := range nullable {
		allowed = append(allowed, c.Column)
	}
	for _, name := range names {
		column := strings.ReplaceAll(name, "-", "_")
		wire := ""
		for _, c := range nullable {
			if c.Column == column {
				wire = c.Wire
				break
			}
		}
		if wire == "" {
			return fmt.Errorf("--set-null %s: not a nullable column of this resource; allowed: %s",
				name, strings.Join(allowed, ", "))
		}
		body[wire] = nil
	}
	return nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// orString and orDuration resolve one setting's default.
//
// Binding a flag writes its default into the variable, so a Client the caller
// configured would be overwritten by registration alone. Feeding its value in
// as the default instead gives the precedence a reader would expect: the flag
// wins over the field, the field over the environment, and the environment
// over the built-in.
func orString(configured, def string) string {
	if configured != "" {
		return configured
	}
	return def
}

func orDuration(configured, def time.Duration) time.Duration {
	if configured != 0 {
		return configured
	}
	return def
}

// runRequest bridges cobra to the client's Run: the command supplies the
// context and the output stream, and everything else is the client package's.
//
// Named runRequest rather than run because a generated unexported identifier
// shares a package with whatever the consumer writes beside it, and run is a
// name a hand-written file in a command package is likely to want — the tasks
// example had one.
func runRequest(c *client.Client, cmd *cobra.Command, req client.Request, all bool) error {
	return c.Run(cmd.Context(), cmd.OutOrStdout(), req, all)
}

// normalizeFlag accepts a column's own spelling as well as the kebab-case one
// cobra conventionally uses, so a name read out of sqlb.json or out of an error
// response can be typed verbatim.
func normalizeFlag(_ *pflag.FlagSet, name string) pflag.NormalizedName {
	return pflag.NormalizedName(strings.ReplaceAll(name, "_", "-"))
}

// registerCompletion offers a fixed set of values for a flag.
//
// The error is dropped deliberately: it reports only that the flag does not
// exist, which would be a generator bug rather than a runtime condition, and
// the flag is declared one line above every call.
func registerCompletion(cmd *cobra.Command, flag string, values []string) {
	_ = cmd.RegisterFlagCompletionFunc(flag,
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return values, cobra.ShellCompDirectiveNoFileComp
		})
}
`
