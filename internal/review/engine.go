package review

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/dshills/prism/internal/cache"
	"github.com/dshills/prism/internal/config"
	"github.com/dshills/prism/internal/diffutil"
	"github.com/dshills/prism/internal/gitctx"
	"github.com/dshills/prism/internal/providers"
	"github.com/dshills/prism/internal/redact"
)

// rawFinding is the JSON structure returned by the LLM and also used for
// cache storage. Provider/Model are set only on the cache-storage path so
// cached findings round-trip their provenance; LLM responses won't populate
// them (the model doesn't know its own identity) — the engine stamps those
// from the provider after parsing.
type rawFinding struct {
	Severity   string   `json:"severity"`
	Category   string   `json:"category"`
	Title      string   `json:"title"`
	Message    string   `json:"message"`
	Suggestion string   `json:"suggestion"`
	Confidence float64  `json:"confidence"`
	Path       string   `json:"path"`
	StartLine  int      `json:"startLine"`
	EndLine    int      `json:"endLine"`
	Tags       []string `json:"tags"`
	Provider   string   `json:"provider,omitempty"`
	Model      string   `json:"model,omitempty"`
}

// reviewOpts controls differences between Run() and RunCodebase() pipelines.
type reviewOpts struct {
	builder     PromptBuilder // nil = default diff prompts
	alwaysChunk bool          // true = skip NeedsChunking() check
}

// Run executes a review using the given diff result and configuration.
func Run(ctx context.Context, diff gitctx.DiffResult, cfg config.Config) (*Report, error) {
	return reviewPipeline(ctx, diff, cfg, reviewOpts{})
}

