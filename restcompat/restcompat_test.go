package restcompat_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/restcompat"
	"github.com/mind-vm/sqlb/schema"
)

// opts parametrises the blog's posts table so a test can state a before and an
// after that differ in exactly one way. The zero value is the baseline blog
// contract; each field turns on one edit.
type opts struct {
	titleName          string    // rename target for title; "" keeps "title"
	titleNullable      bool      // NOT NULL -> nullable on title (reader break)
	statusUnfilter     bool      // drop Filterable from status (un-expose, no DDL)
	dropViewCount      bool      // drop a column (destructive migration)
	publishedNotNull   bool      // nullable -> NOT NULL on published_at (writer break)
	addSubtitle        bool      // add a nullable filterable column (additive)
	addRequiredSlug    bool      // add a NOT NULL no-default column (writer break)
	widenViewCount     bool      // bigint stays; flip to int to test narrowing
	titleReadOnly      bool      // writable -> ReadOnly on title (leaves both bodies)
	titleImmutable     bool      // writable -> Immutable on title (leaves the patch body)
	viewCountWritable  bool      // ReadOnly -> writable on view_count
	publishedNullsLast bool      // declare NULLS LAST on published_at (#88)
	statusValues       []string  // enum values; nil keeps the baseline three
	ops                schema.Op // 0 keeps the baseline op set
	authorUnique       bool      // Unique() on posts.author: its Inverse "posts" becomes one-to-one
	postsNotExpandable bool      // Inverse("posts") with no InverseExpandable: a name, not a contract
	noPostsInverse     bool      // no Inverse("posts") at all: the forward Ref names no reverse relation

	// wire declares the schema's wire spelling. The zero value is Verbatim,
	// which is also what it means, so the baseline blog needs no case at all.
	wire schema.WireCase
}

const baseOps = schema.OpCreate | schema.OpRead | schema.OpUpdate | schema.OpList

// blog builds a registry holding the blog's posts table and an authors table
// for the reference to point at, edited per o. authors is exposed and its Ref
// from posts is InverseExpandable — GET /authors?expand=posts — so that
// o.authorUnique has a REST contract to change the shape of; an unexposed
// reverse relation is invisible to a client and so has nothing to break.
func blog(o opts) *schema.Registry {
	r := schema.NewRegistry().WireCase(o.wire)

	authors := r.Table("authors", schema.UUIDv7("id").PrimaryKey()).
		Expose(schema.REST{Ops: schema.OpRead | schema.OpList})

	author := schema.Ref("author", authors).Filterable().Expandable()
	if !o.noPostsInverse {
		author = author.Inverse("posts")
		if !o.postsNotExpandable {
			author = author.InverseExpandable()
		}
	}
	if o.authorUnique {
		author = author.Unique()
	}

	title := schema.Text(pick(o.titleName, "title")).Searchable().Sortable()
	if o.titleName != "" {
		title = title.RenamedFrom("title")
	}
	if o.titleNullable {
		title = title.Nullable()
	}
	if o.titleReadOnly {
		title = title.ReadOnly()
	}
	if o.titleImmutable {
		title = title.Immutable()
	}

	status := schema.Enum("status", pickVals(o.statusValues, "draft", "review", "published")...).
		Default(schema.Value("draft")).Sortable()
	if !o.statusUnfilter {
		status = status.Filterable()
	}

	viewType := schema.BigInt
	if o.widenViewCount {
		viewType = schema.Int
	}

	published := schema.Timestamp("published_at").Filterable().Sortable()
	if o.publishedNullsLast {
		published = schema.Timestamp("published_at").Filterable().Sortable(schema.NullsLast)
	}
	if !o.publishedNotNull {
		published = published.Nullable()
	}

	fields := []schema.FieldSpec{
		schema.UUIDv7("id").PrimaryKey(),
		author,
		title,
		schema.Text("body").Searchable(),
		status,
		published,
	}
	if !o.dropViewCount {
		views := viewType("view_count").Filterable().Sortable()
		if !o.viewCountWritable {
			views = views.Default(schema.Value(0)).ReadOnly()
		}
		// When writable it also loses its default, so it arrives *required* —
		// the half of "no longer read-only" that breaks a client rather than
		// the half that does not.
		fields = append(fields, views)
	}
	if o.addSubtitle {
		fields = append(fields, schema.Text("subtitle").Nullable().Filterable())
	}
	if o.addRequiredSlug {
		fields = append(fields, schema.Text("slug"))
	}
	fields = append(fields, schema.Timestamps())

	r.Table("posts", fields...).
		Expose(schema.REST{Path: "/posts", Ops: pickOps(o.ops, baseOps)})
	return r
}

