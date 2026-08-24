package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	anthropicAPIURL     = "https://api.anthropic.com/v1/messages"
	anthropicAPIVersion = "2023-06-01"
)

// Anthropic implements the Reviewer interface for Anthropic's API.
type Anthropic struct {
	apiKey string
	model  string
	client *http.Client
}

// NewAnthropic creates a new Anthropic provider.
func NewAnthropic(model string) (*Anthropic, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY environment variable is not set")
	}
	return &Anthropic{
		apiKey: key,
		model:  model,
		client: &http.Client{Timeout: requestTimeout(120 * time.Second)},
	}, nil
}

func (a *Anthropic) Name() string { return "anthropic" }

func (a *Anthropic) Review(ctx context.Context, req ReviewRequest) (ReviewResponse, error) {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	body := anthropicRequest{
		Model:     a.model,
		MaxTokens: maxTokens,
		System:    req.SystemPrompt,
		Messages: []anthropicMessage{
			{Role: "user", Content: req.UserPrompt},
		},
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return ReviewResponse{}, fmt.Errorf("marshaling request: %w", err)
	}

	var resp ReviewResponse
	err = retryWithBackoff(ctx, 3, func() error {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", anthropicAPIURL, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("creating request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("x-api-key", a.apiKey)
		httpReq.Header.Set("anthropic-version", anthropicAPIVersion)

		httpResp, err := a.client.Do(httpReq)
		if err != nil {
			return fmt.Errorf("sending request: %w", err)
		}
		defer func() { _ = httpResp.Body.Close() }()

		respBody, err := io.ReadAll(httpResp.Body)
		if err != nil {
			return fmt.Errorf("reading response: %w", err)
		}

		if httpResp.StatusCode == 429 {
			return &rateLimitError{}
		}
		if httpResp.StatusCode == 401 || httpResp.StatusCode == 403 {
			return &authError{message: string(respBody)}
		}
		if httpResp.StatusCode >= 500 {
			return &serverError{statusCode: httpResp.StatusCode, body: string(respBody)}
		}
		if httpResp.StatusCode != 200 {
			return fmt.Errorf("API error (status %d): %s", httpResp.StatusCode, string(respBody))
		}

		var result anthropicResponse
		if err := json.Unmarshal(respBody, &result); err != nil {
			return fmt.Errorf("parsing response: %w", err)
		}

		var content string
		for _, block := range result.Content {
			if block.Type == "text" {
				content += block.Text
			}
		}
		if content == "" {
			return fmt.Errorf("empty text content in API response")
		}

		resp = ReviewResponse{
			Content:    content,
			TokensUsed: result.Usage.InputTokens + result.Usage.OutputTokens,
			Provider:   a.Name(),
			Model:      a.model,
		}
		return nil
	})

	return resp, err
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []anthropicBlock `json:"content"`
	Usage   anthropicUsage   `json:"usage"`
}

type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
