package artifacts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const api = "https://api.github.com/repos/relaishq/relais"

var tagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
var assetPattern = regexp.MustCompile(`^(checksums\.txt|relais_[0-9]+\.[0-9]+\.[0-9]+_(linux|darwin)_(arm64|amd64)\.tar\.gz)$`)
var pagePattern = regexp.MustCompile(`^[1-9][0-9]*$`)

// PublicReader has no ambient credential lookup. An optional job token is
// restricted to the fixed API path; downloads and redirect children omit it.
type PublicReader struct {
	client http.Client
	token  string
	mu     sync.Mutex
	ids    map[string]int64
}

func NewPublicReader(client *http.Client, token string) *PublicReader {
	c := http.Client{}
	if client != nil {
		c = *client
	}
	c.Jar = nil
	previous := c.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("release redirect limit")
		}
		if previous != nil {
			if err := previous(req, via); err != nil {
				return err
			}
		}
		if req.URL.Scheme != "https" || req.URL.Host != "api.github.com" || !strings.HasPrefix(req.URL.Path, "/repos/relaishq/relais/") || len(via) > 0 && via[len(via)-1].Header.Get("Authorization") == "" {
			req.Header.Del("Authorization")
		}
		return nil
	}
	return &PublicReader{client: c, token: token, ids: map[string]int64{}}
}

func (r *PublicReader) get(ctx context.Context, address string, limit int64, missing, binary bool) ([]byte, bool, error) {
	budget := 15 * time.Second
	if binary {
		budget = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/vnd.github.raw+json")
	if binary {
		req.Header.Set("Accept", "application/octet-stream")
	}
	req.Header.Set("User-Agent", "relais-gates")
	if strings.HasPrefix(address, api+"/") && r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	response, err := r.client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer response.Body.Close()
	if response.StatusCode == 429 || response.StatusCode == 403 && (response.Header.Get("X-RateLimit-Remaining") == "0" || response.Header.Get("Retry-After") != "") {
		return nil, false, refuse("read-rate-limited", "public release API rate limit")
	}
	if missing && response.StatusCode == 404 {
		return nil, true, nil
	}
	if response.StatusCode != 200 {
		return nil, false, fmt.Errorf("public release read failed")
	}
	if response.ContentLength > limit {
		return nil, false, fmt.Errorf("release body too large")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return nil, false, fmt.Errorf("release body too large")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return data, false, nil
}

func (r *PublicReader) Release(ctx context.Context, tag string) (ReleaseReply, error) {
	if !tagPattern.MatchString(tag) {
		return ReleaseReply{}, fmt.Errorf("invalid release tag")
	}
	data, missing, err := r.get(ctx, api+"/releases/tags/"+tag, 1<<20, true, false)
	if err != nil {
		return ReleaseReply{}, err
	}
	if missing {
		return ReleaseReply{State: "not-found", Tag: tag}, nil
	}
	var v struct {
		ID         *int64  `json:"id"`
		Tag        *string `json:"tag_name"`
		Draft      *bool   `json:"draft"`
		Prerelease *bool   `json:"prerelease"`
	}
	if !utf8.Valid(data) {
		return ReleaseReply{}, fmt.Errorf("invalid release JSON text")
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return ReleaseReply{}, err
	}
	if v.ID == nil || v.Tag == nil || v.Draft == nil || v.Prerelease == nil || *v.ID <= 0 || *v.ID > 1<<53-1 || *v.Tag != tag {
		return ReleaseReply{}, fmt.Errorf("invalid release metadata")
	}
	data, _, err = r.get(ctx, api+"/commits/"+tag, 1<<20, false, false)
	if err != nil {
		return ReleaseReply{}, err
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(data, &commit); err != nil || !utf8.Valid(data) || !commitPattern.MatchString(commit.SHA) {
		return ReleaseReply{}, fmt.Errorf("missing tag commit")
	}
	r.mu.Lock()
	r.ids[tag] = *v.ID
	r.mu.Unlock()
	return ReleaseReply{State: "found", Release: Release{tag, *v.Draft, *v.Prerelease, commit.SHA}}, nil
}

func (r *PublicReader) Assets(ctx context.Context, tag, token string) (AssetPage, error) {
	r.mu.Lock()
	id, ok := r.ids[tag]
	r.mu.Unlock()
	page := 1
	if token != "" {
		if !pagePattern.MatchString(token) {
			return AssetPage{}, fmt.Errorf("invalid asset page")
		}
		var err error
		page, err = strconv.Atoi(token)
		if err != nil {
			return AssetPage{}, err
		}
	}
	if !ok || !tagPattern.MatchString(tag) || page > MaxAssetPages {
		return AssetPage{}, fmt.Errorf("asset page limit")
	}
	data, _, err := r.get(ctx, fmt.Sprintf("%s/releases/%d/assets?per_page=100&page=%d", api, id, page), 1<<20, false, false)
	if err != nil {
		return AssetPage{}, err
	}
	var rows []Asset
	if err := json.Unmarshal(data, &rows); err != nil || !utf8.Valid(data) || rows == nil || len(rows) > 100 {
		return AssetPage{}, fmt.Errorf("invalid asset listing")
	}
	next := ""
	if len(rows) == 100 {
		next = strconv.Itoa(page + 1)
	}
	return AssetPage{Tag: tag, Token: token, NextToken: next, Assets: rows, Complete: len(rows) < 100}, nil
}

func (r *PublicReader) Download(ctx context.Context, tag string, a Asset) (Download, error) {
	r.mu.Lock()
	_, ok := r.ids[tag]
	r.mu.Unlock()
	if !ok || !tagPattern.MatchString(tag) || !assetPattern.MatchString(a.Name) {
		return Download{}, fmt.Errorf("invalid public release asset")
	}
	limit := int64(MaxArchiveBytes)
	if a.Name == "checksums.txt" {
		limit = MaxChecksumsBytes
	}
	if a.Size < 0 || a.Size > limit {
		return Download{}, fmt.Errorf("invalid asset size")
	}
	data, _, err := r.get(ctx, "https://github.com/relaishq/relais/releases/download/"+tag+"/"+a.Name, limit, false, true)
	if err != nil {
		return Download{}, err
	}
	if int64(len(data)) != a.Size || a.Digest != "" && a.Digest != "sha256:"+digest(data) {
		return Download{}, fmt.Errorf("asset size or digest differs")
	}
	return Download{tag, a.ID, data}, nil
}

func (r *PublicReader) File(ctx context.Context, commit, path string) (File, error) {
	if !commitPattern.MatchString(commit) || path != "scripts/release/compatibility.json" && path != "scripts/release/compatibility-policy.json" {
		return File{}, fmt.Errorf("invalid pinned release file")
	}
	data, missing, err := r.get(ctx, api+"/contents/"+path+"?ref="+url.QueryEscape(commit), MaxManifestBytes, true, false)
	if err != nil {
		return File{}, err
	}
	state := "found"
	if missing {
		state = "not-found"
	}
	return File{state, commit, path, data}, nil
}
