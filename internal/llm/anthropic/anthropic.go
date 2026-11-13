// Package anthropic implements llm.Provider on the Claude Messages API using
// the official Anthropic Go SDK, with streaming, adaptive thinking, prompt
// caching, eager tool-input streaming and server-side refusal fallbacks.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// ProviderName is stored on messages this provider produced.
const ProviderName = "anthropic"

// Options configure the provider.
type Options struct {
	// APIKey overrides credential discovery. Empty lets the SDK resolve
	// ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN or an `ant auth login` profile.
	APIKey  string
	BaseURL string
	// WebSearch and WebFetch enable Anthropic's server-side web tools.
	WebSearch bool
	WebFetch  bool
	// Compaction enables server-side context compaction for long sessions.
	Compaction bool
	// Fallbacks enables server-side refusal fallbacks ("default" routing).
	Fallbacks  bool
	MaxRetries int
}

// Provider talks to the Claude API.
type Provider struct {
	client anthropic.Client
	opts   Options
}

// New builds a provider.
func New(opts Options) *Provider {
	var ro []option.RequestOption
	if opts.APIKey != "" {
		ro = append(ro, option.WithAPIKey(opts.APIKey))
	}
	if opts.BaseURL != "" {
		ro = append(ro, option.WithBaseURL(opts.BaseURL))
	}
	if opts.MaxRetries > 0 {
		ro = append(ro, option.WithMaxRetries(opts.MaxRetries))
	}
	return &Provider{client: anthropic.NewClient(ro...), opts: opts}
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return ProviderName }

// BuildParams converts a provider-neutral request to SDK params. It is
// exported for tests.
func (p *Provider) BuildParams(req llm.Request) (anthropic.BetaMessageNewParams, error) {
	msgs, err := ToParams(req.Messages)
	if err != nil {
		return anthropic.BetaMessageNewParams{}, err
	}
	params := anthropic.BetaMessageNewParams{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		Messages:  msgs,
		// Automatic caching on the last cacheable block keeps the growing
		// conversation prefix warm between agent turns.
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
	}
	if req.System != "" {
		params.System = []anthropic.BetaTextBlockParam{{
			Text:         req.System,
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		}}
	}
	display := anthropic.BetaThinkingConfigAdaptiveDisplayOmitted
	if req.ShowThinking {
		display = anthropic.BetaThinkingConfigAdaptiveDisplaySummarized
	}
	params.Thinking = anthropic.BetaThinkingConfigParamUnion{
		OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{Display: display},
	}
	if req.Effort != "" {
		params.OutputConfig = anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(req.Effort)}
	}
	for _, t := range req.Tools {
		tp := anthropic.BetaToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: anthropic.BetaToolInputSchemaParam{
				Properties: t.Properties,
				Required:   t.Required,
			},
			// Large file bodies stream as they are generated; every tool
			// validates its decoded input before running.
			EagerInputStreaming: anthropic.Bool(true),
		}
		params.Tools = append(params.Tools, anthropic.BetaToolUnionParam{OfTool: &tp})
	}
	if p.opts.WebSearch {
		params.Tools = append(params.Tools, anthropic.BetaToolUnionParam{OfWebSearchTool20260209: &anthropic.BetaWebSearchTool20260209Param{}})
	}
	if p.opts.WebFetch {
		params.Tools = append(params.Tools, anthropic.BetaToolUnionParam{OfWebFetchTool20260209: &anthropic.BetaWebFetchTool20260209Param{}})
	}
	if p.opts.Fallbacks {
		params.Betas = append(params.Betas, anthropic.AnthropicBetaServerSideFallback2026_07_01)
		params.Fallbacks = anthropic.BetaFallbacksParamOfDefault()
	}
	if p.opts.Compaction {
		params.Betas = append(params.Betas, anthropic.AnthropicBetaCompact2026_01_12)
		params.ContextManagement = anthropic.BetaContextManagementConfigParam{
			Edits: []anthropic.BetaContextManagementConfigEditUnionParam{
				{OfCompact20260112: &anthropic.BetaCompact20260112EditParam{}},
			},
		}
	}
	return params, nil
}