// reviewPipeline is the shared review flow: redact → cache → rules → LLM → cache write → overrides → limit → report.
func reviewPipeline(ctx context.Context, diff gitctx.DiffResult, cfg config.Config, opts reviewOpts) (*Report, error) {
	startTime := time.Now()

	// Redact secrets from diff before sending to provider
	redactedDiff := diff.Diff
	if cfg.Privacy.RedactSecrets {
		redactedDiff = redact.Secrets(redactedDiff)
	}

	if strings.TrimSpace(redactedDiff) == "" {
		return emptyReport(diff, startTime), nil
	}

	// Initialize cache
	reviewCache, err := cache.New(cfg.Cache.Enabled, cfg.Cache.Dir, cfg.Cache.TTLSeconds)
	if err != nil {
		// Cache failure is non-fatal, just disable it
		reviewCache, _ = cache.New(false, "", 0)
	}

	cacheKey := cache.BuildCacheKey(cfg.Provider, cfg.Model, redactedDiff)

	// Check cache
	var findings []Finding
	var llmMs int64
	if cached, ok := reviewCache.Get(cacheKey); ok {
		findings, err = parseFindings(cached)
		if err != nil {
			// Cache entry is corrupt, fall through to LLM
			findings = nil
		} else {
			// Legacy cache entries may lack provenance; stamp from the cache
			// key's (provider, model) since the key itself fixes them.
			findings = stampProvenance(findings, cfg.Provider, cfg.Model)
		}
	}

	// Load rules
	rules, err := LoadRules(cfg.RulesFile)
	if err != nil {
		return nil, fmt.Errorf("loading rules: %w", err)
	}

	if findings == nil {
		provider, err := providers.New(cfg.Provider, cfg.Model)
		if err != nil {
			return nil, fmt.Errorf("creating provider: %w", err)
		}

		// Use chunked review for large diffs or when always requested (codebase mode)
		if opts.alwaysChunk || NeedsChunking(redactedDiff) {
			// Chunk on ChunkMaxBytes, not MaxDiffBytes. MaxDiffBytes has already
			// truncated redactedDiff above, so passing it here made maxBytes >=
			// len(diff) by construction: every diff became one chunk and the
			// splitting below never ran.
			chunks := SplitIntoChunks(redactedDiff, chunkBytesFor(cfg))
			findings, llmMs, err = RunChunkedWithOptions(ctx, chunks, provider, cfg, rules, ChunkOptions{
				Builder: opts.builder,
			})
			if err != nil {
				return nil, fmt.Errorf("chunked review: %w", err)
			}
		} else {
			builder := opts.builder
			if builder == nil {
				builder = defaultPromptBuilder
			}
			sysPr, userPr := builder(redactedDiff, diff.Files, cfg, rules)

			llmStart := time.Now()
			req := providers.ReviewRequest{
				SystemPrompt: sysPr,
				UserPrompt:   userPr,
				MaxTokens:    maxTokensFor(cfg),
			}

			resp, err := provider.Review(ctx, req)
			if err != nil {
				return nil, fmt.Errorf("provider review: %w", err)
			}
			llmMs = time.Since(llmStart).Milliseconds()

			findings, err = parseFindings(resp.Content)
			if err != nil {
				// Attempt one repair pass
				repairPrompt := fmt.Sprintf(
					"Your previous response was not valid JSON. The error was: %s\n\nPlease fix it and respond with ONLY a valid JSON array of findings.\n\nYour previous response was:\n%s",
					err.Error(), resp.Content,
				)
				repairReq := providers.ReviewRequest{
					SystemPrompt: sysPr,
					UserPrompt:   repairPrompt,
					MaxTokens:    maxTokensFor(cfg),
				}
				resp2, err2 := provider.Review(ctx, repairReq)
				if err2 != nil {
					return nil, fmt.Errorf("repair pass failed: %w (original error: %w)", err2, err)
				}
				findings, err = parseFindings(resp2.Content)
				if err != nil {
					return nil, fmt.Errorf("response validation failed after repair: %w", err)
				}
				resp = resp2
			}
			findings = stampProvenance(findings, resp.Provider, resp.Model)
			SortFindings(findings)
		}

		// Store in cache as rawFinding format so parseFindings can read it back
		if rawJSON, jerr := json.Marshal(findingsToRaw(findings)); jerr == nil {
			_ = reviewCache.Put(cacheKey, string(rawJSON))
		}
	}

	// Apply rules severity overrides
	findings = ApplySeverityOverrides(findings, rules)

	// Limit findings
	if cfg.MaxFindings > 0 && len(findings) > cfg.MaxFindings {
		findings = findings[:cfg.MaxFindings]
	}

	return BuildReport(diff, findings, llmMs, time.Since(startTime).Milliseconds()), nil
}

// thinkBlockRe matches <think>...</think> reasoning blocks emitted by
// reasoning models (e.g. MiniMax M-series). (?is) = case-insensitive, dot
// matches newlines.
var thinkBlockRe = regexp.MustCompile(`(?is)<think>.*?</think>`)

// stripReasoning removes reasoning blocks and any prose surrounding the JSON
// payload that reasoning models emit before/after the findings array.
func stripReasoning(content string) string {
	// Remove closed <think>...</think> blocks.
	content = thinkBlockRe.ReplaceAllString(content, "")
	// Remove a dangling, unclosed <think> (response truncated inside it).
	if i := strings.Index(strings.ToLower(content), "<think>"); i != -1 {
		content = content[:i]
	}
	content = strings.TrimSpace(content)

	// After stripping, if the content still isn't a bare JSON array/object,
	// carve out the outermost array from the first '[' to the last ']'.
	if !strings.HasPrefix(content, "[") && !strings.HasPrefix(content, "{") && !strings.HasPrefix(content, "```") {
		if start := strings.Index(content, "["); start != -1 {
			if end := strings.LastIndex(content, "]"); end > start {
				content = content[start : end+1]
			}
		}
	}
	return content
}

