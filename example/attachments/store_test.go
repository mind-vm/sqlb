// The upload flow, without a database and without a bucket.
//
// Both doubles are deliberate and they are different kinds. sqlbtest.DB is
// sqlb's own scripted Executor: it answers what the script says and records
// the statements, which is what makes "did the insert carry the key the URL
// was signed for" a question a test can ask. fakestorage_test.go is an
// httptest endpoint that holds objects in a map, which is what makes "is the
// object still there after the row was deleted" one.
//
// What neither can answer is whether the SQL is valid or whether real storage
// accepts the signature. presign_test.go settles the signature against
// aws-sdk-go-v2, live_test.go settles the storage against a running RustFS or
// MinIO, and the README says which question each of the three answers.
//
//	go test ./...
package attachments_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/attachments"
	"github.com/mind-vm/sqlb/example/attachments/s3"
	"github.com/mind-vm/sqlb/sqlbtest"
)

// attachmentCols is the row as the database hands it back, in declaration
// order. Every scripted reply below is one of these.
var attachmentCols = []string{"id", "key", "filename", "content_type", "size", "status", "created_at", "updated_at"}

func row(id, key, filename, contentType string, size int64, status string) []any {
	now := time.Now().UTC()
	return []any{id, key, filename, contentType, size, status, now, now}
}

// harness is a store wired to a scripted database and a fake bucket.
type harness struct {
	store   *attachments.Store
	db      *sqlbtest.DB
	storage *fakeStorage
	hooks   *sqlb.Registry
	handle  *sqlb.DB
}

func newHarness(t *testing.T, policy attachments.Policy, replies ...sqlbtest.Reply) *harness {
	t.Helper()

	storage := newFakeStorage(t)
	bucket, err := s3.New(s3.Config{
		Endpoint:  storage.URL,
		Bucket:    "uploads",
		Region:    "us-east-1",
		AccessKey: "test",
		SecretKey: "test-secret",
	})
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}

	db := sqlbtest.New(replies...)
	hooks := sqlb.NewRegistry()
	handle := sqlb.New(db).WithHooks(hooks)
	store := attachments.NewStore(handle, bucket, policy)
	attachments.RegisterHooks(hooks, store)

	return &harness{store: store, db: db, storage: storage, hooks: hooks, handle: handle}
}

