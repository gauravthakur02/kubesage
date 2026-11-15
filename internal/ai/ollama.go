package ai

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"kubesage/internal/config"
)

type ollama struct {
	name, base, model string
}

func newOllama(name string, p config.AIProfile) *ollama {
	base := strings.TrimRight(p.BaseURL, "/")
	if base == "" {
		base = "http://localhost:11434"
	}
	return &ollama{name: name, base: base, model: p.Model}
}

func (o *ollama) Name() string { return o.name }
func (o *ollama) Model() string {
	if o.model == "" {
		return "auto"
	}
	return o.model
}

// pickModel uses the first locally installed model when none is configured.
func (o *ollama) pickModel(ctx context.Context) (string, error) {
	if o.model != "" {
		return o.model, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, o.base+"/api/tags", nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama not reachable at %s (is `ollama serve` running?): %w", o.base, err)
	}
	defer resp.Body.Close()
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return "", err
	}
	if len(tags.Models) == 0 {
		return "", fmt.Errorf("no ollama models installed; run e.g. `ollama pull llama3.1`")
	}
	o.model = tags.Models[0].Name
	return o.model, nil
}

func (o *ollama) Stream(ctx context.Context, system string, msgs []Message, out chan<- string) error {
	model, err := o.pickModel(ctx)
	if err != nil {
		return err
	}
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	body := struct {
		Model    string `json:"model"`
		Messages []msg  `json:"messages"`
		Stream   bool   `json:"stream"`
	}{Model: model, Stream: true}
	body.Messages = append(body.Messages, msg{"system", system})
	for _, m := range msgs {
		body.Messages = append(body.Messages, msg{m.Role, m.Content})
	}
	resp, err := postJSON(ctx, o.base+"/api/chat", nil, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var chunk struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Done  bool   `json:"done"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(sc.Bytes(), &chunk); err != nil {
			continue
		}
		if chunk.Error != "" {
			return fmt.Errorf("ollama: %s", chunk.Error)
		}
		if err := send(ctx, out, chunk.Message.Content); err != nil {
			return err
		}
		if chunk.Done {
			break
		}
	}
	return sc.Err()
}
