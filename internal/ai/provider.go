// Package ai contains pluggable LLM backends used to explain cluster problems.
package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gauravthakur02/kubeweave/internal/config"
)

// Message is one turn of a chat conversation.
type Message struct {
	Role    string // "user" or "assistant"
	Content string
}

// Provider streams a completion for a conversation. Chunks of text are sent to out;
// the implementation must not close out.
type Provider interface {
	Name() string
	Model() string
	Stream(ctx context.Context, system string, msgs []Message, out chan<- string) error
}

// New builds the provider for a profile.
func New(name string, p config.AIProfile) (Provider, error) {
	switch strings.ToLower(p.Provider) {
	case "ollama":
		return newOllama(name, p), nil
	case "anthropic", "claude":
		return newAnthropic(name, p)
	case "openai", "openai-compatible", "groq", "openrouter", "lmstudio", "vllm", "azure":
		return newOpenAI(name, p)
	case "gemini", "google":
		return newGemini(name, p)
	default:
		return nil, fmt.Errorf("unknown AI provider %q (use ollama, anthropic, openai, openai-compatible, azure, gemini)", p.Provider)
	}
}

var httpClient = &http.Client{Timeout: 10 * time.Minute}

func postJSON(ctx context.Context, url string, headers map[string]string, body any) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

// readSSE calls fn with the payload of every "data:" line until the stream ends.
func readSSE(r io.Reader, fn func(data string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		if err := fn(data); err != nil {
			return err
		}
	}
	return sc.Err()
}

func send(ctx context.Context, out chan<- string, s string) error {
	if s == "" {
		return nil
	}
	select {
	case out <- s:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