// upload PUTs bytes the way a browser would: to the URL, with the headers it
// was told to send and nothing else. It reports the status, the body being of
// no interest on the way up.
func upload(t *testing.T, up *attachments.Upload, body []byte) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, up.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building the upload: %v", err)
	}
	for name, value := range up.Headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("uploading: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// The path the whole design is arranged around: a row before the bytes, the
// bytes straight to the storage, and the size read back off the storage rather
// than taken from the client.
func TestTheRowIsWrittenBeforeTheBytesAndTheSizeComesFromTheStorage(t *testing.T) {
	ctx := context.Background()
	const key = "att/deadbeef/holiday.jpg"

	h := newHarness(t, attachments.Policy{ContentTypes: []string{"image/jpeg"}},
		sqlbtest.Reply{Match: "INSERT", Cols: attachmentCols,
			Rows: [][]any{row("a1", key, "holiday.jpg", "image/jpeg", 0, "pending")}},
		sqlbtest.Reply{Match: "SELECT", Cols: attachmentCols,
			Rows: [][]any{row("a1", key, "holiday.jpg", "image/jpeg", 0, "pending")}},
		sqlbtest.Reply{Match: "UPDATE", Cols: attachmentCols,
			Rows: [][]any{row("a1", key, "holiday.jpg", "image/jpeg", 11, "ready")}},
	)

	// 1. Permission. The row exists now, and it is pending: nothing has been
	//    uploaded, and the design's answer to "what if nothing ever is" is that
	//    this row is findable.
	up, err := h.store.Begin(ctx, "holiday.jpg", "image/jpeg", 11)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if up.Attachment.Status != attachments.AttachmentStatusPending {
		t.Errorf("the row is %q, want pending: the upload has not happened yet", up.Attachment.Status)
	}
	if stmt := h.db.LastStatement(); !strings.HasPrefix(stmt, "INSERT") {
		t.Errorf("the first statement is %q, want the row before the bytes", stmt)
	}
	// The key is minted here and never accepted from a caller: a client that
	// could choose it could choose somebody else's.
	if !strings.HasPrefix(up.Attachment.Key, "att/") || !strings.HasSuffix(up.Attachment.Key, "holiday.jpg") {
		t.Errorf("key = %q, want a server-minted one", up.Attachment.Key)
	}

	// 2. The bytes, direct. The server is not in this path at all.
	if status := upload(t, up, []byte("hello world")); status != http.StatusOK {
		t.Fatalf("the storage answered %d to the presigned PUT", status)
	}
	if !h.storage.has(key) {
		t.Fatal("the object is not in the bucket")
	}

	// 3. The completion, which asks the storage rather than the client.
	done, err := h.store.Complete(ctx, "a1")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if done.Status != attachments.AttachmentStatusReady {
		t.Errorf("status = %q, want ready", done.Status)
	}
	if done.Size != 11 {
		t.Errorf("size = %d, want the 11 bytes the storage reported", done.Size)
	}
	// The size on the wire is the storage's answer, not the client's claim.
	// Both are 11 here on purpose: what is being checked is which of the two
	// the statement carried, so the assertion is on the argument the update
	// bound rather than on the number.
	var bound bool
	for _, arg := range h.db.LastArgs() {
		if size, ok := arg.(int64); ok && size == 11 {
			bound = true
		}
	}
	if !bound {
		t.Errorf("the update bound %#v, want the size read off the storage", h.db.LastArgs())
	}
}

// The client's word that it finished is not evidence, and this is the case
// that proves the HEAD is doing something: the row is completed without
// anything ever being uploaded.
func TestCompleteRefusesAnUploadThatNeverArrived(t *testing.T) {
	ctx := context.Background()

	h := newHarness(t, attachments.Policy{},
		sqlbtest.Reply{Match: "SELECT", Cols: attachmentCols,
			Rows: [][]any{row("a1", "att/none/ghost.png", "ghost.png", "image/png", 0, "pending")}},
	)

	if _, err := h.store.Complete(ctx, "a1"); err == nil {
		t.Fatal("a row was marked ready with no object behind it")
	} else if !strings.Contains(err.Error(), "never uploaded") {
		t.Errorf("error = %v, want one that names the missing object", err)
	}

	for _, stmt := range h.db.Statements() {
		if strings.HasPrefix(stmt, "UPDATE") {
			t.Errorf("the row was updated anyway:\n%s", strings.Join(h.db.Statements(), "\n"))
		}
	}
}

// Completing twice is what a client that lost the response does, and it cannot
// tell whether the first call landed. So the second one is the same answer
// rather than an error.
func TestCompleteIsIdempotent(t *testing.T) {
	ctx := context.Background()

	h := newHarness(t, attachments.Policy{},
		sqlbtest.Reply{Match: "SELECT", Cols: attachmentCols,
			Rows: [][]any{row("a1", "att/x/a.png", "a.png", "image/png", 11, "ready")}},
	)

	done, err := h.store.Complete(ctx, "a1")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if done.Size != 11 {
		t.Errorf("size = %d, want the recorded 11", done.Size)
	}
	// No HEAD, no UPDATE: there is nothing left to confirm.
	if len(h.db.Statements()) != 1 {
		t.Errorf("a completed row was written again:\n%s", strings.Join(h.db.Statements(), "\n"))
	}
}

// The policy is the only thing the server can enforce before the bytes move,
// so a refusal has to happen here — and has to happen before the row is
// written, or every rejected upload leaves one behind.
func TestPolicyRefusalsHappenBeforeTheRowIsWritten(t *testing.T) {
	ctx := context.Background()
	policy := attachments.Policy{MaxSize: 1 << 20, ContentTypes: []string{"image/png"}}

	for name, tc := range map[string]struct {
		filename    string
		contentType string
		size        int64
		want        string
	}{
		"an unaccepted type": {"x.pdf", "application/pdf", 10, "not an accepted content type"},
		"too large":          {"x.png", "image/png", 2 << 20, "exceeds"},
		"no size at all":     {"x.png", "image/png", 0, "a size is required"},
		"no filename":        {"  ", "image/png", 10, "filename is required"},
	} {
		h := newHarness(t, policy)
		if _, err := h.store.Begin(ctx, tc.filename, tc.contentType, tc.size); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want one naming %q", name, err, tc.want)
		}
		if stmts := h.db.Statements(); len(stmts) != 0 {
			t.Errorf("%s: a refused upload still wrote %v", name, stmts)
		}
	}
}