func parseFindings(content string) ([]Finding, error) {
	content = strings.TrimSpace(content)

	// Reasoning models prefix output with <think>...</think>; strip it (and
	// any surrounding prose) before attempting to parse JSON.
	content = stripReasoning(content)

	// Strip markdown code fences if present
	if strings.HasPrefix(content, "```") {
		lines := strings.Split(content, "\n")
		if len(lines) >= 2 {
			// Remove first line (```json) and last line (```)
			start := 1
			end := len(lines)
			if strings.TrimSpace(lines[end-1]) == "```" {
				end = end - 1
			}
			if start < end {
				content = strings.Join(lines[start:end], "\n")
			} else {
				// Empty code fence (e.g., "```\n```") — treat as empty array
				content = "[]"
			}
		}
	}

	var raw []rawFinding
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON array: %w", err)
	}

	findings := make([]Finding, 0, len(raw))
	for _, r := range raw {
		f := Finding{
			Severity:   Severity(r.Severity),
			Category:   Category(r.Category),
			Title:      r.Title,
			Message:    r.Message,
			Suggestion: r.Suggestion,
			Confidence: r.Confidence,
			Tags:       r.Tags,
			Provider:   r.Provider,
			Model:      r.Model,
			Locations: []Location{
				{
					Path: r.Path,
					Lines: LineRange{
						Start: r.StartLine,
						End:   r.EndLine,
					},
				},
			},
		}
		f.ID = generateFindingID(f)
		findings = append(findings, f)
	}

	return findings, nil
}

// stampProvenance sets Provider and Model on every finding that doesn't
// already have them. Used after parsing fresh LLM responses; cached findings
// retain whatever the cache wrote.
func stampProvenance(findings []Finding, provider, model string) []Finding {
	for i := range findings {
		if findings[i].Provider == "" {
			findings[i].Provider = provider
		}
		if findings[i].Model == "" {
			findings[i].Model = model
		}
	}
	return findings
}

// findingsToRaw converts parsed Findings back to rawFinding format for cache storage.
func findingsToRaw(findings []Finding) []rawFinding {
	raw := make([]rawFinding, len(findings))
	for i, f := range findings {
		r := rawFinding{
			Severity:   string(f.Severity),
			Category:   string(f.Category),
			Title:      f.Title,
			Message:    f.Message,
			Suggestion: f.Suggestion,
			Confidence: f.Confidence,
			Tags:       f.Tags,
			Provider:   f.Provider,
			Model:      f.Model,
		}
		if len(f.Locations) > 0 {
			r.Path = f.Locations[0].Path
			r.StartLine = f.Locations[0].Lines.Start
			r.EndLine = f.Locations[0].Lines.End
		}
		raw[i] = r
	}
	return raw
}

func generateFindingID(f Finding) string {
	var path string
	if len(f.Locations) > 0 {
		path = f.Locations[0].Path
	}
	data := fmt.Sprintf("%s:%s:%d", path, f.Title, func() int {
		if len(f.Locations) > 0 {
			return f.Locations[0].Lines.Start
		}
		return 0
	}())
	h := sha256.Sum256([]byte(data))
	return fmt.Sprintf("%x", h[:8])
}

// GenerateRunID creates a unique run identifier.
func GenerateRunID() string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	return fmt.Sprintf("%x", h[:16])
}

// CodebaseConfig extends config with codebase-specific options.
type CodebaseConfig struct {
	config.Config
	MaxFindingsPerFile int
}

// RunCodebase executes a full-codebase review.
func RunCodebase(ctx context.Context, diff gitctx.DiffResult, cfg CodebaseConfig) (*Report, error) {
	startTime := time.Now()

	reviewCache, err := cache.New(cfg.Cache.Enabled, cfg.Cache.Dir, cfg.Cache.TTLSeconds)
	if err != nil {
		reviewCache, _ = cache.New(false, "", 0)
	}

	rules, err := LoadRules(cfg.RulesFile)
	if err != nil {
		return nil, fmt.Errorf("loading rules: %w", err)
	}

	return runCodebaseWithFileCache(ctx, diff, cfg, reviewCache, rules, startTime)
}

