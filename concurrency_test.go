package sqlb_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
)

// sqlb sits between an HTTP handler and the database, so everything it holds
// across requests — the model cache, a hook registry, a relation's lazily
// resolved target — is read from every goroutine serving one. These tests are
// the ones that say so; run under -race they are the only thing in the suite
// that exercises that at all.
//
// The models are private to this file. A model is process-global once built, so
// a test that shares one with another test is a test whose caches were already
// warm when it started, which is the opposite of what these want.

type concOrg struct {
	ID   int64  `db:"id" json:"id" sqlb:"pk"`
	Name string `db:"name" json:"name" sqlb:"filter,search"`

	Members *sqlb.Collection[concMember] `db:"-" json:"members,omitempty" sqlb:"expands=org_id,order=-created_at,limit=10"`
}

func (concOrg) TableName() string { return "conc_orgs" }

type concMember struct {
	ID        int64     `db:"id" json:"id" sqlb:"pk"`
	OrgID     int64     `db:"org_id" json:"org_id" sqlb:"filter,expand"`
	Email     string    `db:"email" json:"email" sqlb:"filter,search"`
	CreatedAt time.Time `db:"created_at" json:"created_at" sqlb:"sort,default"`

	Org *concOrg `db:"-" json:"org,omitempty" sqlb:"expands=org_id"`
}

func (concMember) TableName() string { return "conc_members" }

// A cold process takes its first burst of requests concurrently, and that burst
// is what builds the models, resolves the relations between them and fires each
// RelationInfo's sync.Once. Both directions of the expansion are exercised at
// once, so the two models resolve each other while both are being built.
func TestConcurrentColdStartResolvesModelsSafely(t *testing.T) {
	h := newHarness(t, []string{"id", "org_id", "email", "created_at"}, nil)
	reg := sqlb.NewRegistry()
	sqlb.On[concMember](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[concMember]) error {
		q.Where(sqlb.F("org_id").Eq(7))
		return nil
	})
	sqlb.On[concOrg](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[concOrg]) error {
		q.Where(sqlb.F("id").Eq(7))
		return nil
	})
	db := h.handle(reg)

	const workers = 64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx := context.Background()
			if i%2 == 0 {
				q := sqlb.Query[concMember]().Expand("org").Where(sqlb.F("email").Eq("a@b.c"))
				sql, _, err := q.SQL()
				if err != nil {
					t.Errorf("member SQL: %v", err)
					return
				}
				if !strings.Contains(sql, `LEFT JOIN "conc_orgs"`) {
					t.Errorf("member statement lost its join:\n%s", sql)
				}
				if _, err := q.All(ctx, db); err != nil {
					t.Errorf("member All: %v", err)
				}
			} else {
				q := sqlb.Query[concOrg]().Expand("members")
				sql, _, err := q.SQL()
				if err != nil {
					t.Errorf("org SQL: %v", err)
					return
				}
				if !strings.Contains(sql, `FROM "conc_members"`) {
					t.Errorf("org statement lost its collection:\n%s", sql)
				}
				if _, err := q.All(ctx, db); err != nil {
					t.Errorf("org All: %v", err)
				}
			}
		}()
	}
	close(start)
	wg.Wait()
}

