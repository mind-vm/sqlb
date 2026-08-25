// Package attachments is the upload path a generated create body cannot
// carry: the bytes never pass through the server at all.
//
// A browser asks for permission, PUTs the file straight to object storage
// with the URL it gets back, and tells the server it is done. What sqlb holds
// is the row that says where the object is and whether it arrived — and the
// interesting part is not the signing, it is the order the two writes happen
// in and what is left behind when one of them fails.
//
// # The order, and why it is that way round
//
// The row is written first, before the upload it authorises, and it is born
// `pending`. That is deliberate and it is the only ordering that fails safely:
//
//   - Row first. A row whose object never arrived is a row this package can
//     find — it is `pending` and it is old — so [Store.Sweep] reaps it.
//   - Object first. An object whose row was never written is findable only by
//     listing the bucket and asking the database about every key in it, which
//     is a job that gets slower as the bucket grows and that nobody runs.
//
// Deletion is the same argument reversed. The row commits first and the object
// is removed after, in an [sqlb.AfterCommit] callback, because object storage
// is not in the transaction: deleting the object first and then rolling back
// would leave a row pointing at nothing, which renders as a broken image
// forever. Failing the other way leaves an orphaned object, which costs money
// and is reaped by the same sweeper. A leak that can be found beats a lie that
// cannot.
//
// # What is not here
//
// No tenancy: see [attachmentschema.Attachment] for where a `.Scoped()` column
// would go and why it is left out. No image processing, no virus scanning, no
// resumable upload — each of those is a queue and a worker, and this example
// is about the two writes.
package attachments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/attachments/s3"
)

// Policy is what an application decides and a schema cannot.
//
// Every field here is a judgement about what this deployment will accept, and
// each is checked at the presign step — which is the only moment the server is
// in the path at all. After that the client is talking to the storage.
type Policy struct {
	// MaxSize is the largest upload this deployment accepts, in bytes.
	//
	// It is checked against what the client *claims* at the presign step, and
	// against what the storage *reports* at the completion step. Both, because
	// the first is a policy the second cannot express: a presigned PUT carries
	// no length constraint the storage will enforce, so a client that lied
	// still uploads, and the completion step is where the lie is caught.
	MaxSize int64

	// ContentTypes is the set of media types that may be uploaded. Empty
	// accepts anything, which is a defensible choice for an internal tool and
	// not one for a public one.
	//
	// The chosen type is signed into the URL, so this is a constraint the
	// storage enforces rather than a note the row carries.
	ContentTypes []string

	// UploadTTL is how long a presigned PUT stays valid. Short: it is the time
	// between a person choosing a file and the upload starting, not the time
	// the upload takes — the storage checks the signature when the request
	// arrives, not while the body streams.
	UploadTTL time.Duration

	// DownloadTTL is how long a presigned GET stays valid.
	DownloadTTL time.Duration
}

// Defaults fills in what a Policy left at zero.
func (p Policy) Defaults() Policy {
	if p.MaxSize == 0 {
		p.MaxSize = 25 << 20
	}
	if p.UploadTTL == 0 {
		p.UploadTTL = 15 * time.Minute
	}
	if p.DownloadTTL == 0 {
		p.DownloadTTL = time.Hour
	}
	return p
}

func (p Policy) allows(contentType string) bool {
	return len(p.ContentTypes) == 0 || slices.Contains(p.ContentTypes, contentType)
}

// Store is the table and the bucket, together.
//
// It holds no state of its own: the database handle carries the hooks, the
// presigner carries the credentials, and everything else is a decision in
// [Policy].
type Store struct {
	db     *sqlb.DB
	bucket *s3.Client
	policy Policy

	// http issues the server's own requests — the HEAD that confirms an upload
	// and the DELETE that removes an object. It is a field so a test can
	// replace it; there is nothing else configurable about it.
	http *http.Client
}