func TestNoChangeIsEmpty(t *testing.T) {
	if got := restcompat.Diff(blog(opts{}), blog(opts{})); len(got) != 0 {
		t.Fatalf("identical schemas should diff empty, got:\n%s", render(got))
	}
}

// A rename is a clean migration and a hard wire break — the case that proves the
// API check is not a by-product of the migration check (ADR-0039).
func TestRenameIsAWireBreak(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}), blog(opts{titleName: "headline"}))
	assertBreaking(t, breaks, restcompat.FacetResponse, "headline")
	assertBreaking(t, breaks, restcompat.FacetFilter, "headline")
	if !mentions(breaks, "renamed from") {
		t.Errorf("rename should be reported as a rename, not a drop and add:\n%s", render(breaks))
	}
	assertNoAdditive(t, breaks) // it is a rename, not a new field
}

// A unique FK's Inverse changing shape from a collection envelope to a
// nullable object is a response-facet break, the same category a rename is —
// a client reading `.items` off it would break, just as one reading a
// renamed field would. docs/compatibility.md carves this out of the Frozen
// list-envelope entry (ADR-0040's precedent: a Frozen guarantee broken once
// before, deliberately, pre-1.0).
func TestUniqueFKChangesInverseFromCollectionToObject(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}), blog(opts{authorUnique: true}))
	assertBreaking(t, breaks, restcompat.FacetExpand, "posts")
	if !mentions(breaks, "one-to-one") {
		t.Errorf("the summary should say why it breaks:\n%s", render(breaks))
	}
}

// The other direction of the same flip — object back to collection — gets its
// own test rather than trusting the branch inside diffField symmetrically, the
// convention TestWireCaseFlipBreaksInBothDirections already keeps for this
// file's other two-way comparisons.
func TestUniqueFKRemovedRevertsInverseToCollection(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{authorUnique: true}), blog(opts{}))
	assertBreaking(t, breaks, restcompat.FacetExpand, "posts")
	if !mentions(breaks, "no longer a unique FK") {
		t.Errorf("the summary should say why it breaks:\n%s", render(breaks))
	}
}

// Guard-proven-both-ways (ADR-0016) companion to the two tests above: a bare
// Inverse("posts") with no InverseExpandable never emits a Go field, never
// exposes an ?expand parameter and never changes the wire, so it must not be
// part of the captured contract at all.
//
// Removing the Inverse name entirely is the case that actually catches an
// unfiltered Capture: dropping it makes "posts" disappear from authors'
// captured fields, and if a non-expandable one had been recorded as present
// (hidden=false, writeOnly=false — indistinguishable from a real response
// field to diffRemoved's inResponse() check), this would report a false
// "field removed from responses" for a field the generated response never
// had.
func TestNonExpandableInverseProducesNoFinding(t *testing.T) {
	breaks := restcompat.Diff(
		blog(opts{postsNotExpandable: true}),
		blog(opts{noPostsInverse: true}),
	)
	if len(breaks) != 0 {
		t.Errorf("a non-expandable inverse is not part of the contract, want no findings:\n%s", render(breaks))
	}
}

// Un-exposing a filter changes the contract while emitting no DDL at all — the
// break a migration-shaped check cannot see.
func TestUnExposeFilterBreaksWithNoDDL(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}), blog(opts{statusUnfilter: true}))
	assertBreaking(t, breaks, restcompat.FacetFilter, "status")
	// status stays sortable and in responses, so nothing else should fire.
	if n := len(restcompat.Breaking(breaks)); n != 1 {
		t.Errorf("want exactly one breaking change, got %d:\n%s", n, render(breaks))
	}
}

