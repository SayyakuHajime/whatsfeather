package app

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

type gifRoundTripper func(*http.Request) (*http.Response, error)

func (f gifRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func useGIFHTTPTransport(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	previousTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
}

func makeSyntheticGIF(t *testing.T) []byte {
	t.Helper()
	imageData := image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.RGBA{A: 0xff}})
	var encoded bytes.Buffer
	if err := gif.Encode(&encoded, imageData, nil); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestGIFSearchRejectsUntrustedPreviewURL(t *testing.T) {
	previousTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	requestHost := ""
	http.DefaultTransport = gifRoundTripper(func(request *http.Request) (*http.Response, error) {
		requestHost = request.URL.Host
		body := `{"query":{"pages":{"10":{"title":"File:allowed.gif","imageinfo":[{"url":"https://upload.wikimedia.org/wikipedia/commons/a/allowed.gif","thumburl":"https://upload.wikimedia.org/wikipedia/commons/thumb/a/allowed.gif/160px-allowed.gif","width":320,"height":240}]},"11":{"title":"File:unsafe-preview.gif","imageinfo":[{"url":"https://upload.wikimedia.org/wikipedia/commons/b/unsafe.gif","thumburl":"https://tracker.example/pixel.gif","width":320,"height":240}]}}}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	})

	application := &App{gifCache: make(map[string]gifResult)}
	result, err := application.gifSearch(context.Background(), []byte(`{"query":"dancing cats","provider":"commons"}`))
	if err != nil {
		t.Fatal(err)
	}
	if requestHost != "commons.wikimedia.org" {
		t.Fatalf("search must stay on the fixed Wikimedia API host, got %q", requestHost)
	}
	response, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected GIF search response type %T", result)
	}
	items, ok := response["items"].([]map[string]any)
	if !ok {
		t.Fatalf("unexpected GIF result list type %T", response["items"])
	}
	if len(items) != 1 {
		t.Fatalf("expected only the result with an allowlisted preview host, got %d items: %#v", len(items), items)
	}
	if items[0]["id"] != "commons:10" || items[0]["preview_url"] != "https://upload.wikimedia.org/wikipedia/commons/thumb/a/allowed.gif/160px-allowed.gif" {
		t.Fatalf("unexpected sanitized result: %#v", items[0])
	}
}

func TestGIFSearchRejectsRedirectWithoutFollowing(t *testing.T) {
	calls := 0
	useGIFHTTPTransport(t, gifRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Host == "commons.wikimedia.org" {
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://tracker.example/search"}}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"query":{"pages":{}}}`)), Header: make(http.Header), Request: request}, nil
	}))
	application := &App{gifCache: make(map[string]gifResult)}
	_, err := application.gifSearch(context.Background(), []byte(`{"query":"synthetic query","provider":"commons"}`))
	if err == nil {
		t.Fatal("redirected provider search should fail closed")
	}
	if calls != 1 {
		t.Fatalf("redirect target was contacted: %d requests", calls)
	}
}

func TestGIFSearchNormalizesWikimediaImageURLs(t *testing.T) {
	body := `{"query":{"pages":{"10":{"index":1,"title":"File:allowed.gif","imageinfo":[{"url":"https://upload.wikimedia.org/wikipedia/commons/a/allowed.gif?utm_campaign=imageinfo&utm_content=original&utm_source=commons.wikimedia.org","thumburl":"https://thumb.wikimedia.org/wikipedia/commons/thumb/a/allowed.gif/160px-allowed.gif?utm_campaign=imageinfo&utm_content=thumbnail&utm_source=commons.wikimedia.org","width":320,"height":240}]}}}}`
	useGIFHTTPTransport(t, gifRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	}))
	application := &App{gifCache: make(map[string]gifResult)}
	result, err := application.gifSearch(context.Background(), []byte(`{"query":"dancing cats","provider":"commons"}`))
	if err != nil {
		t.Fatal(err)
	}
	items := result.(map[string]any)["items"].([]map[string]any)
	if len(items) != 1 {
		t.Fatalf("valid Wikimedia URL variants were rejected: %#v", items)
	}
	if items[0]["source_url"] != "https://upload.wikimedia.org/wikipedia/commons/a/allowed.gif" || items[0]["preview_url"] != "https://thumb.wikimedia.org/wikipedia/commons/thumb/a/allowed.gif/160px-allowed.gif" {
		t.Fatalf("Wikimedia image URLs were not canonicalized: %#v", items[0])
	}
}

