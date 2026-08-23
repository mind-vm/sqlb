package attachments

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jryannel/sqlb"
)

// SweepReport is what one pass reconciled.
type SweepReport struct {
	// Abandoned is the number of pending rows reaped: an upload was authorised
	// and never arrived, and the URL that authorised it has expired.
	Abandoned int

	// Orphans is the number of objects deleted that no row referenced.
	Orphans int
}

// Sweep reconciles the bucket with the table, in both directions.
//
// This is the other half of the ordering [Store.Begin] and [RegisterHooks]
// choose. Both of them fail toward garbage that can be found rather than
// toward a row that lies, and this is what finds it. It is a job to run on a
// timer, not something a request path calls.
//
// Two directions, because the two failures are different:
//
//   - A row with no object. Its upload was authorised and never happened —
//     the person closed the tab, the network died, the file was deleted before
//     the PUT. It is pending and it is older than the URL it was given, so
//     nothing can complete it any more.
//   - An object with no row. Its row was deleted and the object outlived the
//     cleanup, or a completion crashed between the PUT and the update. It
//     costs storage forever and nothing references it.
//
// grace is how far back to look. It must comfortably exceed
// [Policy.UploadTTL], because an upload in flight is a pending row with no
// object *yet* — sweeping one of those deletes a file somebody is uploading
// right now.
func (s *Store) Sweep(ctx context.Context, grace time.Duration) (SweepReport, error) {
	if grace < s.policy.UploadTTL {
		return SweepReport{}, fmt.Errorf(
			"attachments: a grace of %s is shorter than the %s upload window, so this would reap uploads in flight",
			grace, s.policy.UploadTTL)
	}
	cutoff := time.Now().UTC().Add(-grace)

	var report SweepReport

	// Direction one: rows nothing will ever complete.
	//
	// Deleted through the ordinary delete path, so the hook runs and takes any
	// object that did arrive after all with it. That is the one case where a
	// pending row has bytes: the PUT succeeded and the completion call never
	// came.
	// In a transaction, and not as a matter of taste: the delete's hook
	// removes each object with sqlb.AfterCommit, which refuses to run when
	// there is no commit to be after. A sweeper that deleted rows under
	// autocommit would get an error naming exactly that — which is the error
	// arriving in the right place, since without it the objects would be
	// silently left behind.
	if err := s.db.WithTx(ctx, func(ctx context.Context, tx *sqlb.DB) error {
		removed, err := sqlb.DeleteRows[Attachment]().
			Where(AttachmentCols.Status.Eq(AttachmentStatusPending), AttachmentCols.CreatedAt.Lt(cutoff)).
			Exec(ctx, tx)
		if err != nil {
			return err
		}
		report.Abandoned = int(removed)
		return nil
	}); err != nil {
		return report, fmt.Errorf("attachments: reaping abandoned uploads: %w", err)
	}

	// Direction two: objects nothing references.
	//
	// The bucket is walked rather than the table, because that is the
	// direction the question runs: the table cannot name a key it never
	// recorded. Each page is checked against the rows in one query rather than
	// one per key.
	var token string
	for {
		page, err := s.list(ctx, "att/", token)
		if err != nil {
			return report, err
		}
		if len(page.keys) > 0 {
			known, err := s.knownKeys(ctx, page.keys)
			if err != nil {
				return report, err
			}
			for _, key := range page.keys {
				if known[key] {
					continue
				}
				// An object written in the last few minutes is not an orphan
				// yet: its row may be committing as this runs. The same grace
				// the other direction uses, for the same reason.
				if key.modified.After(cutoff) {
					continue
				}
				if err := s.removeObject(ctx, key.name); err != nil {
					return report, err
				}
				report.Orphans++
			}
		}
		if page.next == "" {
			return report, nil
		}
		token = page.next
	}
}

// objectKey is one entry of a listing.
type objectKey struct {
	name     string
	modified time.Time
}

type listPage struct {
	keys []objectKey
	next string
}

// knownKeys asks which of these keys the table still references.
//
// One statement per page rather than one per key: the whole point of walking
// the bucket in pages is that the reconciliation cost stays proportional to
// the bucket rather than to a round trip.
func (s *Store) knownKeys(ctx context.Context, keys []objectKey) (map[objectKey]bool, error) {
	names := make([]string, 0, len(keys))
	for _, key := range keys {
		names = append(names, key.name)
	}

	rows, err := sqlb.Query[Attachment]().
		Select(AttachmentCols.Key).
		Where(AttachmentCols.Key.OneOf(names...)).
		All(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("attachments: matching objects against rows: %w", err)
	}

	byName := make(map[string]bool, len(rows))
	for _, row := range rows {
		byName[row.Key] = true
	}
	known := make(map[objectKey]bool, len(keys))
	for _, key := range keys {
		known[key] = byName[key.name]
	}
	return known, nil
}

// list reads one page of the bucket.
//
// The response is XML, which is the one place this example meets S3's age. It
// is parsed with encoding/xml into the three fields that matter rather than
// modelled: a listing has about thirty, and twenty-seven of them are not this
// example's business.
func (s *Store) list(ctx context.Context, prefix, token string) (listPage, error) {
	url, err := s.bucket.PresignList(prefix, token, 1000, time.Minute)
	if err != nil {
		return listPage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return listPage{}, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return listPage{}, fmt.Errorf("attachments: listing the bucket: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return listPage{}, fmt.Errorf("attachments: the storage answered %s listing the bucket: %s", resp.Status, body)
	}

	var parsed struct {
		Contents []struct {
			Key          string    `xml:"Key"`
			LastModified time.Time `xml:"LastModified"`
		} `xml:"Contents"`
		IsTruncated           bool   `xml:"IsTruncated"`
		NextContinuationToken string `xml:"NextContinuationToken"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return listPage{}, fmt.Errorf("attachments: reading the listing: %w", err)
	}

	page := listPage{}
	for _, entry := range parsed.Contents {
		page.keys = append(page.keys, objectKey{name: entry.Key, modified: entry.LastModified.UTC()})
	}
	if parsed.IsTruncated {
		page.next = parsed.NextContinuationToken
	}
	return page, nil
}
