package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"image"
	_ "image/gif"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type gifResult struct {
	ID, Title, PreviewURL, SourceURL, Attribution string
	Width, Height                                 int
	Provider                                      string
}

func canonicalCommonsMediaURL(raw string, allowThumbnailHost bool) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.ForceQuery || u.Fragment != "" ||
		!strings.HasPrefix(u.Path, "/wikipedia/commons/") {
		return "", false
	}
	hostAllowed := u.Host == "upload.wikimedia.org" || (allowThumbnailHost && u.Host == "thumb.wikimedia.org")
	if !hostAllowed {
		return "", false
	}
	if u.RawQuery != "" {
		query, err := url.ParseQuery(u.RawQuery)
		if err != nil || len(query) != 3 || len(query["utm_campaign"]) != 1 || query.Get("utm_campaign") != "imageinfo" ||
			len(query["utm_source"]) != 1 || query.Get("utm_source") != "commons.wikimedia.org" || len(query["utm_content"]) != 1 {
			return "", false
		}
		content := query.Get("utm_content")
		if content != "original" && content != "thumbnail" && content != "thumbnail_unscaled" {
			return "", false
		}
	}
	u.RawQuery = ""
	return u.String(), true
}

func gifProviders() map[string]any {
	return map[string]any{"providers": []any{map[string]any{"id": "commons", "name": "Wikimedia Commons", "configured": true}, map[string]any{"id": "giphy", "name": "GIPHY", "configured": false}}, "default": "commons"}
}

