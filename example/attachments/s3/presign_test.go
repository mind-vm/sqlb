package s3_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb/example/attachments/s3"
)

// The credentials from AWS's own signing documentation. They are not a secret
// and never were: every worked example of Signature Version 4 uses this pair,
// which is what makes a signature computed from them comparable to somebody
// else's.
const (
	access = "AKIAIOSFODNN7EXAMPLE"
	secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

// signedAt is the instant every golden below was produced at. A signature is a
// function of the clock, so pinning one means pinning the other.
var signedAt = time.Date(2026, 8, 23, 9, 26, 41, 0, time.UTC)

func client(t *testing.T) *s3.Client {
	t.Helper()
	c, err := s3.New(s3.Config{
		Endpoint:  "http://localhost:9000",
		Bucket:    "uploads",
		Region:    "us-east-1",
		AccessKey: access,
		SecretKey: secret,
		Clock:     func() time.Time { return signedAt },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func query(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	return u.Query()
}

// The test this file exists for.
//
// The signature below was produced by aws-sdk-go-v2 — s3.NewPresignClient's
// PresignHeadObject, against the same endpoint, bucket, key, credentials,
// region, expiry and instant — and this implementation reproduces it byte for
// byte. That is the only assertion here that is worth anything on its own: the
// rest of this file checks that the pieces stay put, and this one checks that
// they were right to begin with, against the implementation the storage was
// written to satisfy.
//
// HEAD is the operation compared because it is the one where the two agree
// with nothing to explain. On GET, PUT and DELETE the SDK adds a query
// parameter of its own — x-id=GetObject and so on, a marker for its own
// middleware — which the storage ignores but the signature covers, so those
// URLs differ by that parameter and therefore by the signature. Adding it here
// reproduces those too, and it was checked that way while this was written;
// carrying an SDK-internal parameter into a worked example to make a test
// prettier is not worth it.
func TestSignatureMatchesTheAWSSDK(t *testing.T) {
	const want = "2fb419da972977dd92747e2bc83a0b830130e6006a6a11428a0cdc3ab6df420a"

	got, err := client(t).PresignHead("ws1/018f-7a/photo 1(final).jpg", time.Minute)
	if err != nil {
		t.Fatalf("PresignHead: %v", err)
	}

	q := query(t, got)
	if sig := q.Get("X-Amz-Signature"); sig != want {
		t.Errorf("signature = %s, want the SDK's %s\n%s", sig, want, got)
	}
	for name, value := range map[string]string{
		"X-Amz-Algorithm":     "AWS4-HMAC-SHA256",
		"X-Amz-Credential":    access + "/20260823/us-east-1/s3/aws4_request",
		"X-Amz-Date":          "20260823T092641Z",
		"X-Amz-Expires":       "60",
		"X-Amz-SignedHeaders": "host",
	} {
		if got := q.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
}

// The key is in the path, and the path is what the signature covers. Every
// encoding rule below is one the standard library's escapers get differently,
// which is why the encoder is written out rather than borrowed.
func TestKeyEncodingFollowsTheSigningRules(t *testing.T) {
	c := client(t)

	for _, tc := range []struct {
		key  string
		want string
	}{
		{"photo 1.jpg", "/uploads/photo%201.jpg"},     // a space is %20, never +
		{"photo(1).jpg", "/uploads/photo%281%29.jpg"}, // sub-delimiters are escaped
		{"ws1/018f/a.jpg", "/uploads/ws1/018f/a.jpg"}, // the separators are not
		{"a~b-c_d.e", "/uploads/a~b-c_d.e"},           // unreserved characters pass
		{"café.jpg", "/uploads/caf%C3%A9.jpg"},        // UTF-8, byte by byte
		{"a+b.jpg", "/uploads/a%2Bb.jpg"},             // a plus is a plus, not a space
		{"100%.txt", "/uploads/100%25.txt"},           // and a percent is escaped once
	} {
		raw, err := c.PresignGet(tc.key, time.Minute)
		if err != nil {
			t.Fatalf("PresignGet(%q): %v", tc.key, err)
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parsing %q: %v", raw, err)
		}
		if got := u.EscapedPath(); got != tc.want {
			t.Errorf("key %q encoded to %q, want %q", tc.key, got, tc.want)
		}
	}
}

// Signing the content type is what turns it from a claim the client made into
// a constraint the storage enforces: it is in SignedHeaders, so a PUT sending
// anything else does not verify.
func TestPresignPutBindsTheContentType(t *testing.T) {
	url, headers, err := client(t).PresignPut("ws1/a.jpg", "image/jpeg", 15*time.Minute)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}

	if got := query(t, url).Get("X-Amz-SignedHeaders"); got != "content-type;host" {
		t.Errorf("SignedHeaders = %q, want content-type;host — an unsigned type is only a suggestion", got)
	}
	// The caller has to be told what to send, or the constraint is a trap
	// rather than a guarantee.
	if got := headers.Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("the returned headers say %q, want image/jpeg", got)
	}
}

// A signature is only as good as its expiry, so the two ways of getting that
// wrong are refused with a sentence rather than passed to the storage, which
// answers them with a number.
func TestLifetimeIsChecked(t *testing.T) {
	c := client(t)

	if _, err := c.PresignGet("a.jpg", 0); err == nil {
		t.Error("a presigned URL with no lifetime was accepted")
	}
	if _, err := c.PresignGet("a.jpg", 8*24*time.Hour); err == nil {
		t.Error("a lifetime beyond the seven-day maximum was accepted")
	}
	if _, err := c.PresignGet("a.jpg", 7*24*time.Hour); err != nil {
		t.Errorf("seven days is the maximum and should be allowed: %v", err)
	}
}

// The bucket is the resource a listing addresses, which is the one signed
// request in this package whose path is not an object key.
func TestPresignListAddressesTheBucket(t *testing.T) {
	raw, err := client(t).PresignList("ws1/", "token-2", 100, time.Minute)
	if err != nil {
		t.Fatalf("PresignList: %v", err)
	}

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	if u.EscapedPath() != "/uploads" {
		t.Errorf("path = %q, want the bucket itself", u.EscapedPath())
	}
	q := u.Query()
	for name, want := range map[string]string{
		"list-type":          "2",
		"prefix":             "ws1/",
		"continuation-token": "token-2",
		"max-keys":           "100",
	} {
		if got := q.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// The listing parameters are signed like everything else, so a URL whose
	// prefix was edited on the way past does not verify.
	if !strings.Contains(raw, "X-Amz-Signature=") {
		t.Error("the listing is not signed")
	}
}

// A configuration that cannot sign anything is refused where it is built,
// naming what is missing.
func TestNewRefusesAnUnusableConfiguration(t *testing.T) {
	for name, cfg := range map[string]s3.Config{
		"no endpoint":    {Bucket: "b", AccessKey: "a", SecretKey: "s"},
		"no bucket":      {Endpoint: "http://h:9000", AccessKey: "a", SecretKey: "s"},
		"no credentials": {Endpoint: "http://h:9000", Bucket: "b"},
		"bare host":      {Endpoint: "localhost:9000", Bucket: "b", AccessKey: "a", SecretKey: "s"},
	} {
		if _, err := s3.New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// A region is the one thing with a sensible default: storage that has no
	// regions still wants the credential scope to say one.
	c, err := s3.New(s3.Config{Endpoint: "http://h:9000", Bucket: "b", AccessKey: "a", SecretKey: "s"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	raw, err := c.PresignGet("a.jpg", time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	if !strings.Contains(query(t, raw).Get("X-Amz-Credential"), "/us-east-1/s3/") {
		t.Errorf("no region defaulted into the credential scope: %s", raw)
	}
}
