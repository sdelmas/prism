package review

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dshills/prism/internal/config"
	"github.com/dshills/prism/internal/diffutil"
	"github.com/dshills/prism/internal/providers"
	"github.com/dshills/prism/internal/ratelimit"
)

const (
	// ChunkThreshold is the byte size above which we switch to chunked review.
	ChunkThreshold = 100000 // 100KB
)

// Chunk represents a portion of a diff to be reviewed independently.
type Chunk struct {
	Index int
	Diff  string
	Files []string
}

// SplitIntoChunks splits a diff into per-file chunks.
// Each chunk contains the diff sections for one or more files,
// staying under maxBytes per chunk.
func SplitIntoChunks(diff string, maxBytes int) []Chunk {
	sections := diffutil.SplitSections(diff)
	if len(sections) == 0 {
		return nil
	}

	if maxBytes <= 0 {
		maxBytes = ChunkThreshold
	}

	var chunks []Chunk
	var currentDiff strings.Builder
	var currentFiles []string
	idx := 0

	for _, sec := range sections {
		path := diffutil.PathFromSection(sec)

		// If adding this section would exceed maxBytes, flush the current chunk
		if currentDiff.Len() > 0 && currentDiff.Len()+len(sec) > maxBytes {
			chunks = append(chunks, Chunk{
				Index: idx,
				Diff:  currentDiff.String(),
				Files: currentFiles,
			})
			idx++
			currentDiff.Reset()
			currentFiles = nil
		}

		currentDiff.WriteString(sec)
		if path != "" {
			currentFiles = append(currentFiles, path)
		}
	}

	// Flush remaining
	if currentDiff.Len() > 0 {
		chunks = append(chunks, Chunk{
			Index: idx,
			Diff:  currentDiff.String(),
			Files: currentFiles,
		})
	}

	return chunks
}

// NeedsChunking returns true if the diff is large enough to benefit from chunked review.
func NeedsChunking(diff string) bool {
	return len(diff) > ChunkThreshold
}

// PromptBuilder constructs system and user prompts for a chunk.
type PromptBuilder func(chunkDiff string, files []string, cfg config.Config, rules *Rules) (systemPrompt, userPrompt string)

// ChunkOptions controls how chunked review is performed.
type ChunkOptions struct {
	Builder PromptBuilder
}

// defaultPromptBuilder uses the standard diff-review prompts.
func defaultPromptBuilder(chunkDiff string, files []string, cfg config.Config, rules *Rules) (string, string) {
	return SystemPrompt(), BuildUserPromptWithRules(chunkDiff, files, cfg.MaxFindings, cfg.FailOn, rules)
}

// RunChunked reviews diff chunks in parallel and merges findings.
func RunChunked(ctx context.Context, chunks []Chunk, provider providers.Reviewer, cfg config.Config) ([]Finding, int64, error) {
	return RunChunkedWithRules(ctx, chunks, provider, cfg, nil)
}

// RunChunkedWithRules reviews diff chunks in parallel with optional rules.
func RunChunkedWithRules(ctx context.Context, chunks []Chunk, provider providers.Reviewer, cfg config.Config, rules *Rules) ([]Finding, int64, error) {
	return RunChunkedWithOptions(ctx, chunks, provider, cfg, rules, ChunkOptions{})
}

