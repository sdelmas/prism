package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultOpenAIURL = "https://api.openai.com/v1/chat/completions"

// OpenAI implements the Reviewer interface for OpenAI's API.
type OpenAI struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

// NewOpenAI creates a new OpenAI provider.
func NewOpenAI(model string) (*OpenAI, error) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY environment variable is not set")
	}
	baseURL := os.Getenv("PRISM_OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = defaultOpenAIURL
	}
	return &OpenAI{
		apiKey:  key,
		model:   model,
		baseURL: baseURL,
		client:  &http.Client{Timeout: requestTimeout(120 * time.Second)},
	}, nil
}

func (o *OpenAI) Name() string { return "openai" }

func (o *OpenAI) Review(ctx context.Context, req ReviewRequest) (ReviewResponse, error) {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	messages := []openaiMessage{
		{Role: "system", Content: req.SystemPrompt},
		{Role: "user", Content: req.UserPrompt},
	}

	body := openaiRequest{
		Model:    o.model,
		Messages: messages,
	}
	// GPT-5.x and o-series models require max_completion_tokens instead of max_tokens
	if usesMaxCompletionTokens(o.model) {
		body.MaxCompletionTokens = maxTokens
	} else {
		body.MaxTokens = maxTokens
	}
	if req.Temperature > 0 {
		body.Temperature = &req.Temperature
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return ReviewResponse{}, fmt.Errorf("marshaling request: %w", err)
	}

	var resp ReviewResponse
	err = retryWithBackoff(ctx, 3, func() error {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", o.baseURL, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("creating request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

		httpResp, err := o.client.Do(httpReq)
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

		var result openaiResponse
		if err := json.Unmarshal(respBody, &result); err != nil {
			return fmt.Errorf("parsing response: %w", err)
		}

		if len(result.Choices) == 0 {
			return fmt.Errorf("no choices in response")
		}
		if result.Choices[0].Message.Content == "" {
			return fmt.Errorf("empty text content in API response")
		}

		resp = ReviewResponse{
			Content:    result.Choices[0].Message.Content,
			TokensUsed: result.Usage.TotalTokens,
			Provider:   o.Name(),
			Model:      o.model,
		}
		return nil
	})

	return resp, err
}

type openaiRequest struct {
	Model               string          `json:"model"`
	Messages            []openaiMessage `json:"messages"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
}

// usesMaxCompletionTokens returns true for models that require
// max_completion_tokens instead of max_tokens.
func usesMaxCompletionTokens(model string) bool {
	return strings.HasPrefix(model, "gpt-5") ||
		strings.HasPrefix(model, "o1") ||
		strings.HasPrefix(model, "o3") ||
		strings.HasPrefix(model, "o4")
}

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiResponse struct {
	Choices []openaiChoice `json:"choices"`
	Usage   openaiUsage    `json:"usage"`
}

type openaiChoice struct {
	Message openaiMessage `json:"message"`
}

type openaiUsage struct {
	TotalTokens int `json:"total_tokens"`
}
