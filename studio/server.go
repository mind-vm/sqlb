package studio

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

//go:embed templates/base.html templates/index.html templates/table.html templates/login.html templates/rows.html templates/row.html templates/form.html templates/action.html templates/import.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

var templateFuncs = template.FuncMap{
	"isCollection": func(path string) bool { return !strings.Contains(path, "{id}") },
}

// Server renders a Manifest as a browsable data/schema explorer. Schema
// pages need only the manifest; the data pages call APIBase with the
// operator's own bearer token (see session.go) — the browser inherits
// whatever that token can already see, and nothing more (docs/architecture.md,
// "The manifest describes what cannot be guessed").
type Server struct {
	manifest *schema.Manifest
	apiBase  string
	basePath string

	// credential is the optional application sign-in hook. Its zero value
	// renders the token-paste form alone, which is what studio did before it
	// existed. See CredentialLogin.
	credential CredentialLogin

	index     *template.Template
	table     *template.Template
	login     *template.Template
	rows      *template.Template
	row       *template.Template
	form      *template.Template
	action    *template.Template
	importTpl *template.Template
}

// NewServer parses the embedded templates and pairs them with m. apiBase is
// the running application's REST API root; empty disables the data pages and
// leaves the schema-only view (stage one) working on its own.
//
// basePath is optional and defaults to "" (root-mounted, cmd/sqlb-studio's
// own use — every href, redirect and asset reference is root-absolute). Pass
// one path segment, e.g. NewServer(m, apiBase, "/studio"), to mount the
// result under a prefix on someone else's mux; see Handler's doc comment for
// how. Passing more than one is a programming error and panics — the
// variadic form exists only to keep the two-argument call compiling, not to
// take a list.
func NewServer(m *schema.Manifest, apiBase string, basePath ...string) (*Server, error) {
	bp := ""
	switch len(basePath) {
	case 0:
	case 1:
		bp = normalizeBasePath(basePath[0])
	default:
		panic("studio.NewServer: at most one basePath argument")
	}
	s := &Server{manifest: m, apiBase: apiBase, basePath: bp}
	var err error
	funcs := template.FuncMap{"url": s.url}
	for name, fn := range templateFuncs {
		funcs[name] = fn
	}
	parse := func(files ...string) *template.Template {
		if err != nil {
			return nil
		}
		var t *template.Template
		t, err = template.New(files[0]).Funcs(funcs).ParseFS(templateFS, files...)
		return t
	}
	s.index = parse("templates/base.html", "templates/index.html")
	s.table = parse("templates/base.html", "templates/table.html")
	s.login = parse("templates/base.html", "templates/login.html")
	s.rows = parse("templates/base.html", "templates/rows.html")
	s.row = parse("templates/base.html", "templates/row.html")
	s.form = parse("templates/base.html", "templates/form.html")
	s.action = parse("templates/base.html", "templates/action.html")
	s.importTpl = parse("templates/base.html", "templates/import.html")
	if err != nil {
		return nil, err
	}
	return s, nil
}