// ToParams converts conversation history to SDK message params. Assistant
// messages produced by this provider replay their exact native content so
// thinking blocks, server tool results and compaction blocks round-trip
// unchanged.
func ToParams(history []llm.Message) ([]anthropic.BetaMessageParam, error) {
	out := make([]anthropic.BetaMessageParam, 0, len(history))
	for i, m := range history {
		switch m.Role {
		case llm.RoleAssistant:
			if m.Provider == ProviderName && len(m.Native) > 0 {
				var blocks []anthropic.BetaContentBlockParamUnion
				if err := json.Unmarshal(m.Native, &blocks); err != nil {
					return nil, fmt.Errorf("message %d: decode native content: %w", i, err)
				}
				out = append(out, assistant(blocks))
				continue
			}
			var blocks []anthropic.BetaContentBlockParamUnion
			for _, b := range m.Blocks {
				switch b.Type {
				case llm.BlockText:
					if b.Text != "" {
						blocks = append(blocks, anthropic.NewBetaTextBlock(b.Text))
					}
				case llm.BlockToolUse:
					var input any = map[string]any{}
					if len(b.Input) > 0 {
						if err := json.Unmarshal(b.Input, &input); err != nil {
							input = map[string]any{}
						}
					}
					blocks = append(blocks, anthropic.NewBetaToolUseBlock(b.ToolUseID, input, b.ToolName))
				}
			}
			if len(blocks) == 0 {
				blocks = append(blocks, anthropic.NewBetaTextBlock("(no content)"))
			}
			out = append(out, assistant(blocks))
		default:
			var blocks []anthropic.BetaContentBlockParamUnion
			for _, b := range m.Blocks {
				switch b.Type {
				case llm.BlockText:
					if b.Text != "" {
						blocks = append(blocks, anthropic.NewBetaTextBlock(b.Text))
					}
				case llm.BlockImage:
					if b.Image == nil {
						return nil, fmt.Errorf("message %d: image block without an image", i)
					}
					src, err := imageSource(*b.Image)
					if err != nil {
						return nil, fmt.Errorf("message %d: %w", i, err)
					}
					blocks = append(blocks, anthropic.NewBetaImageBlock(src))
				case llm.BlockToolResult:
					tr, err := toolResult(b)
					if err != nil {
						return nil, fmt.Errorf("message %d: %w", i, err)
					}
					blocks = append(blocks, tr)
				}
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, anthropic.NewBetaUserMessage(blocks...))
		}
	}
	return out, nil
}

// imageSource resolves an image and converts it to a base64 source.
func imageSource(img llm.Image) (anthropic.BetaBase64ImageSourceParam, error) {
	r, err := img.Resolve()
	if err != nil {
		return anthropic.BetaBase64ImageSourceParam{}, err
	}
	return anthropic.BetaBase64ImageSourceParam{
		Data:      r.Data,
		MediaType: anthropic.BetaBase64ImageSourceMediaType(r.MediaType),
	}, nil
}

// toolResult converts a tool result. Images returned by the tool go inside
// the tool_result content, after its text, which the Messages API accepts.
func toolResult(b llm.Block) (anthropic.BetaContentBlockParamUnion, error) {
	text := b.Text
	if text == "" {
		text = "(no output)"
	}
	block := anthropic.NewBetaToolResultBlock(b.ToolUseID, text, b.IsError)
	for k, img := range b.Images {
		src, err := imageSource(img)
		if err != nil {
			return anthropic.BetaContentBlockParamUnion{}, fmt.Errorf("tool result %s image %d: %w", b.ToolUseID, k+1, err)
		}
		block.OfToolResult.Content = append(block.OfToolResult.Content, anthropic.BetaToolResultBlockParamContentUnion{
			OfImage: &anthropic.BetaImageBlockParam{Source: anthropic.BetaImageBlockParamSourceUnion{OfBase64: &src}},
		})
	}
	return block, nil
}

func assistant(blocks []anthropic.BetaContentBlockParamUnion) anthropic.BetaMessageParam {
	return anthropic.BetaMessageParam{OfAssistant: &anthropic.BetaAssistantMessageParam{Content: blocks}}
}

// Stream implements llm.Provider.
func (p *Provider) Stream(ctx context.Context, req llm.Request, onEvent func(llm.Event)) (*llm.Response, error) {
	params, err := p.BuildParams(req)
	if err != nil {
		return nil, err
	}
	if onEvent == nil {
		onEvent = func(llm.Event) {}
	}
	stream := p.client.Beta.Messages.NewStreaming(ctx, params)
	defer stream.Close()

	acc := anthropic.BetaMessage{}
	kinds := map[int64]string{}
	for stream.Next() {
		ev := stream.Current()
		if err := acc.Accumulate(ev); err != nil {
			return nil, fmt.Errorf("the model sent malformed streamed content: %w", err)
		}
		switch e := ev.AsAny().(type) {
		case anthropic.BetaRawContentBlockStartEvent:
			kinds[e.Index] = e.ContentBlock.Type
			switch e.ContentBlock.Type {
			case "tool_use":
				onEvent(llm.Event{Type: llm.EventToolUseStart, ToolName: e.ContentBlock.Name, ToolID: e.ContentBlock.ID})
			case "server_tool_use":
				onEvent(llm.Event{Type: llm.EventServerToolStart, ToolName: e.ContentBlock.Name, ToolID: e.ContentBlock.ID})
			}
		case anthropic.BetaRawContentBlockDeltaEvent:
			switch d := e.Delta.AsAny().(type) {
			case anthropic.BetaTextDelta:
				onEvent(llm.Event{Type: llm.EventTextDelta, Text: d.Text})
			case anthropic.BetaThinkingDelta:
				onEvent(llm.Event{Type: llm.EventThinkingDelta, Text: d.Thinking})
			case anthropic.BetaInputJSONDelta:
				if kinds[e.Index] == "tool_use" {
					onEvent(llm.Event{Type: llm.EventToolInputDelta, Bytes: len(d.PartialJSON)})
				}
			}
		case anthropic.BetaRawContentBlockStopEvent:
			onEvent(llm.Event{Type: llm.EventBlockStop})
		}
	}
	if err := stream.Err(); err != nil {
		return nil, describeError(err)
	}
	return FromMessage(acc)
}

