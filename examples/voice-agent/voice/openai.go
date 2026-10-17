package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

const (
	TranscribeModel = "gpt-transcribe"
	ReplyModel      = "gpt-6-luna"
	SpeechModel     = "gpt-4o-mini-tts"
)

// ProviderError exposes only an allow-listed app code. Never wrap a transport
// error, response body, request, or header: those can contain credentials.
type ProviderError struct {
	Code      string
	Uncertain bool
}

func (e *ProviderError) Error() string { return e.Code }

type OpenAI struct {
	key    string
	client *http.Client
	origin string
}

func NewOpenAI(key string) *OpenAI {
	return &OpenAI{key: key, client: &http.Client{Timeout: 40 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, origin: "https://api.openai.com/v1"}
}
func (p *OpenAI) Available() bool { return p != nil && p.key != "" }
func (p *OpenAI) request(ctx context.Context, path, kind, key string, b []byte) (io.ReadCloser, error) {
	if !p.Available() {
		return nil, &ProviderError{Code: "provider_unavailable"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.origin+path, bytes.NewReader(b))
	if err != nil {
		return nil, &ProviderError{Code: "provider_refused"}
	}
	req.Header.Set("Authorization", "Bearer "+p.key)
	req.Header.Set("Content-Type", kind)
	req.Header.Set("Idempotency-Key", key)
	response, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &ProviderError{Code: "provider_uncertain", Uncertain: true}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		uncertain := response.StatusCode == 408 || response.StatusCode == 429 || response.StatusCode >= 500
		code := "provider_refused"
		if uncertain {
			code = "provider_uncertain"
		}
		return nil, &ProviderError{Code: code, Uncertain: uncertain}
	}
	return response.Body, nil
}
func decodeResponse(body io.ReadCloser, value any) error {
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, 16<<10+1))
	if err != nil {
		return &ProviderError{Code: "provider_uncertain", Uncertain: true}
	}
	if len(data) > 16<<10 || json.Unmarshal(data, value) != nil {
		return &ProviderError{Code: "provider_refused"}
	}
	return nil
}
func (p *OpenAI) Transcribe(ctx context.Context, key string, ogg []byte) (string, error) {
	var b bytes.Buffer
	form := multipart.NewWriter(&b)
	digest := sha256.Sum256([]byte(key))
	if err := form.SetBoundary("relais-" + hex.EncodeToString(digest[:16])); err != nil {
		return "", &ProviderError{Code: "provider_refused"}
	}
	file, err := form.CreateFormFile("file", "recording.ogg")
	if err != nil {
		return "", &ProviderError{Code: "provider_refused"}
	}
	if _, err = file.Write(ogg); err != nil {
		return "", &ProviderError{Code: "provider_refused"}
	}
	if err = errors.Join(form.WriteField("model", TranscribeModel), form.WriteField("response_format", "json"), form.Close()); err != nil {
		return "", &ProviderError{Code: "provider_refused"}
	}
	body, err := p.request(ctx, "/audio/transcriptions", form.FormDataContentType(), key, b.Bytes())
	if err != nil {
		return "", err
	}
	var result struct {
		Text string `json:"text"`
	}
	err = decodeResponse(body, &result)
	return result.Text, err
}
func (p *OpenAI) Reply(ctx context.Context, key, transcript string, history []Exchange) (string, error) {
	messages := []map[string]string{{"role": "system", "content": "You are the Relais voice demo. Reply in one or two short spoken sentences, at most 35 words. Do not use markdown."}}
	for _, h := range history {
		messages = append(messages, map[string]string{"role": "user", "content": h.User}, map[string]string{"role": "assistant", "content": h.Assistant})
	}
	messages = append(messages, map[string]string{"role": "user", "content": transcript})
	b, _ := json.Marshal(map[string]any{"model": ReplyModel, "messages": messages, "max_completion_tokens": 128, "reasoning_effort": "none", "store": false})
	body, err := p.request(ctx, "/chat/completions", "application/json", key, b)
	if err != nil {
		return "", err
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err = decodeResponse(body, &result); err != nil {
		return "", err
	}
	if len(result.Choices) != 1 {
		return "", &ProviderError{Code: "provider_refused"}
	}
	return result.Choices[0].Message.Content, nil
}
func (p *OpenAI) Speech(ctx context.Context, key, text string) (io.ReadCloser, error) {
	b, _ := json.Marshal(map[string]any{"model": SpeechModel, "input": text, "voice": "coral", "response_format": "opus"})
	return p.request(ctx, "/audio/speech", "application/json", key, b)
}