func TestCanonicalCommonsMediaURLRejectsNonCanonicalTargets(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantURL string
	}{
		{"original", "https://upload.wikimedia.org/wikipedia/commons/a/file.gif", "https://upload.wikimedia.org/wikipedia/commons/a/file.gif"},
		{"thumbnail", "https://upload.wikimedia.org/wikipedia/commons/thumb/a/file.gif/160px-file.gif", "https://upload.wikimedia.org/wikipedia/commons/thumb/a/file.gif/160px-file.gif"},
		{"tracking query stripped", "https://upload.wikimedia.org/wikipedia/commons/a/file.gif?utm_campaign=imageinfo&utm_content=original&utm_source=commons.wikimedia.org", "https://upload.wikimedia.org/wikipedia/commons/a/file.gif"},
		{"wrong host", "https://upload.wikimedia.org.evil.example/wikipedia/commons/a/file.gif", ""},
		{"thumbnail host is not an original", "https://thumb.wikimedia.org/wikipedia/commons/thumb/a/file.gif/160px-file.gif", ""},
		{"userinfo", "https://user@upload.wikimedia.org/wikipedia/commons/a/file.gif", ""},
		{"port", "https://upload.wikimedia.org:443/wikipedia/commons/a/file.gif", ""},
		{"unknown query", "https://upload.wikimedia.org/wikipedia/commons/a/file.gif?token=x", ""},
		{"fragment", "https://upload.wikimedia.org/wikipedia/commons/a/file.gif#x", ""},
		{"wrong path", "https://upload.wikimedia.org/wikipedia/en/a/file.gif", ""},
		{"insecure scheme", "http://upload.wikimedia.org/wikipedia/commons/a/file.gif", ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, ok := canonicalCommonsMediaURL(test.url, false)
			if test.wantURL == "" {
				if ok {
					t.Fatalf("canonicalCommonsMediaURL(%q) unexpectedly accepted %q", test.url, got)
				}
				return
			}
			if !ok || got != test.wantURL {
				t.Fatalf("canonicalCommonsMediaURL(%q) = %q, %v; want %q", test.url, got, ok, test.wantURL)
			}
		})
	}
}

func TestGIFSearchUsesFixedHostAndCapsResults(t *testing.T) {
	var requestInfo map[string]string
	var pages strings.Builder
	for i := 0; i < 13; i++ {
		if i > 0 {
			pages.WriteByte(',')
		}
		fmt.Fprintf(&pages, `"%02d":{"index":%d,"title":"File:result-%02d.gif","imageinfo":[{"url":"https://upload.wikimedia.org/wikipedia/commons/%02d/source.gif","thumburl":"https://upload.wikimedia.org/wikipedia/commons/thumb/%02d/160px-preview.gif","width":320,"height":240}]}`, i, i+1, i, i, i)
	}
	body := `{"continue":{"gsroffset":48},"query":{"pages":{` + pages.String() + `}}}`
	useGIFHTTPTransport(t, gifRoundTripper(func(request *http.Request) (*http.Response, error) {
		query := request.URL.Query()
		requestInfo = map[string]string{
			"host":   request.URL.Host,
			"path":   request.URL.Path,
			"search": query.Get("gsrsearch"),
			"limit":  query.Get("gsrlimit"),
			"offset": query.Get("gsroffset"),
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	}))

	application := &App{gifCache: make(map[string]gifResult)}
	result, err := application.gifSearch(context.Background(), []byte(`{"query":"cats & dogs","provider":"commons","offset":24}`))
	if err != nil {
		t.Fatal(err)
	}
	if requestInfo["host"] != "commons.wikimedia.org" || requestInfo["path"] != "/w/api.php" ||
		requestInfo["search"] != "cats & dogs filemime:image/gif" || requestInfo["limit"] != "12" || requestInfo["offset"] != "24" {
		t.Fatalf("unexpected bounded Wikimedia request: %#v", requestInfo)
	}
	response := result.(map[string]any)
	items := response["items"].([]map[string]any)
	if len(items) != 12 || response["has_more"] != true || response["next_offset"] != 48 {
		t.Fatalf("GIF search was not bounded/paginated: items=%d response=%#v", len(items), response)
	}
	for i, item := range items {
		if want := fmt.Sprintf("commons:%02d", i); item["id"] != want {
			t.Fatalf("search relevance order was not preserved at %d: got %v, want %s", i, item["id"], want)
		}
	}
}

func TestGIFSearchRejectsUnconfiguredProviderWithoutNetwork(t *testing.T) {
	called := false
	useGIFHTTPTransport(t, gifRoundTripper(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, fmt.Errorf("unexpected network request")
	}))
	application := &App{gifCache: make(map[string]gifResult)}
	_, err := application.gifSearch(context.Background(), []byte(`{"query":"cats","provider":"giphy"}`))
	if err == nil {
		t.Fatal("unconfigured GIPHY provider was accepted")
	}
	if called {
		t.Fatal("unconfigured provider triggered a network request")
	}
}