// runCodebaseWithFileCache implements per-file cache invalidation for codebase mode.
// Each file section is checked individually against the cache; only cache misses
// are sent to the LLM. Fresh findings are stored per-file so subsequent runs on
// unchanged files return cached results without any LLM call.
func runCodebaseWithFileCache(
	ctx context.Context,
	diff gitctx.DiffResult,
	cfg CodebaseConfig,
	reviewCache *cache.Cache,
	rules *Rules,
	startTime time.Time,
) (*Report, error) {
	var (
		cachedFindings   []Finding
		uncachedSections []string
		freshFindings    []Finding
		llmMs            int64
	)

	// Step 1: Apply redaction once; the same redacted text is used for both
	// cache key derivation and LLM input (FR-5).
	redactedDiff := diff.Diff
	if cfg.Privacy.RedactSecrets {
		redactedDiff = redact.Secrets(diff.Diff)
	}

	// Step 2: Nothing to review.
	if strings.TrimSpace(redactedDiff) == "" {
		return emptyReport(diff, startTime), nil
	}

	// Step 3: Split into per-file sections.
	sections := diffutil.SplitSections(redactedDiff)

	// Step 4: Check each section against the per-file cache.
	for _, section := range sections {
		key := cache.BuildCacheKey(cfg.Provider, cfg.Model, section)
		if cached, ok := reviewCache.Get(key); ok {
			parsed, err := parseFindings(cached)
			if err == nil {
				// Valid cache hit — collect findings and skip LLM for this file.
				cachedFindings = append(cachedFindings, parsed...)
				continue
			}
			// Corrupt entry: fall through and treat as a miss (FR-7).
		}
		uncachedSections = append(uncachedSections, section)
	}

	// Step 5: All files cached — skip LLM entirely (AC-1).
	if len(uncachedSections) > 0 {
		// Step 6: Build a filtered diff containing only cache-miss sections.
		filteredDiff := strings.Join(uncachedSections, "")

		// Step 7: Run the chunked review on uncached sections only.
		provider, err := providers.New(cfg.Provider, cfg.Model)
		if err != nil {
			return nil, fmt.Errorf("creating provider: %w", err)
		}

		maxPerFile := cfg.MaxFindingsPerFile
		codebaseBuilder := func(chunkDiff string, files []string, c config.Config, r *Rules) (string, string) {
			return CodebaseSystemPrompt(), BuildCodebaseUserPrompt(chunkDiff, files, c.MaxFindings, maxPerFile, c.FailOn, r)
		}

		chunks := SplitIntoChunks(filteredDiff, cfg.MaxDiffBytes)
		var err2 error
		freshFindings, llmMs, err2 = RunChunkedWithOptions(ctx, chunks, provider, cfg.Config, rules, ChunkOptions{Builder: codebaseBuilder})
		if err2 != nil {
			return nil, fmt.Errorf("chunked review: %w", err2)
		}

		// Step 8: Store fresh findings per file for future cache hits (FR-4).
		storeFindingsPerFile(reviewCache, uncachedSections, freshFindings, cfg.Provider, cfg.Model)
	}

	// Step 9: Merge cached and fresh findings.
	allFindings := append(cachedFindings, freshFindings...)

	// Step 10: Apply rules severity overrides.
	allFindings = ApplySeverityOverrides(allFindings, rules)

	// Step 11: Deduplicate (safety net for any cross-chunk duplicates).
	allFindings = DeduplicateFindings(allFindings)

	// Step 12: Sort high → medium → low, then by path, then by line.
	SortFindings(allFindings)

	// Step 13: Enforce MaxFindings on the merged set (FR-9).
	if cfg.MaxFindings > 0 && len(allFindings) > cfg.MaxFindings {
		allFindings = allFindings[:cfg.MaxFindings]
	}

	return BuildReport(diff, allFindings, llmMs, time.Since(startTime).Milliseconds()), nil
}

