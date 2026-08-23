// Package s3 is a presigning client for S3-compatible object storage, written
// against the standard library.
//
// It exists because this example needs one operation from S3 — hand a browser
// a URL it may PUT bytes to — and taking the AWS SDK for it would put a
// hundred-odd packages behind an example about ordering. Signature Version 4
// is a documented HMAC construction; what follows is that construction and
// nothing else.
//
// # Everything is a presigned URL, including the server's own requests
//
// SigV4 signs a request either in a header or in the query string. The query
// form is what a presigned upload needs, since the browser sends no
// Authorization header — and once that is written, the server's own HEAD,
// DELETE and LIST can use it too, with a lifetime measured in seconds. One
// signing routine rather than two, and nothing here has to build an
// Authorization header at all.
//
// # What it is not
//
// Not an S3 client. No retries, no multipart upload, no checksums beyond what
// the server computes, no credential chain — the keys are passed in, because a
// worked example that read them from the environment would be demonstrating
// the AWS SDK's credential resolution rather than the thing it is about.
//
// Tested against MinIO- and RustFS-compatible path-style endpoints; the
// signature itself is cross-checked against aws-sdk-go-v2 in presign_test.go.
package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Config is what a presigner needs and cannot derive.
type Config struct {
	// Endpoint is the storage root, e.g. http://localhost:9000 for a RustFS or
	// MinIO container. Path-style: the bucket is the first path segment rather
	// than a subdomain, which is what a container on localhost can serve.
	Endpoint string

	// Bucket holds the objects.
	Bucket string

	// Region is part of the credential scope. Storage that does not have
	// regions still requires one to match what the credential says, and
	// "us-east-1" is what both MinIO and RustFS default to.
	Region string

	// AccessKey and SecretKey are the credentials the signature is derived
	// from. They never leave the server: a presigned URL carries the access
	// key and a signature over the request, not the secret.
	AccessKey string
	SecretKey string

	// Clock replaces time.Now, and exists because a signature is only
	// reproducible against a fixed one: presign_test.go pins its golden URLs
	// against the values aws-sdk-go-v2 produced for the same instant. Nil is
	// time.Now, which is what everything outside a test wants.
	Clock func() time.Time
}

// Client presigns requests against one bucket.
//
// The zero value is unusable; build one with New.
type Client struct {
	cfg  Config
	root *url.URL

	now func() time.Time
}

