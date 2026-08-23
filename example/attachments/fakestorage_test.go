package attachments_test

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStorage is an S3-compatible endpoint that holds objects in a map.
//
// It is not a signature verifier — presign_test.go answers that question
// against aws-sdk-go-v2, which is a better answer than a second
// implementation of the same arithmetic would be. What this checks is the part
// a signature cannot: that the request the client makes is the request the URL
// authorised. A PUT arriving with no signature at all, or with a content type
// other than the one that was signed into it, is refused here the way real
// storage refuses it.
type fakeStorage struct {
	*httptest.Server

	mu      sync.Mutex
	objects map[string]storedObject

	// refuse, when set, answers every request with it. A storage that is down
	// is a case the completion step has to survive.
	refuse int
}

type storedObject struct {
	body        []byte
	contentType string
	modified    time.Time
}

func newFakeStorage(t *testing.T) *fakeStorage {
	t.Helper()
	f := &fakeStorage{objects: map[string]storedObject{}}
	f.Server = httptest.NewServer(f)
	t.Cleanup(f.Close)
	return f
}

// put writes an object as though a client had uploaded it, for the tests that
// need one to exist without going through a presigned URL.
func (f *fakeStorage) put(key, contentType string, body []byte, modified time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = storedObject{body: body, contentType: contentType, modified: modified}
}

func (f *fakeStorage) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

func (f *fakeStorage) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.objects))
	for key := range f.objects {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func (f *fakeStorage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.refuse != 0 {
		w.WriteHeader(f.refuse)
		return
	}

	query := r.URL.Query()
	// Every request this package makes is presigned, including the server's
	// own. An unsigned one is a bug in the caller, not a request to serve.
	if query.Get("X-Amz-Signature") == "" || query.Get("X-Amz-Credential") == "" {
		http.Error(w, "unsigned request", http.StatusForbidden)
		return
	}

	// The bucket is the first path segment: path style, which is what a
	// container on localhost can serve and what RustFS and MinIO default to.
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(trimmed, "/")
	if bucket != "uploads" {
		http.Error(w, "no such bucket", http.StatusNotFound)
		return
	}

	if key == "" && query.Get("list-type") == "2" {
		f.list(w, query)
		return
	}

	switch r.Method {
	case http.MethodPut:
		f.upload(w, r, key, query)
	case http.MethodHead, http.MethodGet:
		f.read(w, r, key)
	case http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (f *fakeStorage) upload(w http.ResponseWriter, r *http.Request, key string, query url.Values) {
	// The constraint the presigned PUT exists to impose: content-type is in
	// SignedHeaders, so a request sending a different one does not verify.
	// Real storage discovers that by recomputing the signature; this checks
	// the same fact directly, which is what makes the test about the rule
	// rather than about the arithmetic.
	if strings.Contains(query.Get("X-Amz-SignedHeaders"), "content-type") {
		if r.Header.Get("Content-Type") == "" {
			http.Error(w, "content-type was signed and not sent", http.StatusForbidden)
			return
		}
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.objects[key] = storedObject{
		body:        body,
		contentType: r.Header.Get("Content-Type"),
		modified:    time.Now().UTC(),
	}
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (f *fakeStorage) read(w http.ResponseWriter, r *http.Request, key string) {
	f.mu.Lock()
	object, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		http.Error(w, "no such key", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Length", strconv.Itoa(len(object.body)))
	if object.contentType != "" {
		w.Header().Set("Content-Type", object.contentType)
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(object.body)
}

func (f *fakeStorage) list(w http.ResponseWriter, query url.Values) {
	prefix := query.Get("prefix")

	type entry struct {
		Key          string    `xml:"Key"`
		LastModified time.Time `xml:"LastModified"`
	}
	var result struct {
		XMLName     xml.Name `xml:"ListBucketResult"`
		Contents    []entry  `xml:"Contents"`
		IsTruncated bool     `xml:"IsTruncated"`
	}

	f.mu.Lock()
	for _, key := range f.sortedKeysLocked() {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		result.Contents = append(result.Contents, entry{Key: key, LastModified: f.objects[key].modified})
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(result)
}

func (f *fakeStorage) sortedKeysLocked() []string {
	out := make([]string, 0, len(f.objects))
	for key := range f.objects {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