// RunChunkedWithOptions reviews diff chunks in parallel with custom prompt construction.
func RunChunkedWithOptions(ctx context.Context, chunks []Chunk, provider providers.Reviewer, cfg config.Config, rules *Rules, opts ChunkOptions) ([]Finding, int64, error) {
	builder := opts.Builder
	if builder == nil {
		builder = defaultPromptBuilder
	}

	type result struct {
		index    int
		findings []Finding
		err      error
	}

	// Compute effective concurrency and rate limit from config + provider defaults.
	concurrency := cfg.MaxConcurrency
	if concurrency <= 0 {
		concurrency = providers.DefaultMaxConcurrency(provider.Name())
	}
	rpm := cfg.RateLimitRPM
	if rpm <= 0 {
		rpm = providers.DefaultRPM(provider.Name())
	}
	limiter := ratelimit.New(rpm)

	results := make([]result, len(chunks))
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	var totalLLMMs int64
	var mu sync.Mutex

	for i, chunk := range chunks {
		wg.Add(1)
		go func(i int, chunk Chunk) {
			defer wg.Done()
			sem <- struct{}{}        // acquire
			defer func() { <-sem }() // release

			if err := limiter.Wait(ctx); err != nil {
				results[i] = result{index: i, err: fmt.Errorf("chunk %d: rate limiter: %w", i, err)}
				return
			}

			sysPr, userPr := builder(chunk.Diff, chunk.Files, cfg, rules)
			req := providers.ReviewRequest{
				SystemPrompt: sysPr,
				UserPrompt:   userPr,
				MaxTokens:    maxTokensFor(cfg),
			}

			llmStart := time.Now()
			resp, err := provider.Review(ctx, req)
			elapsed := time.Since(llmStart).Milliseconds()

			mu.Lock()
			totalLLMMs += elapsed
			mu.Unlock()

			if err != nil {
				results[i] = result{index: i, err: fmt.Errorf("chunk %d: %w", i, err)}
				return
			}

			findings, err := parseFindings(resp.Content)
			if err != nil {
				// Try repair
				repairPrompt := fmt.Sprintf(
					"Your previous response was not valid JSON. The error was: %s\n\nPlease fix and respond with ONLY a valid JSON array of findings.\n\nPrevious response:\n%s",
					err.Error(), resp.Content,
				)
				resp2, err2 := provider.Review(ctx, providers.ReviewRequest{
					SystemPrompt: sysPr,
					UserPrompt:   repairPrompt,
					MaxTokens:    maxTokensFor(cfg),
				})
				if err2 != nil {
					results[i] = result{index: i, err: fmt.Errorf("chunk %d repair: %w", i, err2)}
					return
				}
				findings, err = parseFindings(resp2.Content)
				if err != nil {
					results[i] = result{index: i, err: fmt.Errorf("chunk %d validation after repair: %w", i, err)}
					return
				}
				resp = resp2
			}

			findings = stampProvenance(findings, resp.Provider, resp.Model)
			results[i] = result{index: i, findings: findings}
		}(i, chunk)
	}

	wg.Wait()

	// Merge findings in stable order (by chunk index)
	var allFindings []Finding
	for _, r := range results {
		if r.err != nil {
			return nil, totalLLMMs, r.err
		}
		allFindings = append(allFindings, r.findings...)
	}

	// Deduplicate by finding ID
	allFindings = DeduplicateFindings(allFindings)

	// Sort by severity (high first), then by file path, then by line.
	SortFindings(allFindings)

	return allFindings, totalLLMMs, nil
}

// DeduplicateFindings removes duplicate findings by ID.
func DeduplicateFindings(findings []Finding) []Finding {
	seen := make(map[string]bool)
	var result []Finding
	for _, f := range findings {
		if !seen[f.ID] {
			seen[f.ID] = true
			result = append(result, f)
		}
	}
	return result
}

// SortFindings sorts findings by severity (high first), then path, then line.
func SortFindings(findings []Finding) {
	sort.Slice(findings, func(i, j int) bool {
		ri := SeverityRank(findings[i].Severity)
		rj := SeverityRank(findings[j].Severity)
		if ri != rj {
			return ri > rj
		}
		pi := findingPath(findings[i])
		pj := findingPath(findings[j])
		if pi != pj {
			return pi < pj
		}
		li := findingStartLine(findings[i])
		lj := findingStartLine(findings[j])
		return li < lj
	})
}

func findingPath(f Finding) string {
	if len(f.Locations) > 0 {
		return f.Locations[0].Path
	}
	return ""
}

func findingStartLine(f Finding) int {
	if len(f.Locations) > 0 {
		return f.Locations[0].Lines.Start
	}
	return 0
}