func TestGIFPrepareStagesValidatedMediaPrivately(t *testing.T) {
	gifBytes := makeSyntheticGIF(t)
	useGIFHTTPTransport(t, gifRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "upload.wikimedia.org" {
			t.Fatalf("unexpected GIF download host %q", request.URL.Host)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(gifBytes)), Header: make(http.Header), Request: request}, nil
	}))
	application := &App{
		dir: t.TempDir(),
		gifCache: map[string]gifResult{
			"commons:42": {ID: "commons:42", Provider: "commons", SourceURL: "https://upload.wikimedia.org/wikipedia/commons/a/test.gif", Attribution: "Wikimedia Commons · CC BY"},
		},
		prepared: make(map[string]Attachment),
	}

	result, err := application.gifPrepare(context.Background(), []byte(`{"id":"commons:42","provider":"commons"}`))
	if err != nil {
		t.Fatal(err)
	}
	attachment, ok := result.(Attachment)
	if !ok {
		t.Fatalf("unexpected prepared GIF result type %T", result)
	}
	if attachment.Kind != "gif" || attachment.MIME != "image/gif" || attachment.Size != int64(len(gifBytes)) || attachment.SHA256 != sha256Hex(gifBytes) || attachment.Attribution != "Wikimedia Commons · CC BY" {
		t.Fatalf("prepared GIF metadata mismatch: %#v", attachment)
	}
	info, err := os.Stat(attachment.PreparedPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 || filepath.Base(filepath.Dir(attachment.PreparedPath)) != "prepared" {
		t.Fatalf("prepared GIF is not in a private cache: mode=%#o path=%q", info.Mode().Perm(), attachment.PreparedPath)
	}
}

func TestGIFPrepareRejectsRedirectWithoutFollowing(t *testing.T) {
	calls := 0
	useGIFHTTPTransport(t, gifRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://tracker.example/track.gif"}}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	}))
	application := &App{
		dir: t.TempDir(),
		gifCache: map[string]gifResult{
			"commons:42": {ID: "commons:42", Provider: "commons", SourceURL: "https://upload.wikimedia.org/wikipedia/commons/a/test.gif"},
		},
		prepared: make(map[string]Attachment),
	}
	_, err := application.gifPrepare(context.Background(), []byte(`{"id":"commons:42","provider":"commons"}`))
	if err == nil {
		t.Fatal("redirected GIF download should fail closed")
	}
	if calls != 1 {
		t.Fatalf("redirect target was contacted: %d requests", calls)
	}
}

func TestGIFSearchRejectsOversizedProviderResponse(t *testing.T) {
	body := strings.Repeat(" ", (192<<10)+1)
	useGIFHTTPTransport(t, gifRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	}))
	application := &App{gifCache: make(map[string]gifResult)}
	_, err := application.gifSearch(context.Background(), []byte(`{"query":"cats","provider":"commons"}`))
	if err == nil {
		t.Fatal("oversized provider response should be rejected")
	}
}

func TestGIFPrepareRejectsOversizedMediaBeforeStaging(t *testing.T) {
	oversized := make([]byte, (10<<20)+1)
	useGIFHTTPTransport(t, gifRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(oversized)), Header: make(http.Header), Request: request}, nil
	}))
	application := &App{
		dir: t.TempDir(),
		gifCache: map[string]gifResult{
			"commons:42": {ID: "commons:42", Provider: "commons", SourceURL: "https://upload.wikimedia.org/wikipedia/commons/a/test.gif"},
		},
		prepared: make(map[string]Attachment),
	}
	_, err := application.gifPrepare(context.Background(), []byte(`{"id":"commons:42","provider":"commons"}`))
	if err == nil {
		t.Fatal("oversized GIF should be rejected")
	}
	if len(application.prepared) != 0 {
		t.Fatal("oversized GIF was recorded as prepared")
	}
	if _, err := os.Stat(filepath.Join(application.dir, "media")); !os.IsNotExist(err) {
		t.Fatalf("oversized GIF created a media cache: err=%v", err)
	}
}

func TestPlainTruncatesMetadataAtUTF8Boundary(t *testing.T) {
	input := strings.Repeat("a", 159) + "€Z"
	got := plain(input)
	if !utf8.ValidString(got) {
		t.Fatalf("plain returned invalid UTF-8 after truncation: %q", got)
	}
	if want := strings.Repeat("a", 159); got != want {
		t.Fatalf("plain = %q, want %q", got, want)
	}
}