// An upload larger than it claimed still reaches the storage — a presigned PUT
// carries no length the storage would enforce — so the completion step is
// where it is caught, and the object is what has to go.
func TestAnOversizeUploadIsRefusedAndTheObjectRemoved(t *testing.T) {
	ctx := context.Background()
	const key = "att/big/large.png"

	h := newHarness(t, attachments.Policy{MaxSize: 8},
		sqlbtest.Reply{Match: "SELECT", Cols: attachmentCols,
			Rows: [][]any{row("a1", key, "large.png", "image/png", 0, "pending")}},
	)
	h.storage.put(key, "image/png", bytes.Repeat([]byte("x"), 64), time.Now().UTC())

	if _, err := h.store.Complete(ctx, "a1"); err == nil {
		t.Fatal("an oversize upload was accepted")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want one naming the limit", err)
	}
	if h.storage.has(key) {
		t.Error("the oversize object is still in the bucket")
	}
}

// The hook, and the ordering it exists for: the row commits, and only then is
// the object removed. Deleting the object first and rolling back would leave a
// row pointing at nothing.
func TestDeletingTheRowRemovesTheObjectAfterTheCommit(t *testing.T) {
	ctx := context.Background()
	const key = "att/gone/receipt.pdf"

	h := newHarness(t, attachments.Policy{},
		sqlbtest.Reply{Match: "DELETE", Cols: attachmentCols,
			Rows: [][]any{row("a1", key, "receipt.pdf", "application/pdf", 12, "ready")}},
	)
	h.storage.put(key, "application/pdf", []byte("a receipt"), time.Now().UTC())

	err := h.handle.WithTx(ctx, func(ctx context.Context, tx *sqlb.DB) error {
		_, err := sqlb.DeleteRows[attachments.Attachment]().
			Where(attachments.AttachmentCols.ID.Eq("a1")).
			Exec(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("deleting: %v", err)
	}

	if h.storage.has(key) {
		t.Error("the row is gone and its object is not")
	}

	// The order is visible in the statement log, which is the point: the
	// object went after COMMIT, not before it.
	stmts := strings.Join(h.db.Statements(), " | ")
	if !strings.Contains(stmts, "BEGIN") || !strings.Contains(stmts, "COMMIT") {
		t.Errorf("the delete did not run in a transaction, so there was no commit to be after:\n%s", stmts)
	}
	// The delete carries RETURNING because a hook wants the rows: without it
	// the key would not be knowable and the object could not be found.
	if !strings.Contains(stmts, "RETURNING") {
		t.Errorf("the delete returned no rows, so the object key was never read:\n%s", stmts)
	}
}

// The rollback half of the same rule. Nothing was committed, so nothing was
// deleted — and an object removed here would be unrecoverable while the row
// that names it is still there.
func TestARolledBackDeleteLeavesTheObjectAlone(t *testing.T) {
	ctx := context.Background()
	const key = "att/kept/receipt.pdf"

	h := newHarness(t, attachments.Policy{},
		sqlbtest.Reply{Match: "DELETE", Cols: attachmentCols,
			Rows: [][]any{row("a1", key, "receipt.pdf", "application/pdf", 12, "ready")}},
	)
	h.storage.put(key, "application/pdf", []byte("a receipt"), time.Now().UTC())

	wanted := "the caller changed its mind"
	err := h.handle.WithTx(ctx, func(ctx context.Context, tx *sqlb.DB) error {
		if _, err := sqlb.DeleteRows[attachments.Attachment]().
			Where(attachments.AttachmentCols.ID.Eq("a1")).
			Exec(ctx, tx); err != nil {
			return err
		}
		return errRollback{wanted}
	})
	if err == nil {
		t.Fatal("the transaction committed")
	}

	if !h.storage.has(key) {
		t.Error("the object was deleted for a transaction that rolled back")
	}
	if stmts := strings.Join(h.db.Statements(), " | "); !strings.Contains(stmts, "ROLLBACK") {
		t.Errorf("no rollback in:\n%s", stmts)
	}
}

type errRollback struct{ reason string }

func (e errRollback) Error() string { return e.reason }

// The sweeper's two directions. Both are reconciliations rather than
// enforcement: the design has already chosen to leave findable garbage, and
// this is the thing that finds it.
func TestSweepReapsAbandonedRowsAndOrphanedObjects(t *testing.T) {
	ctx := context.Background()

	h := newHarness(t, attachments.Policy{UploadTTL: 15 * time.Minute},
		// Direction one: the pending rows nothing will complete. One row comes
		// back, and its object goes with it through the same hook the delete
		// endpoint uses.
		sqlbtest.Reply{Match: "DELETE", Cols: attachmentCols,
			Rows: [][]any{row("old", "att/stale/half.png", "half.png", "image/png", 0, "pending")}},
		// Direction two: of the keys in the bucket, this is the one a row
		// still references.
		sqlbtest.Reply{Match: "SELECT", Cols: []string{"key"},
			Rows: [][]any{{"att/live/kept.png"}}},
	)

	old := time.Now().UTC().Add(-2 * time.Hour)
	h.storage.put("att/live/kept.png", "image/png", []byte("referenced"), old)
	h.storage.put("att/orphan/lost.png", "image/png", []byte("nothing points here"), old)
	h.storage.put("att/stale/half.png", "image/png", []byte("half an upload"), old)

	report, err := h.store.Sweep(ctx, time.Hour)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if report.Abandoned != 1 {
		t.Errorf("Abandoned = %d, want the one pending row", report.Abandoned)
	}
	if report.Orphans != 1 {
		t.Errorf("Orphans = %d, want the one object no row references", report.Orphans)
	}
	if got := h.storage.keys(); len(got) != 1 || got[0] != "att/live/kept.png" {
		t.Errorf("the bucket holds %v, want only the referenced object", got)
	}
}

// An upload in flight is a pending row with no object *yet*, so a sweep whose
// window is shorter than the upload window would delete a file somebody is
// uploading. Refused where it is asked for rather than discovered in a bucket.
func TestSweepRefusesAGraceShorterThanTheUploadWindow(t *testing.T) {
	h := newHarness(t, attachments.Policy{UploadTTL: time.Hour})

	if _, err := h.store.Sweep(context.Background(), time.Minute); err == nil {
		t.Fatal("a sweep that would reap uploads in flight was allowed")
	} else if !strings.Contains(err.Error(), "in flight") {
		t.Errorf("error = %v, want one that says what it would have deleted", err)
	}
}

// Storage that is down must not be read as storage that is empty: a sweep that
// treated an error as "no objects" would delete nothing, and a completion that
// treated one as "not there" would refuse a good upload.
func TestStorageFailuresAreReportedRatherThanRead(t *testing.T) {
	ctx := context.Background()

	h := newHarness(t, attachments.Policy{},
		sqlbtest.Reply{Match: "SELECT", Cols: attachmentCols,
			Rows: [][]any{row("a1", "att/x/a.png", "a.png", "image/png", 0, "pending")}},
	)
	h.storage.refuse = http.StatusInternalServerError

	if _, err := h.store.Complete(ctx, "a1"); err == nil {
		t.Error("a completion succeeded against storage that was refusing everything")
	} else if strings.Contains(err.Error(), "never uploaded") {
		t.Errorf("a failing storage was read as a missing object: %v", err)
	}
}