func TestDropOperationBreaks(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}), blog(opts{ops: baseOps &^ schema.OpRead}))
	assertBreaking(t, breaks, restcompat.FacetOps, "")
	if !mentions(breaks, "operation read removed") {
		t.Errorf("want the removed read operation named:\n%s", render(breaks))
	}
}

func TestDropColumnBreaksEveryCapability(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}), blog(opts{dropViewCount: true}))
	assertBreaking(t, breaks, restcompat.FacetResponse, "view_count")
	assertBreaking(t, breaks, restcompat.FacetFilter, "view_count")
	assertBreaking(t, breaks, restcompat.FacetSort, "view_count")
}

// The additive baseline: a nullable, filterable column breaks nobody.
func TestAddNullableFilterableIsAdditive(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}), blog(opts{addSubtitle: true}))
	if got := restcompat.Breaking(breaks); len(got) != 0 {
		t.Fatalf("adding a nullable filterable column should break nobody, got:\n%s", render(got))
	}
	assertLevel(t, breaks, restcompat.LevelAdditive, restcompat.FacetFilter, "subtitle")
	assertLevel(t, breaks, restcompat.LevelAdditive, restcompat.FacetResponse, "subtitle")
}

// nullable -> NOT NULL: neutral for readers, breaking for writers. The both-ways
// case ADR-0016 says must be reported on both sides, never folded.
func TestNullableToNotNullSplitsReaderAndWriter(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}), blog(opts{publishedNotNull: true}))
	assertLevel(t, breaks, restcompat.LevelNeutral, restcompat.FacetResponse, "published_at")
	assertLevel(t, breaks, restcompat.LevelBreaking, restcompat.FacetCreate, "published_at")
}

// not null -> nullable: breaking for readers, additive for writers.
func TestNotNullToNullableBreaksReaders(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}), blog(opts{titleNullable: true}))
	assertLevel(t, breaks, restcompat.LevelBreaking, restcompat.FacetResponse, "title")
	assertLevel(t, breaks, restcompat.LevelAdditive, restcompat.FacetCreate, "title")
}

func TestNewRequiredFieldBreaksCreate(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}), blog(opts{addRequiredSlug: true}))
	assertBreaking(t, breaks, restcompat.FacetCreate, "slug")
}

// The writer side of the contract, which the snapshot captured and the diff
// never read: three breaks that let `sqlb impact -error` pass CI on a breaking
// deploy (#68). The reader side of diffField was thorough throughout, and no
// existing test touched a body-only capability, which is what let it ship.

// writable -> ReadOnly: the column leaves both generated bodies, so a client
// that sends it now 422s with "unknown field".
func TestBecomingReadOnlyBreaksBothBodies(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}), blog(opts{titleReadOnly: true}))
	assertLevel(t, breaks, restcompat.LevelBreaking, restcompat.FacetCreate, "title")
	assertLevel(t, breaks, restcompat.LevelBreaking, restcompat.FacetPatch, "title")
	if !mentions(breaks, "422") {
		t.Errorf("the summary should name the client-visible consequence:\n%s", render(breaks))
	}
}

// writable -> Immutable: it leaves the PATCH body only. Create is unaffected,
// which is the whole distinction between Immutable and ReadOnly.
func TestBecomingImmutableBreaksThePatchBodyOnly(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}), blog(opts{titleImmutable: true}))
	assertLevel(t, breaks, restcompat.LevelBreaking, restcompat.FacetPatch, "title")
	for _, b := range breaks {
		if b.Facet == restcompat.FacetCreate {
			t.Errorf("Immutable must not touch the create body:\n%s", render(breaks))
		}
	}
}

// ReadOnly -> writable on a NOT NULL, no-default column: it becomes *required*
// at create, so a client that omitted it — which is every client, since it could
// not send it before — now fails validation.
func TestLeavingReadOnlyWithoutADefaultBreaksCreate(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}), blog(opts{viewCountWritable: true}))
	assertLevel(t, breaks, restcompat.LevelBreaking, restcompat.FacetCreate, "view_count")
	if !mentions(breaks, "required") {
		t.Errorf("the summary should say why it breaks:\n%s", render(breaks))
	}
	// And the patch body gained it, which breaks nobody.
	assertLevel(t, breaks, restcompat.LevelAdditive, restcompat.FacetPatch, "view_count")
}

