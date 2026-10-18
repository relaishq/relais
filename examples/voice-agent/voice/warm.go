package voice

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

func normalizeWarmMode(mode string) string {
	if mode == "" {
		return "auth"
	}
	return mode
}

// ValidateWarmMode rejects invalid process configuration before worker startup.
// The error omits the supplied value because environment values may be secrets.
func ValidateWarmMode(mode string) error {
	switch normalizeWarmMode(mode) {
	case "auth", "none":
		return nil
	default:
		return errors.New("invalid RELAIS_VOICE_WARM: expected auth or none")
	}
}

// WarmMode is process configuration only; it never enters state or registration.
func (p *OpenAI) WarmMode() string { return normalizeWarmMode(p.warm) }

// Warm performs three concurrent authenticated, non-billed model metadata reads
// to warm the provider after idle. RELAIS_VOICE_WARM=none disables these reads.
func (p *OpenAI) Warm(ctx context.Context) {
	if !p.Available() || strings.ContainsAny(p.key, "\r\n") {
		return
	}
	switch p.WarmMode() {
	case "none":
		return
	case "auth":
		var tasks sync.WaitGroup
		for _, model := range []struct{ name, stage string }{{TranscribeModel, "warm-transcribe"}, {ReplyModel, "warm-reply"}, {SpeechModel, "warm-speech"}} {
			tasks.Add(1)
			go func() {
				defer tasks.Done()
				p.warmRequest(ctx, "/models/"+model.name, model.stage)
			}()
		}
		tasks.Wait()
	}
}
func (p *OpenAI) warmRequest(ctx context.Context, path, stage string) {
	identity, _ := ctx.Value(requestIdentityKey{}).(requestIdentity)
	ctx = requestContext(ctx, identity.turn, stage)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(p.origin, "/")+path, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+p.key)
	req, trace := p.traceRequest(req)
	response, err := p.client.Do(req)
	if err != nil {
		trace.finish()
		return
	}
	response.Body = trace.response(response)
	defer func() { _ = response.Body.Close() }()
	// The status is traced, but every response (including non-2xx) is ignored.
	// No retry, no returned body, and no more than 8 KiB read per response.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8<<10))
}
