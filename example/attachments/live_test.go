package attachments_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jryannel/sqlb/example/attachments/s3"
)

// The one question the other two test files cannot answer: does real storage
// accept these signatures.
//
// presign_test.go compares them to aws-sdk-go-v2, which is strong evidence and
// not proof — an implementation can agree with the SDK and still be refused by
// a server that reads the URL differently, and path-style addressing against a
// container is exactly where that happens. So this runs the whole object
// lifecycle against a live endpoint and is skipped when there is not one.
//
// It takes a DSN and starts nothing, the way sqlb's database-backed suites do:
//
//	docker run -p 9000:9000 -e RUSTFS_ACCESS_KEY=rustfsadmin \
//	    -e RUSTFS_SECRET_KEY=rustfsadmin rustfs/rustfs
//
//	SQLB_TEST_S3_ENDPOINT=http://localhost:9000 \
//	SQLB_TEST_S3_BUCKET=uploads \
//	SQLB_TEST_S3_ACCESS_KEY=rustfsadmin \
//	SQLB_TEST_S3_SECRET_KEY=rustfsadmin \
//	    go test ./... -run Live -v
//
// MinIO works the same way, with MINIO_ROOT_USER and MINIO_ROOT_PASSWORD. The
// bucket has to exist; creating it is a PUT this package has no reason to
// know how to make.
const (
	endpointEnv = "SQLB_TEST_S3_ENDPOINT"
	bucketEnv   = "SQLB_TEST_S3_BUCKET"
	accessEnv   = "SQLB_TEST_S3_ACCESS_KEY"
	secretEnv   = "SQLB_TEST_S3_SECRET_KEY"
	regionEnv   = "SQLB_TEST_S3_REGION"
)

func liveBucket(t *testing.T) *s3.Client {
	t.Helper()

	endpoint := os.Getenv(endpointEnv)
	if endpoint == "" {
		t.Skipf("%s is not set, so there is no storage to talk to; see the comment above", endpointEnv)
	}
	bucket := os.Getenv(bucketEnv)
	if bucket == "" {
		bucket = "uploads"
	}
	client, err := s3.New(s3.Config{
		Endpoint:  endpoint,
		Bucket:    bucket,
		Region:    os.Getenv(regionEnv),
		AccessKey: os.Getenv(accessEnv),
		SecretKey: os.Getenv(secretEnv),
	})
	if err != nil {
		t.Fatalf("building a client for %s: %v", endpoint, err)
	}
	return client
}

// TestLiveObjectLifecycle walks one object through every request this package
// makes, against whatever is listening.
//
// Each step is a separate signature over a separate method, and a failure
// names which one — because "the signature is wrong" is a different bug from
// "the listing is parsed wrong", and a single end-to-end assertion could not
// tell them apart.
func TestLiveObjectLifecycle(t *testing.T) {
	bucket := liveBucket(t)
	ctx := context.Background()

	key := fmt.Sprintf("att/live-%d/hello world.txt", time.Now().UnixNano())
	body := []byte("bytes that went straight from the client to the storage")

	// 1. The upload the whole design exists for: a URL the server signed and
	//    never saw the bytes for.
	put, headers, err := bucket.PresignPut(key, "text/plain", 5*time.Minute)
	if err != nil {
		t.Fatalf("signing the upload: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, put, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name := range headers {
		req.Header.Set(name, headers.Get(name))
	}
	resp := do(t, req)
	if resp.status >= 300 {
		t.Fatalf("PUT answered %s\n%s\n%s", resp.text, put, resp.body)
	}

	// 2. The content type was signed, so sending a different one must fail.
	//    This is the constraint the presign step imposes, checked against a
	//    server that enforces it rather than against a fake that agrees to.
	req, err = http.NewRequestWithContext(ctx, http.MethodPut, put, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if resp := do(t, req); resp.status < 400 {
		t.Errorf("a PUT with an unsigned content type answered %s, want a refusal", resp.text)
	}

	// 3. HEAD, which is what the completion step reads the size off.
	head, err := bucket.PresignHead(key, time.Minute)
	if err != nil {
		t.Fatalf("signing the head: %v", err)
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodHead, head, nil)
	resp = do(t, req)
	if resp.status != http.StatusOK {
		t.Fatalf("HEAD answered %s", resp.text)
	}
	if got := resp.length; got != int64(len(body)) {
		t.Errorf("the storage reports %d bytes, want %d", got, len(body))
	}

	// 4. GET, which is what a download link is.
	get, err := bucket.PresignGet(key, time.Minute)
	if err != nil {
		t.Fatalf("signing the download: %v", err)
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, get, nil)
	resp = do(t, req)
	if resp.status != http.StatusOK {
		t.Fatalf("GET answered %s", resp.text)
	}
	if got := resp.body; got != string(body) {
		t.Errorf("the object reads back as %q", got)
	}

	// 5. LIST, which is how the sweeper finds an object with no row. The XML
	//    is the part most likely to differ between implementations, so this
	//    asserts the key came back rather than that the request succeeded.
	list, err := bucket.PresignList("att/", "", 1000, time.Minute)
	if err != nil {
		t.Fatalf("signing the listing: %v", err)
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, list, nil)
	resp = do(t, req)
	if resp.status != http.StatusOK {
		t.Fatalf("LIST answered %s", resp.text)
	}
	if listing := resp.body; !bytes.Contains([]byte(listing), []byte(key)) {
		t.Errorf("the listing does not name the object just uploaded:\n%s", listing)
	}

	// 6. DELETE, which is what the after-commit hook issues.
	del, err := bucket.PresignDelete(key, time.Minute)
	if err != nil {
		t.Fatalf("signing the delete: %v", err)
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodDelete, del, nil)
	if resp := do(t, req); resp.status >= 300 {
		t.Fatalf("DELETE answered %s", resp.text)
	}

	req, _ = http.NewRequestWithContext(ctx, http.MethodHead, head, nil)
	if resp := do(t, req); resp.status != http.StatusNotFound {
		t.Errorf("after the delete, HEAD answered %s, want 404", resp.text)
	}
}

// An expired URL is refused, which is what makes a short lifetime a control
// rather than a decoration.
func TestLiveExpiredURLIsRefused(t *testing.T) {
	bucket := liveBucket(t)

	get, err := bucket.PresignGet("att/whatever.txt", time.Second)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	time.Sleep(2 * time.Second)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, get, nil)
	if resp := do(t, req); resp.status != http.StatusForbidden {
		t.Errorf("an expired URL answered %s, want 403", resp.text)
	}
}

// answer is a response already read and closed.
//
// The body is drained here rather than by each caller because a live test
// makes a dozen requests and a leaked connection in one of them shows up as a
// hang in another.
type answer struct {
	status int
	text   string
	length int64
	body   string
}

func do(t *testing.T, req *http.Request) answer {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("reading the response to %s %s: %v", req.Method, req.URL.Path, err)
	}
	return answer{status: resp.StatusCode, text: resp.Status, length: resp.ContentLength, body: string(body)}
}
