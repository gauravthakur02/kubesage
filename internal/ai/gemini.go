package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gauravthakur02/kubesage/internal/config"
)

type gemini struct {
	name, base, model, key string
}

func newGemini(name string, p config.AIProfile) (*gemini, error) {
	g := &gemini{name: name, model: p.Model, key: p.Key(), base: strings.TrimRight(p.BaseURL, "/")}
	if g.base == "" {
		g.base = "https://generativelanguage.googleapis.com/v1beta"
	}
	if g.model == "" {
		g.model = "gemini-2.0-flash"
	}
	if g.key == "" {
		return nil, fmt.Errorf("gemini profile %q has no API key (set api_key_env)", name)
	}
	return g, nil
}

func (g *gemini) Name() string  { return g.name }
func (g *gemini) Model() string { return g.model }

func (g *gemini) Stream(ctx context.Context, system string, msgs []Message, out chan<- string) error {
	type part struct {
		Text string `json:"text"`
	}
	type content struct {
		Role  string `json:"role,omitempty"`
		Parts []part `json:"parts"`
	}
	body := struct {
		System   content   `json:"systemInstruction"`
		Contents []content `json:"contents"`
	}{System: content{Parts: []part{{system}}}}
	for _, m := range msgs {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		body.Contents = append(body.Contents, content{Role: role, Parts: []part{{m.Content}}})
	}
	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", g.base, g.model)
	resp, err := postJSON(ctx, url, map[string]string{"x-goog-api-key": g.key}, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return readSSE(resp.Body, func(data string) error {
		var ev struct {
			Candidates []struct {
				Content content `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return nil
		}
		for _, c := range ev.Candidates {
			for _, p := range c.Content.Parts {
				if err := send(ctx, out, p.Text); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