// normalizeBasePath strips a trailing slash and ensures a non-empty path
// starts with one, so the rule lives here once rather than being re-derived
// at every call site that joins it against a route.
func normalizeBasePath(p string) string {
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// url prefixes path with the server's mount point. Every hardcoded
// root-absolute href, redirect target and asset reference — in server.go and
// in templates/*.html via the "url" template func — goes through this (or
// r.URL.RequestURI(), which already reflects the real request path) rather
// than concatenating a leading "/" directly, so the result is correct
// whether Handler is mounted at the root or under basePath.
func (s *Server) url(path string) string {
	return s.basePath + path
}

// Handler returns the studio's HTTP handler, routed under the basePath given
// to NewServer (the empty string by default). Mount it directly at that same
// prefix — e.g. mux.Handle("/studio/", studioSrv.Handler()) after
// NewServer(m, apiBase, "/studio") — with no http.StripPrefix: every route
// registered here, every asset path and every link or redirect the handlers
// build already carries basePath, so stripping it before the request reaches
// this mux would make every route fail to match.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // embedded at build time; a failure here is a build bug
	}
	staticPrefix := s.basePath + "/static/"
	mux.Handle("GET "+staticPrefix, http.StripPrefix(staticPrefix, http.FileServerFS(static)))

	routes := []struct {
		pattern string
		handler http.HandlerFunc
	}{
		{"GET /{$}", s.handleIndex},
		{"GET /tables/{name}", s.handleTable},
		{"GET /tables/{name}/rows", s.handleRows},
		{"GET /tables/{name}/rows/new", s.handleRowNewForm},
		{"POST /tables/{name}/rows/new", s.handleRowNewSubmit},
		{"GET /tables/{name}/rows/export", s.handleRowsExport},
		{"GET /tables/{name}/rows/import", s.handleRowsImportForm},
		{"POST /tables/{name}/rows/import", s.handleRowsImportSubmit},
		{"GET /tables/{name}/rows/{id}", s.handleRowDetail},
		{"GET /tables/{name}/rows/{id}/edit", s.handleRowEditForm},
		{"POST /tables/{name}/rows/{id}/edit", s.handleRowEditSubmit},
		{"GET /tables/{name}/actions/{action}", s.handleActionForm},
		{"POST /tables/{name}/actions/{action}", s.handleActionSubmit},
		{"GET /tables/{name}/rows/{id}/actions/{action}", s.handleActionForm},
		{"POST /tables/{name}/rows/{id}/actions/{action}", s.handleActionSubmit},
		{"GET /login", s.handleLoginForm},
		{"POST /login", s.handleLoginSubmit},
		{"POST /logout", s.handleLogout},
	}
	for _, rt := range routes {
		method, path, _ := strings.Cut(rt.pattern, " ")
		mux.HandleFunc(method+" "+s.basePath+path, rt.handler)
	}

	return mux
}

// pageHeader is embedded in every page so base.html can render the navbar
// (module name, sign-in state) the same way regardless of which page it
// wraps.
type pageHeader struct {
	Module   string
	LoggedIn bool
}

func (s *Server) header(r *http.Request) pageHeader {
	return pageHeader{Module: s.manifest.Module, LoggedIn: tokenFromRequest(r) != ""}
}

func (s *Server) findTable(name string) *schema.TableManifest {
	for i := range s.manifest.Tables {
		if s.manifest.Tables[i].Name == name {
			return &s.manifest.Tables[i]
		}
	}
	return nil
}

func wireOf(c schema.ColumnManifest) string {
	if c.Wire != "" {
		return c.Wire
	}
	return c.Name
}

func containsOp(ops []string, op string) bool {
	for _, o := range ops {
		if o == op {
			return true
		}
	}
	return false
}

// dispValue renders a decoded JSON value the way an operator wants to read
// it. It exists because {{.}} on a bare `any` holding a JSON-decoded value
// prints Go's %v form (a float64 as "1", a nil map entry as "<no value>"),
// neither of which is what the response actually said.
func dispValue(v any) string {
	if v == nil {
		return "—"
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	}
}

type indexPage struct {
	pageHeader
	Tables []schema.TableManifest
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	tables := append([]schema.TableManifest(nil), s.manifest.Tables...)
	sort.Slice(tables, func(i, j int) bool { return tables[i].Name < tables[j].Name })
	s.render(w, s.index, indexPage{pageHeader: s.header(r), Tables: tables})
}

type tablePage struct {
	pageHeader
	Table     schema.TableManifest
	CanBrowse bool
}

func (s *Server) handleTable(w http.ResponseWriter, r *http.Request) {
	t := s.findTable(r.PathValue("name"))
	if t == nil {
		http.NotFound(w, r)
		return
	}
	canBrowse := s.apiBase != "" && t.REST != nil && containsOp(t.REST.Operations, "list")
	s.render(w, s.table, tablePage{pageHeader: s.header(r), Table: *t, CanBrowse: canBrowse})
}

// displayRow is one row of a data grid, pre-rendered so the template stays
// free of lookup logic: PK for the row's detail link, Cells parallel to the
// page's Columns.
type displayRow struct {
	PK    string
	Cells []string
}

type rowsPage struct {
	pageHeader
	Table         schema.TableManifest
	Columns       []schema.ColumnManifest
	Rows          []displayRow
	Page, PerPage int
	HasMore       bool
	CanCreate     bool
	CanExport     bool

	Filters       []filterField
	SortOptions   []string
	CurrentSort   string
	HasSearch     bool
	CurrentSearch string
	PrevURL       string
	NextURL       string
	ExportBase    string
	ExportQuery   string
}

