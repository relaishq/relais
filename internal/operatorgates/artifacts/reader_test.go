package artifacts

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: -1}
}

func TestReleaseReaderTokenScope(t *testing.T) {
	var seen []string
	client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		seen = append(seen, req.URL.Host+" "+req.Header.Get("Authorization"))
		if req.URL.Host == "api.github.com" && !strings.HasSuffix(req.URL.Path, "/final") {
			r := response(302, "")
			r.Header.Set("Location", "https://assets.test/final")
			return r, nil
		}
		return response(200, "ok"), nil
	})}
	r := NewPublicReader(client, "test-job-token")
	if _, _, err := r.get(context.Background(), api+"/releases/tags/v0.3.0", 20, false, false); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "api.github.com Bearer test-job-token" || seen[1] != "assets.test " {
		t.Fatalf("release token crossed origin: %v", seen)
	}
	seen = nil
	if _, _, err := r.get(context.Background(), "https://github.com/relaishq/relais/releases/download/v0.3.0/checksums.txt", 20, false, true); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "github.com " {
		t.Fatalf("download received API token: %v", seen)
	}
}

func TestReleaseReaderBodyLimits(t *testing.T) {
	for _, length := range []int64{-1, 100} {
		r := NewPublicReader(&http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
			v := response(200, strings.Repeat("x", 100))
			v.ContentLength = length
			return v, nil
		})}, "")
		if _, _, err := r.get(context.Background(), api+"/releases/tags/v0.3.0", 10, false, false); err == nil {
			t.Fatal("streamed-release-without-byte-cap: oversized response accepted")
		}
	}
	r := NewPublicReader(&http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		v := response(403, "")
		v.Header.Set("X-RateLimit-Remaining", "0")
		return v, nil
	})}, "")
	if _, _, err := r.get(context.Background(), api+"/releases/tags/v0.3.0", 10, false, false); code(err) != "read-rate-limited" {
		t.Fatalf("rate limit classification: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r = NewPublicReader(&http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("unexpected request")
	})}, "")
	if _, _, err := r.get(ctx, api+"/releases/tags/v0.3.0", 10, false, false); err == nil {
		t.Fatal("expired read accepted")
	}
}

func TestReleaseReaderRequiredFields(t *testing.T) {
	for _, metadata := range []string{`{"id":1,"tag_name":"v0.3.0","prerelease":false}`, `{"id":1,"tag_name":"v0.3.0","draft":false}`, `{"id":1,"tag_name":"v0.3.0","draft":null,"prerelease":false}`} {
		r := NewPublicReader(&http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) { return response(200, metadata), nil })}, "")
		if _, err := r.Release(context.Background(), "v0.3.0"); err == nil {
			t.Fatal("missing release readiness flags accepted")
		}
	}
}