// storeFindingsPerFile stores fresh LLM findings into the cache at per-file
// granularity. Each section gets its own cache entry keyed on
// hash(provider, model, sectionText) so only the changed file is a miss on
// the next run.
//
// If ANY finding in the batch has no primary path the entire batch is left
// uncached — we cannot attribute unattributable findings to a specific file,
// and silently dropping them would cause them to disappear from subsequent
// reports (FR-4, plan Design Decisions).
//
// All write errors are silently ignored (FR-7).
func storeFindingsPerFile(reviewCache *cache.Cache, sections []string, findings []Finding, provider, model string) {
	// Guard: if any finding lacks a primary path, skip the entire batch.
	for _, f := range findings {
		if len(f.Locations) == 0 || f.Locations[0].Path == "" {
			return
		}
	}

	// Group findings by primary file path.
	byPath := make(map[string][]Finding)
	for _, f := range findings {
		path := f.Locations[0].Path
		byPath[path] = append(byPath[path], f)
	}

	// Write one cache entry per section. Sections with no findings are stored
	// as "[]" so they produce cache hits on the next run (FR-4, AC-5).
	for _, section := range sections {
		path := diffutil.PathFromSection(section)
		if path == "" {
			continue
		}
		key := cache.BuildCacheKey(provider, model, section)
		raw := findingsToRaw(byPath[path]) // nil slice marshals as JSON []
		data, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		_ = reviewCache.Put(key, string(data))
	}
}

// BuildReport constructs a Report from diff metadata, findings, and timing info.
// Report.Provenance is derived from the per-finding Provider/Model pairs.
// Callers are expected to pass pre-sorted findings (reviewPipeline and
// RunChunkedWithOptions both sort before calling BuildReport).
func BuildReport(diff gitctx.DiffResult, findings []Finding, llmMs, totalMs int64) *Report {
	if findings == nil {
		findings = []Finding{}
	}
	return &Report{
		Tool:    "prism",
		Version: "1.0",
		RunID:   GenerateRunID(),
		Repo: RepoInfo{
			Root:   diff.Repo.Root,
			Head:   diff.Repo.Head,
			Branch: diff.Repo.Branch,
		},
		Inputs: InputInfo{
			Mode:  diff.Mode,
			Range: diff.Range,
		},
		Summary:  ComputeSummary(findings),
		Findings: findings,
		Timing: Timing{
			LLMMs:   llmMs,
			TotalMs: totalMs,
		},
		Provenance: CollectProvenance(findings),
	}
}

// CollectProvenance returns the deduplicated, stably ordered list of
// (provider, model) pairs across all findings. Order follows first appearance.
func CollectProvenance(findings []Finding) []Provenance {
	seen := make(map[string]bool)
	var out []Provenance
	for _, f := range findings {
		if f.Provider == "" && f.Model == "" {
			continue
		}
		key := f.Provider + "\x00" + f.Model
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Provenance{
			AIGenerated: true,
			Provider:    f.Provider,
			Model:       f.Model,
		})
	}
	return out
}

func emptyReport(diff gitctx.DiffResult, startTime time.Time) *Report {
	return BuildReport(diff, []Finding{}, 0, time.Since(startTime).Milliseconds())
}

// maxTokensFor returns the per-request output budget. It falls back to the
// historical hardcoded 8192 when nothing is configured, so behaviour is
// unchanged for anyone who never sets it. Reasoning models need considerably
// more: they spend the budget thinking before they answer, and a budget that
// runs out mid-thought returns an empty completion, not a short one.
func maxTokensFor(cfg config.Config) int {
	if cfg.MaxTokens > 0 {
		return cfg.MaxTokens
	}
	return 8192
}

// chunkBytesFor returns the per-chunk byte bound, defaulting when unset.
func chunkBytesFor(cfg config.Config) int {
	if cfg.ChunkMaxBytes > 0 {
		return cfg.ChunkMaxBytes
	}
	return 20000
}