// pageURL clones the request's current query, points it at another page and
// re-encodes it, so Previous/Next and a redisplayed filter form all carry
// forward whatever filter/sort/search the grid is already showing rather
// than resetting it.
func pageURL(r *http.Request, page int) string {
	q := cloneQuery(r.URL.Query())
	q.Set("page", strconv.Itoa(page))
	return "?" + q.Encode()
}

func cloneQuery(q url.Values) url.Values {
	out := make(url.Values, len(q))
	maps.Copy(out, q)
	return out
}

// exportQuery is the request's own filter/sort/search params, re-encoded
// without page, for the export links to append after their own format=
// param — export.go's handleRowsExport reads it back through
// combineFilters, the same function handleRows itself uses, so "export what
// I'm looking at" reads the identical filter.
func exportQuery(r *http.Request) string {
	q := cloneQuery(r.URL.Query())
	q.Del("page")
	return q.Encode()
}

func (s *Server) handleRows(w http.ResponseWriter, r *http.Request) {
	t := s.findTable(r.PathValue("name"))
	if t == nil || t.REST == nil || !containsOp(t.REST.Operations, "list") {
		http.NotFound(w, r)
		return
	}
	client, ok := s.clientFor(w, r)
	if !ok {
		return
	}

	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			page = n
		}
	}
	q := combineFilters(t, r.URL.Query())
	q.Set("page", strconv.Itoa(page))

	result, err := client.List(r.Context(), t.REST.Path, q)
	if err != nil {
		s.renderAPIError(w, r, err)
		return
	}

	data := rowsPage{
		pageHeader:    s.header(r),
		Table:         *t,
		Columns:       t.Columns,
		Page:          result.Page,
		PerPage:       result.PerPage,
		HasMore:       result.HasMore,
		CanCreate:     containsOp(t.REST.Operations, "create"),
		CanExport:     true,
		Filters:       buildFilterFields(t, r.URL.Query()),
		SortOptions:   sortOptions(t),
		CurrentSort:   r.URL.Query().Get("sort"),
		HasSearch:     len(t.REST.Searchable) > 0,
		CurrentSearch: r.URL.Query().Get("search"),
		PrevURL:       pageURL(r, page-1),
		NextURL:       pageURL(r, page+1),
		ExportBase:    s.url("/tables/" + t.Name + "/rows/export"),
		ExportQuery:   exportQuery(r),
	}
	for _, row := range result.Items {
		dr := displayRow{}
		for _, col := range t.Columns {
			dr.Cells = append(dr.Cells, dispValue(row[wireOf(col)]))
		}
		if pk := findColumn(t.Columns, t.PrimaryKey); pk != nil {
			dr.PK = dispValue(row[wireOf(*pk)])
		}
		data.Rows = append(data.Rows, dr)
	}
	s.render(w, s.rows, data)
}

func findColumn(cols []schema.ColumnManifest, name string) *schema.ColumnManifest {
	for i := range cols {
		if cols[i].Name == name {
			return &cols[i]
		}
	}
	return nil
}

type fieldValue struct {
	Name, Value, Link string
}

type actionLink struct {
	Name, InvokeLink string
}

type rowPage struct {
	pageHeader
	Table    schema.TableManifest
	Fields   []fieldValue
	CanEdit  bool
	EditLink string
	Actions  []actionLink
}

func (s *Server) handleRowDetail(w http.ResponseWriter, r *http.Request) {
	t := s.findTable(r.PathValue("name"))
	if t == nil || t.REST == nil {
		http.NotFound(w, r)
		return
	}
	client, ok := s.clientFor(w, r)
	if !ok {
		return
	}

	row, err := client.Get(r.Context(), t.REST.Path+"/"+r.PathValue("id"))
	if err != nil {
		s.renderAPIError(w, r, err)
		return
	}

	id := r.PathValue("id")
	data := rowPage{
		pageHeader: s.header(r),
		Table:      *t,
		CanEdit:    containsOp(t.REST.Operations, "update"),
		EditLink:   s.url("/tables/" + t.Name + "/rows/" + id + "/edit"),
	}
	for _, a := range t.REST.Actions {
		if !isCollectionActionPath(a.Path) {
			data.Actions = append(data.Actions, actionLink{
				Name:       a.Name,
				InvokeLink: s.url("/tables/" + t.Name + "/rows/" + id + "/actions/" + a.Name),
			})
		}
	}
	for _, col := range t.Columns {
		wire := wireOf(col)
		val := row[wire]
		link := ""
		if col.References != nil && !col.References.External && val != nil {
			link = s.url("/tables/" + col.References.Table + "/rows/" + dispValue(val))
		}
		data.Fields = append(data.Fields, fieldValue{Name: col.Name, Value: dispValue(val), Link: link})
	}
	s.render(w, s.row, data)
}

