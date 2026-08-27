// Package sessionkit is a library that ships tables, written the way this
// repository recommends: it contributes a *declaration* the host composes, and
// owns no migration sequence of its own.
//
// Nothing here imports the application. The application imports this, hands it
// its registry and — optionally — the table its sessions should point at, and
// gets back a handle to what was declared. One registry, one `sqlb generate`,
// one migration sequence, one drift check, and foreign keys that cross the
// boundary because there is no boundary left for them to cross.
//
// # Why not own the migrations
//
// The instinct a library author has first is to ship goose files. It is the one
// answer that is always wrong, and the failure is not aesthetic: a library with
// its own sequence has its own tracking table, so its tables are created in an
// order nothing coordinates with the host's. A foreign key across that line
// then works only when the referenced table happens to be created first, and
// the failure lands at deploy rather than at compile. Declaring into the host's
// registry puts every ALTER TABLE after every CREATE TABLE in one generated
// migration, and ordering stops being a question anyone has to hold.
//
// # The two halves, and why neither needs codegen
//
// [Declare] is the declaration half: DDL, capabilities, and the confinement
// obligation. [Session] is the runtime half — a plain struct with `db` and
// `sqlb` tags, which the engine reflects over, so this library runs its own
// queries without generating anything and without the host generating anything
// for it. The host's own `sqlb generate` will emit a model for this table too;
// the two coexist because the model cache is keyed by Go type, and
// libtables_test.go asserts exactly that.
//
// # The table name is a constant, not an option
//
// A prefix keeps `sessions` from colliding with the host's own, and it is fixed
// here rather than passed in. The reason is [Session.TableName]: a model names
// its table statically, so a host-configurable prefix would have to be applied
// with sqlb.Describe at startup — which must run before any statement does, and
// fails at runtime rather than at compile when it does not. A rename is
// available to a host that genuinely needs one (adopting a database that
// already has this table under another name); it is not the default, because
// the default should not cost every consumer an initialisation-order rule.
package sessionkit

import (
	"context"
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/schema"
)

// TablePrefix namespaces every table this library declares. Fixed, so that
// [Session.TableName] can be.
const TablePrefix = "sessionkit_"

// Options is what the host supplies that this library cannot know.
type Options struct {
	// Users is the host's user table, if it has one. Supplied, the session's
	// user_id becomes a real foreign key with ON DELETE CASCADE, so deleting a
	// user takes their sessions with them. Omitted — a host with no user table,
	// or a test — the same column is declared as a plain UUID and the library
	// still works.
	//
	// This is the whole reason Declare takes options rather than being a
	// package-level var: a reference is only real when the target is in the
	// same registry, and whether it is cannot be decided here.
	Users *schema.TableDef

	// Scope names the column sessions are confined by, when the host is
	// multi-tenant and wants this library's rows confined the same way its own
	// are. Empty means the sessions are confined by user_id alone.
	//
	// Whichever it is, the confinement is declared and therefore obligatory:
	// see [Session].
	Scope string
}

// Tables is what Declare hands back, so the host can point its own references
// at these tables and register hooks against them.
type Tables struct {
	Sessions *schema.TableDef
}

// Declare adds this library's tables to reg and returns them.
//
// Call it from the host's schema package, beside its own declarations:
//
//	var Registry = schema.NewRegistry()
//	var User = Registry.Table("users", …)
//	var Session = sessionkit.Declare(Registry, sessionkit.Options{Users: User}).Sessions
//
// Declaring into the host's registry rather than one of this library's own is
// the entire convention. Everything downstream — generate, migrate, drift,
// the manifest — reads one registry, so a library with its own would be
// invisible to all of it.
func Declare(reg *schema.Registry, opts Options) Tables {
	sessions := reg.Table(TablePrefix+"sessions",
		schema.UUIDv7("id").PrimaryKey(),

		// The optional reference, in one code path rather than two. Both
		// constructors return *schema.Field, so choosing between them is a
		// function call and not a fork in the declaration.
		userRef(opts.Users),

		schema.Text("token_hash").Unique(),
		schema.Timestamp("expires_at").Filterable().Sortable(),
		schema.Timestamps(),
	).Describe("A signed-in session. Declared by sessionkit, owned by the host's registry.").
		// This package ships its own Session model (below), so the host's
		// codegen must not emit a second one. Hooks are keyed by Go type: a
		// confinement hook registered on sessionkit.Session would not fire for
		// a query written against a host-generated Session, and the host's
		// package is the one its author is already importing (#284).
		//
		// Declared here, by the library that knows, rather than by a list each
		// host maintains — that list is a second copy of this package's table
		// set, and goes wrong the release a table is added to it.
		ModelsIn("sessionkit")

	if opts.Scope != "" {
		sessions.AddField(schema.Text(opts.Scope).Filterable().ReadOnly().Scoped().Indexed())
	}
	return Tables{Sessions: sessions}
}

// userRef is the whole of "optional reference": a real foreign key when the
// host has a table to point at, and a bare column when it does not.
//
// The column is named the same either way, so nothing downstream — a hook, a
// filter, this package's own model — has to know which one it got.
func userRef(users *schema.TableDef) *schema.Field {
	if users == nil {
		return schema.UUID("user_id").Filterable().Indexed()
	}
	return schema.Ref("user", users).OnDelete(schema.Cascade).Filterable().Indexed()
}

// Session is this library's own model, and it is a plain struct.
//
// The engine reflects over `db` and `sqlb` tags, so a library needs no
// generated code to query its own tables — which matters here more than it does
// in an application, because the host owns codegen and this package cannot
// import its output.
//
// `sqlb:"scope"` on user_id is the load-bearing tag. It is the runtime form of
// the schema's Scoped, and it means a REST mount over this model refuses to
// start until a BeforeQuery hook confines it. A library declaring it is
// obliging its host to answer "whose sessions are these" before serving them,
// which is the right direction for the obligation to travel: the library knows
// the rows are confidential, and only the host knows what confines them.
type Session struct {
	ID        string    `db:"id" json:"id" sqlb:"pk,default"`
	UserID    string    `db:"user_id" json:"user_id" sqlb:"filter,scope"`
	TokenHash string    `db:"token_hash" json:"token_hash" sqlb:"filter"`
	ExpiresAt time.Time `db:"expires_at" json:"expires_at" sqlb:"filter,sort"`
	CreatedAt time.Time `db:"created_at" json:"created_at" sqlb:"sort,default"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at" sqlb:"sort,default"`
}

// TableName is static, which is what makes [TablePrefix] a constant.
func (Session) TableName() string { return TablePrefix + "sessions" }

// Live is a query this library runs against its own table.
//
// It takes an sqlb.Executor rather than owning a handle: the host decides
// whether the statement carries the hook registry, and passing the hooked
// handle is how this library's rows get confined by the host's rules —
// including rules written for tables this library has never heard of.
func Live(ctx context.Context, exec sqlb.Executor, now time.Time) ([]Session, error) {
	return sqlb.Query[Session]().
		Where(sqlb.F("expires_at").Gt(now)).
		OrderBy(sqlb.F("created_at").Desc()).
		All(ctx, exec)
}