// New validates a configuration and returns a client for it.
func New(cfg Config) (*Client, error) {
	switch {
	case cfg.Endpoint == "":
		return nil, fmt.Errorf("s3: no Endpoint; a presigner needs the storage root, e.g. http://localhost:9000")
	case cfg.Bucket == "":
		return nil, fmt.Errorf("s3: no Bucket")
	case cfg.AccessKey == "" || cfg.SecretKey == "":
		return nil, fmt.Errorf("s3: no credentials")
	}
	root, err := url.Parse(strings.TrimSuffix(cfg.Endpoint, "/"))
	if err != nil {
		return nil, fmt.Errorf("s3: Endpoint %q: %w", cfg.Endpoint, err)
	}
	if root.Host == "" {
		return nil, fmt.Errorf("s3: Endpoint %q has no host; it needs a scheme, e.g. http://localhost:9000", cfg.Endpoint)
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Client{cfg: cfg, root: root, now: clock}, nil
}

// Bucket is the bucket this client signs for.
func (c *Client) Bucket() string { return c.cfg.Bucket }

// PresignPut returns a URL a client may PUT bytes to, and the headers it must
// send with them.
//
// contentType is signed rather than merely suggested: it is in SignedHeaders,
// so a request that sends a different one does not verify and the storage
// refuses it. That is the difference between recording what the uploader
// claimed and constraining what it may send.
//
// The size is deliberately *not* signed. S3 can bind a length through a POST
// policy, which is a different upload shape with a different browser API; for
// a PUT the length is checked after the fact, which is one of the reasons the
// completion step in this example issues a HEAD rather than trusting the
// client's word.
func (c *Client) PresignPut(key, contentType string, ttl time.Duration) (string, http.Header, error) {
	headers := http.Header{}
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	signed, err := c.presign(http.MethodPut, key, nil, headers, ttl)
	if err != nil {
		return "", nil, err
	}
	return signed, headers, nil
}

// PresignGet returns a URL that reads the object, for handing a browser a link
// that expires rather than making the bucket public.
func (c *Client) PresignGet(key string, ttl time.Duration) (string, error) {
	return c.presign(http.MethodGet, key, nil, nil, ttl)
}

// PresignHead returns a URL that reads the object's metadata.
func (c *Client) PresignHead(key string, ttl time.Duration) (string, error) {
	return c.presign(http.MethodHead, key, nil, nil, ttl)
}

// PresignDelete returns a URL that removes the object.
func (c *Client) PresignDelete(key string, ttl time.Duration) (string, error) {
	return c.presign(http.MethodDelete, key, nil, nil, ttl)
}

// PresignList returns a URL listing up to max keys under prefix, continuing
// from token when one is given.
//
// The bucket itself is the resource, so this is the one signed request whose
// path is not an object key.
func (c *Client) PresignList(prefix, token string, max int, ttl time.Duration) (string, error) {
	query := url.Values{}
	query.Set("list-type", "2")
	if prefix != "" {
		query.Set("prefix", prefix)
	}
	if token != "" {
		query.Set("continuation-token", token)
	}
	if max > 0 {
		query.Set("max-keys", strconv.Itoa(max))
	}
	return c.presign(http.MethodGet, "", query, nil, ttl)
}

// presign is the whole of Signature Version 4, query-string form.
//
// The steps are the specification's, in its order, because a signature that
// does not verify gives no clue which step was wrong — so each one is left
// recognisable rather than folded into its neighbour.
func (c *Client) presign(method, key string, query url.Values, headers http.Header, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", fmt.Errorf("s3: a presigned URL needs a lifetime")
	}
	// The specification caps a presigned URL at seven days. Sending a longer
	// one produces a rejection from the storage naming a number, which is a
	// worse way to learn this than a sentence.
	if ttl > 7*24*time.Hour {
		return "", fmt.Errorf("s3: %s exceeds the seven-day maximum for a presigned URL", ttl)
	}

	now := c.now().UTC()
	amzDate := now.Format("20060102T150405Z")
	scopeDate := now.Format("20060102")
	scope := strings.Join([]string{scopeDate, c.cfg.Region, "s3", "aws4_request"}, "/")

	canonicalURI := "/" + uriEncodePath(c.cfg.Bucket)
	if key != "" {
		canonicalURI += "/" + uriEncodePath(key)
	}

	// The host header is always signed, and is the only one that has to be:
	// it is what stops a signature for one endpoint being replayed against
	// another.
	canonicalHeaders := "host:" + c.root.Host + "\n"
	signedHeaders := []string{"host"}
	for name, values := range headers {
		lower := strings.ToLower(name)
		canonicalHeaders += lower + ":" + strings.TrimSpace(strings.Join(values, ",")) + "\n"
		signedHeaders = append(signedHeaders, lower)
	}
	sort.Strings(signedHeaders)
	if len(signedHeaders) > 1 {
		// Rebuilt in sorted order: the canonical form is sorted by header
		// name, and appending as they came out of a map is not that.
		canonicalHeaders = ""
		for _, name := range signedHeaders {
			value := c.root.Host
			if name != "host" {
				value = strings.TrimSpace(strings.Join(headers.Values(name), ","))
			}
			canonicalHeaders += name + ":" + value + "\n"
		}
	}
	signedHeaderList := strings.Join(signedHeaders, ";")

	if query == nil {
		query = url.Values{}
	}
	query.Set("X-Amz-Algorithm", algorithm)
	query.Set("X-Amz-Credential", c.cfg.AccessKey+"/"+scope)
	query.Set("X-Amz-Date", amzDate)
	query.Set("X-Amz-Expires", strconv.Itoa(int(ttl.Seconds())))
	query.Set("X-Amz-SignedHeaders", signedHeaderList)

	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI,
		canonicalQuery(query),
		canonicalHeaders,
		signedHeaderList,
		// The bytes are not in this request — the browser sends them — so
		// there is nothing to hash. UNSIGNED-PAYLOAD is the documented
		// spelling of that, and it is what makes a presigned PUT possible at
		// all.
		unsignedPayload,
	}, "\n")

	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(c.signingKey(scopeDate), stringToSign))
	query.Set("X-Amz-Signature", signature)

	return c.root.Scheme + "://" + c.root.Host + canonicalURI + "?" + canonicalQuery(query), nil
}

// signingKey derives the date/region/service key the signature is taken with.
//
// Four nested HMACs, each keyed by the last, which is what makes a leaked
// signing key expire on its own: it is only good for one day, one region and
// one service.
func (c *Client) signingKey(scopeDate string) []byte {
	key := hmacSHA256([]byte("AWS4"+c.cfg.SecretKey), scopeDate)
	key = hmacSHA256(key, c.cfg.Region)
	key = hmacSHA256(key, "s3")
	return hmacSHA256(key, "aws4_request")
}

const (
	algorithm       = "AWS4-HMAC-SHA256"
	unsignedPayload = "UNSIGNED-PAYLOAD"
)

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// canonicalQuery renders parameters the way the signature reads them: sorted
// by name, then by value, each encoded.
func canonicalQuery(query url.Values) string {
	names := make([]string, 0, len(query))
	for name := range query {
		names = append(names, name)
	}
	sort.Strings(names)

	var parts []string
	for _, name := range names {
		values := append([]string(nil), query[name]...)
		sort.Strings(values)
		for _, value := range values {
			parts = append(parts, uriEncode(name)+"="+uriEncode(value))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncodePath encodes an object key, leaving the separators alone.
//
// A key is a string and not a path — "a/b" and "a%2Fb" are different objects —
// but the segments of the key the URL addresses are separated by real slashes,
// so those survive and everything else is escaped.
func uriEncodePath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = uriEncode(part)
	}
	return strings.Join(parts, "/")
}

// uriEncode is the specification's encoding, which is not any of the three the
// standard library offers.
//
// url.QueryEscape writes a space as "+" and leaves "~" alone in some versions;
// url.PathEscape leaves sub-delimiters unescaped. Either produces a canonical
// request that differs from the storage's by one byte, and a signature
// mismatch names nothing. So the rule is written out: unreserved characters
// pass, everything else is percent-encoded with upper-case hex.
func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
