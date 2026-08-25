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

const geminiAPIURL = "https://generativelanguage.googleapis.com/v1beta/models"

// Gemini implements the Reviewer interface for Google's Gemini API.
type Gemini struct {
	apiKey string
	model  string
	client *http.Client
}

// NewGemini creates a new Gemini provider.
func NewGemini(model string) (*Gemini, error) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		key = os.Getenv("GOOGLE_API_KEY")
	}
	if key == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY (or GOOGLE_API_KEY) environment variable is not set")
	}
	return &Gemini{
		apiKey: key,
		model:  model,
		client: &http.Client{Timeout: requestTimeout(120 * time.Second)},
	}, nil
}

func (g *Gemini) Name() string { return "gemini" }

func (g *Gemini) Review(ctx context.Context, req ReviewRequest) (ReviewResponse, error) {
	url := fmt.Sprintf("%s/%s:generateContent", geminiAPIURL, g.model)

	body := geminiRequest{
		SystemInstruction: &geminiContent{
			Parts: []geminiPart{{Text: req.SystemPrompt}},
		},
		Contents: []geminiContent{
			{
				Role:  "user",
				Parts: []geminiPart{{Text: req.UserPrompt}},
			},
		},
		GenerationConfig: &geminiGenConfig{
			MaxOutputTokens: req.MaxTokens,
		},
	}
	if body.GenerationConfig.MaxOutputTokens == 0 {
		body.GenerationConfig.MaxOutputTokens = 4096
	}
	if req.Temperature > 0 {
		body.GenerationConfig.Temperature = &req.Temperature
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return ReviewResponse{}, fmt.Errorf("marshaling request: %w", err)
	}

	var resp ReviewResponse
	err = retryWithBackoff(ctx, 3, func() error {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("creating request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("x-goog-api-key", g.apiKey)

		httpResp, err := g.client.Do(httpReq)
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

		var result geminiResponse
		if err := json.Unmarshal(respBody, &result); err != nil {
			return fmt.Errorf("parsing response: %w", err)
		}

		if len(result.Candidates) == 0 || len(result.Candidates[0].Content.Parts) == 0 {
			return fmt.Errorf("no content in response")
		}

		var content string
		for _, part := range result.Candidates[0].Content.Parts {
			content += part.Text
		}
		if content == "" {
			return fmt.Errorf("empty text content in API response")
		}

		resp = ReviewResponse{
			Content:    content,
			TokensUsed: result.UsageMetadata.TotalTokenCount,
			Provider:   g.Name(),
			Model:      g.model,
		}
		return nil
	})

	return resp, err
}

type geminiRequest struct {
	SystemInstruction *geminiContent   `json:"systemInstruction,omitempty"`
	Contents          []geminiContent  `json:"contents"`
	GenerationConfig  *geminiGenConfig `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenConfig struct {
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
}

type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata geminiUsage       `json:"usageMetadata"`
}

type geminiCandidate struct {
	Content geminiContent `json:"content"`
}

type geminiUsage struct {
	TotalTokenCount int `json:"totalTokenCount"`
}
