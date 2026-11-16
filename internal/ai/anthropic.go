package ai

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/gauravthakur02/kubesage/internal/config"
)

type claude struct {
	name, model string
	maxTokens   int64
	client      anthropic.Client
}

func newAnthropic(name string, p config.AIProfile) (*claude, error) {
	var opts []option.RequestOption
	if k := p.Key(); k != "" {
		opts = append(opts, option.WithAPIKey(k))
	}
	if p.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(p.BaseURL))
	}
	c := &claude{name: name, model: p.Model, maxTokens: int64(p.MaxTokens), client: anthropic.NewClient(opts...)}
	if c.model == "" {
		c.model = "claude-opus-5-5"
	}
	if c.maxTokens <= 0 {
		c.maxTokens = 64000
	}
	return c, nil
}

func (c *claude) Name() string  { return c.name }
func (c *claude) Model() string { return c.model }

func (c *claude) Stream(ctx context.Context, system string, msgs []Message, out chan<- string) error {
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: c.maxTokens,
		System:    []anthropic.BetaTextBlockParam{{Text: system}},
		// Server-side refusal fallback: if a safety classifier declines, the API
		// re-serves the request on a fallback model within the same call.
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
		Fallbacks: anthropic.BetaFallbacksParamOfDefault(),
	}
	for _, m := range msgs {
		if m.Role == "assistant" {
			params.Messages = append(params.Messages, anthropic.BetaMessageParam{
				OfAssistant: &anthropic.BetaAssistantMessageParam{
					Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(m.Content)},
				},
			})
			continue
		}
		params.Messages = append(params.Messages, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(m.Content)))
	}

	stream := c.client.Beta.Messages.NewStreaming(ctx, params)
	defer stream.Close()
	refused := false
	for stream.Next() {
		switch ev := stream.Current().AsAny().(type) {
		case anthropic.BetaRawContentBlockDeltaEvent:
			if d, ok := ev.Delta.AsAny().(anthropic.BetaTextDelta); ok {
				if err := send(ctx, out, d.Text); err != nil {
					return err
				}
			}
		case anthropic.BetaRawMessageDeltaEvent:
			if ev.Delta.StopReason == anthropic.BetaStopReasonRefusal {
				refused = true
			}
		}
	}
	if err := stream.Err(); err != nil {
		return err
	}
	if refused {
		return fmt.Errorf("the model declined this request")
	}
	return nil
}