// FromMessage converts a completed SDK message to the neutral form.
func FromMessage(acc anthropic.BetaMessage) (*llm.Response, error) {
	param := acc.ToParam()
	native, err := json.Marshal(param.OfAssistant.Content)
	if err != nil {
		return nil, fmt.Errorf("encode native content: %w", err)
	}
	msg := llm.Message{Role: llm.RoleAssistant, Provider: ProviderName, Native: native}
	for _, block := range acc.Content {
		switch b := block.AsAny().(type) {
		case anthropic.BetaTextBlock:
			msg.Blocks = append(msg.Blocks, llm.Block{Type: llm.BlockText, Text: b.Text})
		case anthropic.BetaThinkingBlock:
			if b.Thinking != "" {
				msg.Blocks = append(msg.Blocks, llm.Block{Type: llm.BlockThinking, Text: b.Thinking})
			}
		case anthropic.BetaToolUseBlock:
			raw := json.RawMessage(b.JSON.Input.Raw())
			if !json.Valid(raw) {
				enc, err := json.Marshal(b.Input)
				if err != nil {
					enc = []byte("{}")
				}
				raw = enc
			}
			msg.Blocks = append(msg.Blocks, llm.Block{Type: llm.BlockToolUse, ToolUseID: b.ID, ToolName: b.Name, Input: raw})
		case anthropic.BetaServerToolUseBlock:
			raw, _ := json.Marshal(b.Input)
			msg.Blocks = append(msg.Blocks, llm.Block{Type: llm.BlockServerTool, ToolUseID: b.ID, ToolName: string(b.Name), Input: raw})
		}
	}
	resp := &llm.Response{
		Message: msg,
		Model:   string(acc.Model),
		Usage: llm.Usage{
			InputTokens:      acc.Usage.InputTokens,
			OutputTokens:     acc.Usage.OutputTokens,
			CacheReadTokens:  acc.Usage.CacheReadInputTokens,
			CacheWriteTokens: acc.Usage.CacheCreationInputTokens,
		},
	}
	switch acc.StopReason {
	case anthropic.BetaStopReasonEndTurn, anthropic.BetaStopReasonStopSequence:
		resp.StopReason = llm.StopEndTurn
	case anthropic.BetaStopReasonToolUse:
		resp.StopReason = llm.StopToolUse
	case anthropic.BetaStopReasonMaxTokens, anthropic.BetaStopReasonModelContextWindowExceeded:
		resp.StopReason = llm.StopMaxTokens
	case anthropic.BetaStopReasonRefusal:
		resp.StopReason = llm.StopRefusal
		resp.StopDetail = strings.TrimSpace(acc.StopDetails.Explanation)
	case anthropic.BetaStopReasonPauseTurn, anthropic.BetaStopReasonCompaction:
		resp.StopReason = llm.StopPauseTurn
	default:
		resp.StopReason = llm.StopOther
	}
	return resp, nil
}

// describeError turns SDK errors into short, actionable messages.
func describeError(err error) error {
	var apierr *anthropic.Error
	if errors.As(err, &apierr) {
		switch apierr.StatusCode {
		case 401:
			return fmt.Errorf("authentication failed (401): set ANTHROPIC_API_KEY or run `ant auth login`")
		case 403:
			return fmt.Errorf("permission denied (403): %s", oneLine(apierr.Error()))
		case 404:
			return fmt.Errorf("not found (404), check the model name: %s", oneLine(apierr.Error()))
		case 429:
			return fmt.Errorf("rate limited (429) after retries, try again shortly")
		case 400:
			return fmt.Errorf("request rejected (400): %s", oneLine(apierr.Error()))
		}
		if apierr.StatusCode >= 500 {
			return fmt.Errorf("Claude API server error (%d), try again", apierr.StatusCode)
		}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return err
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}