type formPage struct {
	pageHeader
	Table  schema.TableManifest
	Fields []formField
	Action string
	Title  string
	Back   string
	Error  string
}

func (s *Server) handleRowNewForm(w http.ResponseWriter, r *http.Request) {
	t := s.findTable(r.PathValue("name"))
	if t == nil || t.REST == nil || !containsOp(t.REST.Operations, "create") {
		http.NotFound(w, r)
		return
	}
	s.render(w, s.form, formPage{
		pageHeader: s.header(r),
		Table:      *t,
		Fields:     buildFormFields(t, createInput(t), nil),
		Action:     s.url("/tables/" + t.Name + "/rows/new"),
		Title:      "New " + t.Name + " row",
		Back:       s.url("/tables/" + t.Name + "/rows"),
	})
}

func (s *Server) handleRowNewSubmit(w http.ResponseWriter, r *http.Request) {
	t := s.findTable(r.PathValue("name"))
	if t == nil || t.REST == nil || !containsOp(t.REST.Operations, "create") {
		http.NotFound(w, r)
		return
	}
	client, ok := s.clientFor(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	action, title, back := s.url("/tables/"+t.Name+"/rows/new"), "New "+t.Name+" row", s.url("/tables/"+t.Name+"/rows")

	props := createInput(t)
	body, err := parseFormBody(t, props, r.PostForm)
	if err != nil {
		s.renderFormError(w, r, t, props, r.PostForm, action, title, back, err.Error())
		return
	}
	created, err := client.Create(r.Context(), t.REST.Path, body)
	if err != nil {
		s.renderFormAPIError(w, r, t, props, r.PostForm, action, title, back, err)
		return
	}

	dest := back
	if pk := findColumn(t.Columns, t.PrimaryKey); pk != nil {
		if v := dispValue(created[wireOf(*pk)]); v != "" && v != "—" {
			dest = s.url("/tables/" + t.Name + "/rows/" + v)
		}
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

func (s *Server) handleRowEditForm(w http.ResponseWriter, r *http.Request) {
	t := s.findTable(r.PathValue("name"))
	if t == nil || t.REST == nil || !containsOp(t.REST.Operations, "update") {
		http.NotFound(w, r)
		return
	}
	client, ok := s.clientFor(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	row, err := client.Get(r.Context(), t.REST.Path+"/"+id)
	if err != nil {
		s.renderAPIError(w, r, err)
		return
	}
	s.render(w, s.form, formPage{
		pageHeader: s.header(r),
		Table:      *t,
		Fields:     buildFormFields(t, nil, row),
		Action:     s.url("/tables/" + t.Name + "/rows/" + id + "/edit"),
		Title:      "Edit " + t.Name + " row",
		Back:       s.url("/tables/" + t.Name + "/rows/" + id),
	})
}

func (s *Server) handleRowEditSubmit(w http.ResponseWriter, r *http.Request) {
	t := s.findTable(r.PathValue("name"))
	if t == nil || t.REST == nil || !containsOp(t.REST.Operations, "update") {
		http.NotFound(w, r)
		return
	}
	client, ok := s.clientFor(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id := r.PathValue("id")
	action := s.url("/tables/" + t.Name + "/rows/" + id + "/edit")
	title := "Edit " + t.Name + " row"
	back := s.url("/tables/" + t.Name + "/rows/" + id)

	body, err := parseFormBody(t, nil, r.PostForm)
	if err != nil {
		s.renderFormError(w, r, t, nil, r.PostForm, action, title, back, err.Error())
		return
	}
	if _, err := client.Patch(r.Context(), t.REST.Path+"/"+id, body); err != nil {
		s.renderFormAPIError(w, r, t, nil, r.PostForm, action, title, back, err)
		return
	}
	http.Redirect(w, r, back, http.StatusFound)
}

func (s *Server) renderFormError(w http.ResponseWriter, r *http.Request, t *schema.TableManifest, props []schema.BodyProperty, form url.Values, action, title, back, errMsg string) {
	s.render(w, s.form, formPage{
		pageHeader: s.header(r),
		Table:      *t,
		Fields:     formFieldsFromForm(t, props, form),
		Action:     action,
		Title:      title,
		Back:       back,
		Error:      errMsg,
	})
}

// renderFormAPIError is renderAPIError's counterpart for a form submission: a
// stale token still bounces to /login, but every other failure re-renders the
// form with what the operator typed rather than losing it behind a generic
// error page.
func (s *Server) renderFormAPIError(w http.ResponseWriter, r *http.Request, t *schema.TableManifest, props []schema.BodyProperty, form url.Values, action, title, back string, err error) {
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
		s.clearTokenCookie(w)
		http.Redirect(w, r, s.url("/login")+"?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	msg := err.Error()
	if errors.As(err, &ae) {
		msg = fmt.Sprintf("%d from API: %s", ae.Status, ae.Body)
	}
	s.renderFormError(w, r, t, props, form, action, title, back, msg)
}

func (s *Server) findAction(t *schema.TableManifest, name string) *schema.ActionManifest {
	if t.REST == nil {
		return nil
	}
	for i := range t.REST.Actions {
		if t.REST.Actions[i].Name == name {
			return &t.REST.Actions[i]
		}
	}
	return nil
}

// resolveActionPath substitutes a row's primary key into a declared item
// action's {id} placeholder; a collection action (id == "") is returned as
// declared.
func resolveActionPath(path, id string) string {
	if id == "" {
		return path
	}
	return strings.Replace(path, "{id}", id, 1)
}

type actionPage struct {
	pageHeader
	Table  schema.TableManifest
	Action schema.ActionManifest
	Fields []formField
	Back   string
	Result string
	Error  string
}

// actionRoute resolves the {name}/{action}/{id?} triple and rejects a
// mismatch between the URL shape and what the action itself declares — a
// collection action reached through a /rows/{id}/ URL, or an item action
// reached without one, is a 404 rather than a call with a broken path.
func (s *Server) actionRoute(r *http.Request) (t *schema.TableManifest, action *schema.ActionManifest, id, back string, ok bool) {
	t = s.findTable(r.PathValue("name"))
	if t == nil {
		return nil, nil, "", "", false
	}
	action = s.findAction(t, r.PathValue("action"))
	if action == nil {
		return nil, nil, "", "", false
	}
	id = r.PathValue("id")
	if isCollectionActionPath(action.Path) != (id == "") {
		return nil, nil, "", "", false
	}
	back = s.url("/tables/" + t.Name)
	if id != "" {
		back = s.url("/tables/" + t.Name + "/rows/" + id)
	}
	return t, action, id, back, true
}

func isCollectionActionPath(path string) bool { return !strings.Contains(path, "{id}") }

func (s *Server) handleActionForm(w http.ResponseWriter, r *http.Request) {
	t, action, _, back, ok := s.actionRoute(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.render(w, s.action, actionPage{
		pageHeader: s.header(r),
		Table:      *t,
		Action:     *action,
		Fields:     buildBodyFields(action.Body),
		Back:       back,
	})
}

func (s *Server) handleActionSubmit(w http.ResponseWriter, r *http.Request) {
	t, action, id, back, ok := s.actionRoute(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	client, ok := s.clientFor(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	body, err := parseActionBody(action.Body, r.PostForm)
	if err != nil {
		s.renderActionError(w, r, t, action, back, r.PostForm, err.Error())
		return
	}

	result, err := client.Create(r.Context(), resolveActionPath(action.Path, id), body)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			s.clearTokenCookie(w)
			http.Redirect(w, r, s.url("/login")+"?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		msg := err.Error()
		if errors.As(err, &ae) {
			msg = fmt.Sprintf("%d from API: %s", ae.Status, ae.Body)
		}
		s.renderActionError(w, r, t, action, back, r.PostForm, msg)
		return
	}

	pretty, _ := json.MarshalIndent(result, "", "  ")
	s.render(w, s.action, actionPage{
		pageHeader: s.header(r),
		Table:      *t,
		Action:     *action,
		Fields:     buildBodyFields(action.Body),
		Back:       back,
		Result:     string(pretty),
	})
}

func (s *Server) renderActionError(w http.ResponseWriter, r *http.Request, t *schema.TableManifest, action *schema.ActionManifest, back string, form url.Values, errMsg string) {
	s.render(w, s.action, actionPage{
		pageHeader: s.header(r),
		Table:      *t,
		Action:     *action,
		Fields:     bodyFieldsFromForm(action.Body, form),
		Back:       back,
		Error:      errMsg,
	})
}

type loginPage struct {
	pageHeader
	Next, Error, APIBase string
	// Credential reports whether the application supplied a sign-in hook, and
	// CredentialLabel names its first field. Both are zero when it did not,
	// and the template then renders the token form alone.
	Credential      bool
	CredentialLabel string
}

// loginView is the page as it should render for this request, so the two
// handlers cannot disagree about which forms are on it.
func (s *Server) loginView(r *http.Request, next, errMsg string) loginPage {
	return loginPage{
		pageHeader:      s.header(r),
		Next:            next,
		Error:           errMsg,
		APIBase:         s.apiBase,
		Credential:      s.credential.configured(),
		CredentialLabel: s.credential.label(),
	}
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, s.login, s.loginView(r, r.URL.Query().Get("next"), ""))
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	next := r.PostForm.Get("next")

	// The credential form when the application supplied a hook and this
	// submission came from it. Keyed on the identifier being present rather
	// than on the hook existing, because both forms post here and a pasted
	// token must still work when a hook is configured.
	if id := r.PostForm.Get("identifier"); id != "" || r.PostForm.Get("secret") != "" {
		s.submitCredential(w, r, next)
		return
	}

	token := r.PostForm.Get("token")
	if token == "" {
		s.render(w, s.login, s.loginView(r, next, "a token is required"))
		return
	}
	s.signIn(w, r, token, next)
}

// submitCredential exchanges the application's credentials for a token.
func (s *Server) submitCredential(w http.ResponseWriter, r *http.Request, next string) {
	if !s.credential.configured() {
		// Posted credentials with no hook to answer them: the form that sent
		// these was not rendered by this server.
		s.render(w, s.login, s.loginView(r, next, "this studio does not accept a credential sign-in"))
		return
	}
	id := r.PostForm.Get("identifier")
	secret := r.PostForm.Get("secret")
	if id == "" || secret == "" {
		s.render(w, s.login, s.loginView(r, next, s.credential.label()+" and password are both required"))
		return
	}

	token, err := s.credential.Exchange(r.Context(), id, secret)
	// One message for a refusal and for a failure, and the hook's own error is
	// never rendered. This page is reachable without a token, so anything shown
	// here is readable by anyone who can reach the URL — and telling a visitor
	// which half was wrong is an enumeration oracle. See CredentialLogin.
	if err != nil || token == "" {
		s.render(w, s.login, s.loginView(r, next, "those credentials were not accepted"))
		return
	}
	s.signIn(w, r, token, next)
}

// signIn is the one place a token becomes a session, whether it was pasted or
// exchanged — so the cookie's shape cannot differ between the two paths.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request, token, next string) {
	s.setTokenCookie(w, token)
	if next == "" {
		next = s.url("/")
	}
	http.Redirect(w, r, next, http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearTokenCookie(w)
	http.Redirect(w, r, s.url("/"), http.StatusFound)
}

// clientFor returns an apiClient using the caller's own cookie-stored token,
// or redirects to /login and reports ok=false when there isn't one.
func (s *Server) clientFor(w http.ResponseWriter, r *http.Request) (*apiClient, bool) {
	token := tokenFromRequest(r)
	if token == "" {
		http.Redirect(w, r, s.url("/login")+"?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return nil, false
	}
	return newAPIClient(s.apiBase, token), true
}

// renderAPIError sends a stale token back through login, and reports every
// other API error as a gateway failure with the response body attached — an
// operator staring at a 403 needs to see that, not a generic 500.
func (s *Server) renderAPIError(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		if ae.Status == http.StatusUnauthorized {
			s.clearTokenCookie(w)
			http.Redirect(w, r, s.url("/login")+"?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		http.Error(w, fmt.Sprintf("%d from API: %s", ae.Status, ae.Body), http.StatusBadGateway)
		return
	}
	http.Error(w, err.Error(), http.StatusBadGateway)
}

func (s *Server) render(w http.ResponseWriter, t *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "base", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