// Registration is documented as a startup step, but nothing stops a program
// doing it late — reloading rules, admitting a tenant — and the hook slices are
// read on the build path of every statement. A reader takes the slice under the
// lock and iterates it after releasing, so a concurrent append is invisible to
// it rather than corrupting it.
func TestConcurrentHookRegistrationDuringQueries(t *testing.T) {
	h := newHarness(t, []string{"id", "org_id", "email", "created_at"}, nil)
	reg := sqlb.NewRegistry()
	db := h.handle(reg)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := sqlb.Query[concMember]().All(context.Background(), db); err != nil {
					t.Errorf("All: %v", err)
					return
				}
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				sqlb.On[concMember](reg).BeforeQuery(func(context.Context, *sqlb.Builder[concMember]) error {
					return nil
				})
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// Describe is copy-on-write, and this is the property that makes it so rather
// than merely unlikely to be caught: a model already handed out is never
// written again. It needs no race detector to fail — a description that wrote
// through the shared pointer would change `before` too.
type concFrozen struct {
	ID     int64  `db:"id"`
	Name   string `db:"name"`
	Secret string `db:"secret"`
}

func (concFrozen) TableName() string { return "conc_frozen" }

func TestDescribeDoesNotWriteAModelAlreadyHandedOut(t *testing.T) {
	before := sqlb.ModelOf[concFrozen]()
	if before.Column("secret").Hidden {
		t.Fatal("secret starts visible")
	}

	sqlb.Describe[concFrozen]().Table("renamed").PrimaryKey("id").Hidden("secret")

	if before.Table != "conc_frozen" {
		t.Errorf("the handed-out model was renamed to %q", before.Table)
	}
	if before.Column("secret").Hidden {
		t.Error("the handed-out model had a column hidden underneath it")
	}
	if before.PK != nil {
		t.Errorf("the handed-out model gained a primary key %q", before.PK.Name)
	}

	after := sqlb.ModelOf[concFrozen]()
	if after == before {
		t.Fatal("describing published the same model rather than a copy")
	}
	if after.Table != "renamed" {
		t.Errorf("Table = %q, want renamed", after.Table)
	}
	if !after.Column("secret").Hidden {
		t.Error("the description did not take effect")
	}
	if after.PK == nil || after.PK.Name != "id" {
		t.Errorf("PK = %+v, want id", after.PK)
	}
}

// A relation survives the copy, with its foreign key rebased onto the copied
// column rather than left pointing at the model it came from. Renaming the key
// afterwards is the case that tells the two apart: rebasing by pointer identity
// follows the column, rebasing by name would lose it.
type concRelChild struct {
	ID       int64 `db:"id"`
	ParentID int64 `db:"parent_id"`
}

func (concRelChild) TableName() string { return "conc_rel_children" }

type concRelParent struct {
	ID       int64  `db:"id"`
	OwnerRef int64  `db:"owner_ref"`
	Name     string `db:"name"`

	Owner *concRelChild `db:"-"`
}

func (concRelParent) TableName() string { return "conc_rel_parents" }

func TestDescribeCarriesRelationsAcrossTheCopy(t *testing.T) {
	sqlb.Describe[concRelChild]().PrimaryKey("id")
	// The relation is declared first, then three more descriptions copy the
	// model out from under it, then its column is renamed.
	sqlb.Describe[concRelParent]().
		PrimaryKey("id").
		Relation("Owner", "owner_ref").
		Filterable("name").
		Sortable("name").
		Column("OwnerRef", "owner_id")

	m := sqlb.ModelOf[concRelParent]()
	if got := len(m.Relations); got != 1 {
		t.Fatalf("relations = %d, want 1", got)
	}
	if col := m.Column("owner_id"); col == nil || !col.Expandable {
		t.Fatalf("owner_id should be expandable after the rename: %+v", col)
	}

	sql, _, err := sqlb.Query[concRelParent]().Expand("owner").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	// The join follows the renamed column, which is what says the relation was
	// rebased onto the copy rather than left holding the original's.
	if want := `"conc_rel_parents"."owner_id"`; !strings.Contains(sql, want) {
		t.Errorf("statement missing %q:\n%s", want, sql)
	}
}

// Describing while queries run is a contract violation that Describe panics on
// — but the panic is checked when the Description is built and the writes
// happen in the calls after it, so the window is real. Copy-on-write is what
// makes falling into it safe rather than merely unlucky. Under -race this fails
// outright if a description ever writes a published model; without it, the
// consistency check is what would catch a torn one.
type concWindow struct {
	ID   int64  `db:"id"`
	Name string `db:"name"`
	Memo string `db:"memo"`
}

func (concWindow) TableName() string { return "conc_windows" }

func TestDescribeDuringConcurrentQueriesStaysConsistent(t *testing.T) {
	d := sqlb.Describe[concWindow]() // the guard passes: nothing has queried yet

	var wg sync.WaitGroup
	start := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		d.Table("renamed").PrimaryKey("id").Filterable("name").Hidden("memo")
	}()

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 200 {
				q := sqlb.Query[concWindow]().Where(sqlb.F("name").Eq("x"))
				m := q.Model()
				// Whatever description this statement caught, the model it
				// caught it in has to be internally consistent: every column
				// reachable by its own name, and a primary key that is one of
				// the model's own columns rather than a copy's.
				for _, col := range m.Columns {
					if m.Column(col.Name) != col {
						t.Errorf("column %q is not reachable by name in its own model", col.Name)
						return
					}
				}
				if m.PK != nil && m.Column(m.PK.Name) != m.PK {
					t.Errorf("primary key %q belongs to a different model", m.PK.Name)
					return
				}
				if _, _, err := q.SQL(); err != nil {
					t.Errorf("SQL: %v", err)
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()
}