// Widening an integer is not claimed neutral: a narrow generated client can
// overflow, so it surfaces as unknown (which a strict gate treats as breaking).
func TestWidenIntegerIsUnknownNotNeutral(t *testing.T) {
	// old = int, new = bigint (widen). blog()'s baseline is bigint, so flip the
	// direction: old widens view_count to int, new keeps bigint.
	breaks := restcompat.Diff(blog(opts{widenViewCount: true}), blog(opts{}))
	assertLevel(t, breaks, restcompat.LevelUnknown, restcompat.FacetResponse, "view_count")
	if len(restcompat.Breaking(breaks)) == 0 {
		t.Errorf("an unknown type change should count under a strict gate:\n%s", render(breaks))
	}
}

func TestEnumGainedValueBreaksReaders(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}),
		blog(opts{statusValues: []string{"draft", "review", "published", "archived"}}))
	assertBreaking(t, breaks, restcompat.FacetResponse, "status")
	if !mentions(breaks, "archived") {
		t.Errorf("the new enum value should be named:\n%s", render(breaks))
	}
}

func TestEnumDroppedValueBreaksInput(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}),
		blog(opts{statusValues: []string{"draft", "published"}}))
	assertBreaking(t, breaks, restcompat.FacetFilter, "status")
	if !mentions(breaks, "review") {
		t.Errorf("the dropped enum value should be named:\n%s", render(breaks))
	}
}

// --- assertions ---------------------------------------------------------------

func assertBreaking(t *testing.T, breaks []restcompat.Break, facet restcompat.Facet, field string) {
	t.Helper()
	assertLevel(t, breaks, restcompat.LevelBreaking, facet, field)
}

func assertLevel(t *testing.T, breaks []restcompat.Break, lvl restcompat.Level, facet restcompat.Facet, field string) {
	t.Helper()
	for _, b := range breaks {
		if b.Level == lvl && b.Facet == facet && b.Field == field {
			return
		}
	}
	t.Errorf("missing %s break on %s %q, got:\n%s", lvl, facet, field, render(breaks))
}

func assertNoAdditive(t *testing.T, breaks []restcompat.Break) {
	t.Helper()
	for _, b := range breaks {
		if b.Level == restcompat.LevelAdditive {
			t.Errorf("unexpected additive break:\n%s", render(breaks))
			return
		}
	}
}

func mentions(breaks []restcompat.Break, substr string) bool {
	for _, b := range breaks {
		if strings.Contains(b.Summary, substr) {
			return true
		}
	}
	return false
}

func render(breaks []restcompat.Break) string {
	var b strings.Builder
	for _, br := range breaks {
		b.WriteString("  " + br.String() + "\n")
	}
	if b.Len() == 0 {
		return "  (none)"
	}
	return b.String()
}

func pick(s, dflt string) string {
	if s == "" {
		return dflt
	}
	return s
}

func pickVals(v []string, dflt ...string) []string {
	if v == nil {
		return dflt
	}
	return v
}

func pickOps(o, dflt schema.Op) schema.Op {
	if o == 0 {
		return dflt
	}
	return o
}

// A null placement change removes no parameter and rejects no request: every
// ?sort= that worked still works, and answers in a different order. That is the
// shape the capability diff is blind to by construction — it compares whether
// the sort key exists, and the key exists on both sides (#88).
func TestNullPlacementChangeIsABreakTheCapabilityDiffCannotSee(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}), blog(opts{publishedNullsLast: true}))
	assertBreaking(t, breaks, restcompat.FacetSort, "published_at")
	if !mentions(breaks, "null placement changed") {
		t.Errorf("want the placement change named:\n%s", render(breaks))
	}
	// The two consequences a caller acts on: the order, and the cursors.
	if !mentions(breaks, "different order") || !mentions(breaks, "cursors") {
		t.Errorf("the break does not say what it costs a deployed client:\n%s", render(breaks))
	}
	if n := len(restcompat.Breaking(breaks)); n != 1 {
		t.Errorf("want exactly one breaking change, got %d:\n%s", n, render(breaks))
	}
}

