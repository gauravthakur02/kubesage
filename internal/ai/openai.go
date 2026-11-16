package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gauravthakur02/kubesage/internal/config"
)

// openAI speaks the Chat Completions wire format, which covers OpenAI, Azure OpenAI,
// and OpenAI-compatible servers (LM Studio, vLLM, Groq, OpenRouter, LocalAI...).
type openAI struct {
	name, url, model, key string
	azure                 bool
	maxTokens             int
}

func newOpenAI(name string, p config.AIProfile) (*openAI, error) {
	o := &openAI{name: name, model: p.Model, key: p.Key(), maxTokens: p.MaxTokens}
	base := strings.TrimRight(p.BaseURL, "/")
	switch strings.ToLower(p.Provider) {
	case "azure":
		if base == "" || p.Model == "" {
			return nil, fmt.Errorf("azure profile needs base_url (https://<resource>.openai.azure.com) and model (deployment name)")
		}
		ver := p.APIVersion
		if ver == "" {
			ver = "2024-10-21"
		}
		o.azure = true
		o.url = fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s", base, p.Model, ver)
	default:
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		o.url = base + "/chat/completions"
	}
	if o.model == "" {
		o.model = "gpt-4o-mini"
	}
	return o, nil
}

func (o *openAI) Name() string  { return o.name }
func (o *openAI) Model() string { return o.model }

func (o *openAI) Stream(ctx context.Context, system string, msgs []Message, out chan<- string) error {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	body := map[string]any{"model": o.model, "stream": true}
	if o.maxTokens > 0 {
		body["max_tokens"] = o.maxTokens
	}
	all := []msg{{"system", system}}
	for _, m := range msgs {
		all = append(all, msg{m.Role, m.Content})
	}
	body["messages"] = all

	headers := map[string]string{"Accept": "text/event-stream"}
	if o.key != "" {
		if o.azure {
			headers["api-key"] = o.key
		} else {
			headers["Authorization"] = "Bearer " + o.key
		}
	}
	resp, err := postJSON(ctx, o.url, headers, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return readSSE(resp.Body, func(data string) error {
		var ev struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return nil
		}
		if ev.Error != nil {
			return fmt.Errorf("%s", ev.Error.Message)
		}
		for _, c := range ev.Choices {
			if err := send(ctx, out, c.Delta.Content); err != nil {
				return err
			}
		}
		return nil
	})
}
