package studio

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// multipartUpload builds a multipart/form-data body carrying format and one
// file field, the shape handleRowsImportSubmit's r.FormFile("file") and
// r.FormValue("format") expect.
func multipartUpload(t *testing.T, format, filename, content string) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("format", format); err != nil {
		t.Fatal(err)
	}
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

func TestImportJSONHappyPath(t *testing.T) {
	const token = "secret-token"
	api := fakeAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	body, contentType := multipartUpload(t, "json", "widgets.json",
		`[{"title":"Imported one","owner_id":"o1","count":1,"status":"draft"},`+
			`{"title":"Imported two","owner_id":"o1","count":2,"status":"draft"}]`)

	resp, err := client.Post(ts.URL+"/tables/widgets/rows/import", contentType, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	page, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "Created 2, failed 0") {
		t.Fatalf("import summary missing 2 created, 0 failed:\n%s", page)
	}
}

func TestImportCSVUnknownHeaderIsReportedBeforeAnyRowIsSent(t *testing.T) {
	const token = "secret-token"
	api := fakeAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	body, contentType := multipartUpload(t, "csv", "widgets.csv",
		"title,owner_id,not_a_column\nImported,o1,x\n")

	resp, err := client.Post(ts.URL+"/tables/widgets/rows/import", contentType, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	page, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "not_a_column") {
		t.Fatalf("error page missing the offending header name:\n%s", page)
	}
}

func TestImportReportsPartialFailure(t *testing.T) {
	const token = "secret-token"
	api := fakeAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	// fakeAPI's POST /widgets rejects a row with no title.
	body, contentType := multipartUpload(t, "json", "widgets.json",
		`[{"title":"Has a title","owner_id":"o1"},{"owner_id":"o1"}]`)

	resp, err := client.Post(ts.URL+"/tables/widgets/rows/import", contentType, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	page, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "Created 1, failed 1") {
		t.Fatalf("import summary missing 1 created, 1 failed:\n%s", page)
	}
}

func TestImportRejectsSQLFormat(t *testing.T) {
	const token = "secret-token"
	api := fakeAPI(t, token)
	defer api.Close()
	srv, err := NewServer(testManifest(), api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := loggedInClient(t, ts.URL, token)

	body, contentType := multipartUpload(t, "sql", "widgets.sql", "INSERT INTO widgets VALUES (1);")

	resp, err := client.Post(ts.URL+"/tables/widgets/rows/import", contentType, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK { // studio renders its own error page, not an HTTP error
		t.Fatalf("status = %d, want 200 with an in-page error", resp.StatusCode)
	}
	page, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "format must be json or csv") {
		t.Fatalf("error page missing the format rejection:\n%s", page)
	}
}