// The placement is not a capability, so declaring one must not read as adding
// or removing a sort key. A column that was sortable before and after is not
// newly sortable.
func TestNullPlacementChangeIsNotReportedAsACapabilityDelta(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}), blog(opts{publishedNullsLast: true}))
	if mentions(breaks, "sort key added") || mentions(breaks, "sort key removed") {
		t.Errorf("a placement change was reported as a capability change:\n%s", render(breaks))
	}
}

// A WireCase flip renames every field on the wire at once while renaming no
// column, so the field-level comparison — which matches columns by column name
// — sees nothing. This is the wire break with the widest blast radius and the
// emptiest migration, and it is exactly a rename (ADR-0036's amendment, and
// compatibility.md's Frozen entry) at schema scale.
func TestWireCaseFlipIsAWireBreak(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}), blog(opts{wire: schema.Camel}))
	assertBreaking(t, breaks, restcompat.FacetWire, "")

	// Both spellings named, because "the wire case changed" is not actionable
	// without them, and one column that actually moves.
	if !mentions(breaks, "verbatim") || !mentions(breaks, "camel") {
		t.Errorf("the break should name both spellings:\n%s", render(breaks))
	}
	// The first column whose spelling actually moves: id is spelled the same in
	// both cases and is not the example a reader learns anything from.
	if !mentions(breaks, "author_id is now authorId") {
		t.Errorf("the break should show a column that moves:\n%s", render(breaks))
	}
}

// One finding, not one per column. Nine columns respelled is one edit, and nine
// lines would bury it.
func TestWireCaseFlipIsOneFindingNotOnePerColumn(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{}), blog(opts{wire: schema.Camel}))
	if n := len(breaks); n != 1 {
		t.Errorf("want exactly one finding for a wire case flip, got %d:\n%s", n, render(breaks))
	}
	if mentions(breaks, "renamed from") {
		t.Errorf("no column was renamed; only the spelling of all of them was:\n%s", render(breaks))
	}
}

// Back the other way, so the guard is not a one-directional check that happens
// to catch the direction the test was written for.
func TestWireCaseFlipBreaksInBothDirections(t *testing.T) {
	breaks := restcompat.Diff(blog(opts{wire: schema.Camel}), blog(opts{}))
	assertBreaking(t, breaks, restcompat.FacetWire, "")
	if !mentions(breaks, "from camel to verbatim") {
		t.Errorf("the break should read in the direction it happened:\n%s", render(breaks))
	}
}

// Declaring a case is not itself a break: a schema that was camel and stayed
// camel changed nothing, and every column comparison under it still works,
// because the snapshot records columns by column name in either case.
func TestUnchangedWireCaseIsNotABreak(t *testing.T) {
	if got := restcompat.Diff(blog(opts{wire: schema.Camel}), blog(opts{wire: schema.Camel})); len(got) != 0 {
		t.Fatalf("a camel schema compared with itself should diff empty, got:\n%s", render(got))
	}
}

// The compatibility constraint on the snapshot format itself: a Verbatim schema
// records no wire case at all, so every restcontract.json committed before this
// field existed stays byte-identical and no baseline needs re-recording. The
// second half holds the field to being present when there is something to say,
// so the first half cannot be satisfied by never writing it.
func TestVerbatimSnapshotIsUnchangedByTheWireCaseField(t *testing.T) {
	verbatim, err := json.MarshalIndent(restcompat.Capture(blog(opts{})), "", "  ")
	if err != nil {
		t.Fatalf("capture is not encodable: %v", err)
	}
	if strings.Contains(string(verbatim), "wire_case") {
		t.Errorf("a Verbatim snapshot must record no wire case, or every committed baseline moves:\n%s", verbatim)
	}

	camel, err := json.MarshalIndent(restcompat.Capture(blog(opts{wire: schema.Camel})), "", "  ")
	if err != nil {
		t.Fatalf("capture is not encodable: %v", err)
	}
	if !strings.Contains(string(camel), `"wire_case": "camel"`) {
		t.Errorf("a Camel snapshot must record its wire case:\n%s", camel)
	}
}