type commonsImageInfo struct {
	URL    string `json:"url"`
	Thumb  string `json:"thumburl"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Ext    map[string]struct {
		Value json.RawMessage `json:"value"`
	} `json:"extmetadata"`
}

type commonsPage struct {
	Index     int                `json:"index"`
	Title     string             `json:"title"`
	ImageInfo []commonsImageInfo `json:"imageinfo"`
}

type commonsResponse struct {
	Continue struct {
		Offset int `json:"gsroffset"`
	} `json:"continue"`
	Query struct {
		Pages map[string]commonsPage `json:"pages"`
	} `json:"query"`
}

func (a *App) gifSearch(ctx context.Context, raw []byte) (any, error) {
	var p struct {
		Query    string `json:"query"`
		Provider string `json:"provider"`
		Offset   int    `json:"offset"`
	}
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	if p.Provider == "" {
		p.Provider = "commons"
	}
	if p.Provider != "commons" && p.Provider != "giphy" {
		return nil, fail("invalid_params", "Unknown GIF provider")
	}
	if utf8.RuneCountInString(p.Query) > 50 {
		return nil, fail("invalid_params", "GIF search query is too long")
	}
	if strings.TrimSpace(p.Query) == "" {
		return nil, fail("invalid_params", "GIF search query is empty")
	}
	if p.Offset < 0 || p.Offset > 1000 {
		return nil, fail("invalid_params", "GIF offset is out of range")
	}
	if p.Provider == "giphy" {
		return nil, fail("provider_not_configured", "GIPHY is unavailable; use Wikimedia Commons")
	}
	u := "https://commons.wikimedia.org/w/api.php?action=query&format=json&generator=search&gsrnamespace=6&gsrsearch=" + url.QueryEscape(p.Query+" filemime:image/gif") + "&gsrlimit=12&prop=imageinfo&iiprop=url|size|mime|extmetadata&iiurlwidth=160"
	if p.Offset > 0 {
		u += fmt.Sprintf("&gsroffset=%d", p.Offset)
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if e != nil {
		return nil, fail("provider_unavailable", "GIF provider request failed")
	}
	req.Header.Set("User-Agent", "WhatsFeather/0.0.1 (https://github.com/SayyakuHajime/whatsfeather)")
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return fmt.Errorf("redirect rejected") }}
	resp, e := client.Do(req)
	if e != nil {
		return nil, fail("provider_unavailable", "Wikimedia Commons is unavailable; retry explicitly")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fail("provider_unavailable", "Wikimedia Commons returned an error")
	}
	body, e := io.ReadAll(io.LimitReader(resp.Body, 192<<10))
	if e != nil {
		return nil, fail("provider_unavailable", "GIF provider response could not be read")
	}
	var parsed commonsResponse
	if json.Unmarshal(body, &parsed) != nil {
		return nil, fail("provider_unavailable", "GIF provider response was invalid")
	}
	pageIDs := make([]string, 0, len(parsed.Query.Pages))
	for id := range parsed.Query.Pages {
		pageIDs = append(pageIDs, id)
	}
	sort.Slice(pageIDs, func(i, j int) bool {
		left, right := parsed.Query.Pages[pageIDs[i]], parsed.Query.Pages[pageIDs[j]]
		if left.Index != right.Index {
			return left.Index < right.Index
		}
		return pageIDs[i] < pageIDs[j]
	})
	items := []map[string]any{}
	a.gifMu.Lock()
	defer a.gifMu.Unlock()
	for _, id := range pageIDs {
		page := parsed.Query.Pages[id]
		if len(items) >= 12 {
			break
		}
		if len(page.ImageInfo) == 0 {
			continue
		}
		ii := page.ImageInfo[0]
		sourceURL, sourceOK := canonicalCommonsMediaURL(ii.URL, false)
		previewURL, previewOK := canonicalCommonsMediaURL(ii.Thumb, true)
		if !sourceOK || !previewOK || ii.Width < 1 || ii.Height < 1 || int64(ii.Width)*int64(ii.Height) > maxPixels {
			continue
		}
		title := strings.TrimSpace(plain(page.Title))
		attribution := "Wikimedia Commons"
		if v := metadataValue(ii.Ext["Artist"].Value); v != "" {
			attribution += " · " + plain(v)
		}
		if v := metadataValue(ii.Ext["LicenseShortName"].Value); v != "" {
			attribution += " · " + plain(v)
		}
		r := gifResult{ID: "commons:" + id, Title: title, PreviewURL: previewURL, SourceURL: sourceURL, Provider: "commons", Attribution: attribution, Width: ii.Width, Height: ii.Height}
		if len(a.gifCache) >= 256 {
			for old := range a.gifCache {
				delete(a.gifCache, old)
				break
			}
		}
		a.gifCache[r.ID] = r
		items = append(items, map[string]any{"id": r.ID, "title": r.Title, "preview_url": r.PreviewURL, "source_url": r.SourceURL, "provider": r.Provider, "attribution": r.Attribution, "width": r.Width, "height": r.Height})
	}
	next := parsed.Continue.Offset
	if next == 0 {
		next = p.Offset + len(items)
	}
	return map[string]any{"items": items, "provider": "commons", "has_more": parsed.Continue.Offset > 0, "next_offset": next}, nil
}
func metadataValue(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}
func plain(s string) string {
	// Commons extmetadata is often literal HTML; never expose markup as attribution.
	var text strings.Builder
	text.Grow(len(s))
	inTag := false
	for _, r := range s {
		switch r {
		case '<':
			if !inTag {
				text.WriteByte(' ')
				inTag = true
			}
		case '>':
			if inTag {
				inTag = false
			} else {
				text.WriteRune(r)
			}
		default:
			if !inTag {
				text.WriteRune(r)
			}
		}
	}
	s = strings.Join(strings.Fields(html.UnescapeString(text.String())), " ")
	if len(s) > 160 {
		end := 160
		for !utf8.ValidString(s[:end]) {
			end--
		}
		s = s[:end]
	}
	return s
}
func (a *App) gifPrepare(ctx context.Context, raw []byte) (any, error) {
	var p struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
	}
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	if p.Provider != "commons" {
		return nil, fail("invalid_params", "Unsupported GIF provider")
	}
	a.gifMu.Lock()
	r, ok := a.gifCache[p.ID]
	a.gifMu.Unlock()
	if !ok {
		return nil, fail("retryable", "GIF result expired; search again")
	}
	sourceURL, ok := canonicalCommonsMediaURL(r.SourceURL, false)
	if !ok {
		return nil, fail("provider_unavailable", "GIF source host is not allowed")
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if e != nil {
		return nil, fail("provider_unavailable", "GIF download failed")
	}
	req.Header.Set("User-Agent", "WhatsFeather/0.0.1 (https://github.com/SayyakuHajime/whatsfeather)")
	resp, e := (&http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return fmt.Errorf("redirect rejected") }}).Do(req)
	if e != nil {
		return nil, fail("provider_unavailable", "GIF download failed; retry explicitly")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fail("provider_unavailable", "GIF download returned an error")
	}
	data, e := io.ReadAll(io.LimitReader(resp.Body, 10<<20+1))
	if e != nil || len(data) > 10<<20 {
		return nil, fail("media_too_large", "GIF exceeds 10 MiB")
	}
	if len(data) < 6 || (string(data[:6]) != "GIF87a" && string(data[:6]) != "GIF89a") {
		return nil, fail("invalid_attachment", "GIF source is not a valid GIF")
	}
	cfg, _, decodeErr := image.DecodeConfig(bytes.NewReader(data))
	if decodeErr != nil || cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return nil, fail("invalid_attachment", "GIF dimensions exceed the safe limit")
	}
	path, e := a.stagePrepared(ctx, data, "gif.gif")
	if e != nil {
		return nil, e
	}
	m := Attachment{Name: "gif.gif", Size: int64(len(data)), OriginalSize: int64(len(data)), PreparedSize: int64(len(data)), Mode: "original", MIME: "image/gif", Kind: "gif", URL: fileURL(path), PreparedPath: path, State: "ready", Attribution: r.Attribution, SourceURL: r.SourceURL, Width: cfg.Width, Height: cfg.Height, SHA256: sha256Hex(data)}
	a.mediaMu.Lock()
	a.prepared[path] = m
	a.mediaMu.Unlock()
	return m, nil
}