// NewStore ties a database handle to a bucket.
//
// The handle is a *sqlb.DB rather than an Executor because one of the writes
// here has to own a transaction: deleting an attachment removes an object
// after the commit, and [sqlb.AfterCommit] reports an error rather than
// guessing when there is no transaction to be after. See [RegisterHooks].
func NewStore(db *sqlb.DB, bucket *s3.Client, policy Policy) *Store {
	return &Store{
		db:     db,
		bucket: bucket,
		policy: policy.Defaults(),
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

// Upload is what a client needs in order to send the bytes.
type Upload struct {
	// Attachment is the row, as it stands before the upload: pending, and with
	// a size of zero.
	Attachment Attachment `json:"attachment"`

	// URL is the presigned PUT. It is single-purpose by construction — one
	// method, one key, one content type, one short lifetime — so handing it to
	// a browser grants exactly this upload and nothing else.
	URL string `json:"url"`

	// Headers are what the PUT must send. Sending anything else fails the
	// signature check, which is the point: the content type is a constraint
	// here, not a label.
	Headers map[string]string `json:"headers"`

	// ExpiresAt is when URL stops working.
	ExpiresAt time.Time `json:"expires_at"`
}

// Begin records the intent to upload and returns the URL that authorises it.
//
// The row exists when this returns, and it is pending. Nothing has been
// uploaded yet, and it is entirely possible that nothing ever will be — which
// is a state this design has an answer for rather than a case it hopes does
// not happen.
func (s *Store) Begin(ctx context.Context, filename, contentType string, claimedSize int64) (*Upload, error) {
	switch {
	case strings.TrimSpace(filename) == "":
		return nil, fmt.Errorf("attachments: a filename is required")
	case !s.policy.allows(contentType):
		return nil, fmt.Errorf("attachments: %q is not an accepted content type; this deployment accepts %s",
			contentType, strings.Join(s.policy.ContentTypes, ", "))
	case claimedSize <= 0:
		return nil, fmt.Errorf("attachments: a size is required, so the policy can refuse an upload before it starts")
	case claimedSize > s.policy.MaxSize:
		return nil, fmt.Errorf("attachments: %d bytes exceeds the %d-byte maximum", claimedSize, s.policy.MaxSize)
	}

	key, err := mintKey(filename)
	if err != nil {
		return nil, err
	}

	row := &Attachment{
		Key:         key,
		Filename:    filename,
		ContentType: contentType,
		Status:      AttachmentStatusPending,
	}
	// Written through Go rather than through a request body, which is the same
	// move example/vault makes for its Hidden payload: ReadOnly narrows what a
	// generated surface can reach, not what this package can do.
	stored, err := sqlb.InsertRows(row).One(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("attachments: recording the upload: %w", err)
	}

	// Signed for the key the *row* carries, not the one minted above. They are
	// the same string today, and tying the URL to what the database returned
	// is what keeps them so: the row is the record of where the bytes go, and
	// a URL signed for anything else would authorise an upload nothing points
	// at.
	url, headers, err := s.bucket.PresignPut(stored.Key, contentType, s.policy.UploadTTL)
	if err != nil {
		// The row is already committed and is now a pending row nothing will
		// complete. That is exactly what the sweeper is for, so this reports
		// the failure rather than trying to undo it.
		return nil, fmt.Errorf("attachments: signing the upload URL: %w", err)
	}

	flat := map[string]string{}
	for name := range headers {
		flat[name] = headers.Get(name)
	}
	return &Upload{
		Attachment: stored,
		URL:        url,
		Headers:    flat,
		ExpiresAt:  time.Now().UTC().Add(s.policy.UploadTTL),
	}, nil
}

// Complete asks the storage whether the object is there, and records what it
// says.
//
// The client's word that it finished is not evidence — it is the party with
// the most reason to be wrong about it, and a browser that was closed mid-PUT
// says nothing at all. So this issues a HEAD and reads the size off the
// storage, which is also the only place the real size is known: a presigned
// PUT carries no length the storage would enforce.
func (s *Store) Complete(ctx context.Context, id string) (*Attachment, error) {
	row, err := sqlb.Query[Attachment]().Where(AttachmentCols.ID.Eq(id)).One(ctx, s.db)
	if err != nil {
		return nil, err
	}
	if row.Status == AttachmentStatusReady {
		// Completing twice is a retry, not an error: the client that lost the
		// response has no way to tell which happened.
		return &row, nil
	}

	info, err := s.head(ctx, row.Key)
	if err != nil {
		return nil, err
	}
	if info.size > s.policy.MaxSize {
		// The upload was larger than the claim it was authorised against. The
		// object exists and the row does not accept it, so the object is the
		// thing that has to go.
		if err := s.removeObject(ctx, row.Key); err != nil {
			return nil, fmt.Errorf("attachments: %d bytes exceeds the maximum and the object could not be removed: %w", info.size, err)
		}
		return nil, fmt.Errorf("attachments: %d bytes exceeds the %d-byte maximum", info.size, s.policy.MaxSize)
	}

	updated, err := UpdateAttachment().
		SetStatus(AttachmentStatusReady).
		SetSize(info.size).
		SetUpdatedAt(time.Now().UTC()).
		Where(AttachmentCols.ID.Eq(id)).
		Stmt().
		One(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("attachments: recording the arrival: %w", err)
	}
	return &updated, nil
}

// DownloadURL is a link to the object that expires.
//
// The bucket stays private: nothing here makes an object public, and the only
// way to read one is a URL this method signed, which is why an attachment row
// being readable and its bytes being readable are two separate decisions.
func (s *Store) DownloadURL(ctx context.Context, id string) (string, error) {
	row, err := sqlb.Query[Attachment]().Where(AttachmentCols.ID.Eq(id)).One(ctx, s.db)
	if err != nil {
		return "", err
	}
	if row.Status != AttachmentStatusReady {
		return "", fmt.Errorf("attachments: %s has not been uploaded yet", id)
	}
	return s.bucket.PresignGet(row.Key, s.policy.DownloadTTL)
}

// objectInfo is what a HEAD tells us.
type objectInfo struct {
	size        int64
	contentType string
}

func (s *Store) head(ctx context.Context, key string) (objectInfo, error) {
	url, err := s.bucket.PresignHead(key, time.Minute)
	if err != nil {
		return objectInfo{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return objectInfo{}, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return objectInfo{}, fmt.Errorf("attachments: asking the storage about %s: %w", key, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return objectInfo{}, fmt.Errorf("attachments: %s was never uploaded", key)
	case resp.StatusCode >= 400:
		return objectInfo{}, fmt.Errorf("attachments: the storage answered %s for %s", resp.Status, key)
	}

	size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil {
		return objectInfo{}, fmt.Errorf("attachments: the storage reported no length for %s", key)
	}
	return objectInfo{size: size, contentType: resp.Header.Get("Content-Type")}, nil
}

// removeObject deletes one object, and is deliberately forgiving about an
// object that is already gone: every caller here is reconciling, and "it is
// not there" is the state they were trying to reach.
func (s *Store) removeObject(ctx context.Context, key string) error {
	url, err := s.bucket.PresignDelete(key, time.Minute)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("attachments: deleting %s: %w", key, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("attachments: the storage answered %s deleting %s", resp.Status, key)
	}
	return nil
}

// mintKey builds the object's name in the bucket.
//
// Random rather than derived from the row id, and it matters: the id is in
// URLs and in responses, so a key derived from it would make every object's
// name guessable — and a presigned GET is only as private as the key it names.
// The filename is kept on the end because it is what a browser saves the
// download as, and sanitised because it arrived from a person.
func mintKey(filename string) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("attachments: minting a key: %w", err)
	}
	return "att/" + hex.EncodeToString(buf) + "/" + sanitize(filename), nil
}

// sanitize reduces a filename to something safe to put in a key.
//
// A key is not a path, but it is rendered into a URL and read back by tools
// that treat it like one, so the separators and the traversal go. What is left
// is deliberately conservative: this is the name a download is saved as, not
// an identifier anything looks up.
func sanitize(filename string) string {
	name := path.Base(strings.ReplaceAll(filename, "\\", "/"))
	name = strings.TrimLeft(name, ".")
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "file"
	}
	return b.String()
}
