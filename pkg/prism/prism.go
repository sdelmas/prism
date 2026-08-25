package prism

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dshills/prism/internal/config"
	"github.com/dshills/prism/internal/gitctx"
	"github.com/dshills/prism/internal/output"
	"github.com/dshills/prism/internal/providers"
	"github.com/dshills/prism/internal/review"
)

const Version = "0.6.1"

type Report = review.Report
type Finding = review.Finding
type Severity = review.Severity
type Category = review.Category
type Location = review.Location
type LineRange = review.LineRange
type Provenance = review.Provenance
type RepoInfo = review.RepoInfo
type InputInfo = review.InputInfo
type Summary = review.Summary
type Timing = review.Timing

type ReviewMode string

const (
	ModeRawDiff  ReviewMode = "raw_diff"
	ModeSnippet  ReviewMode = "snippet"
	ModeUnstaged ReviewMode = "unstaged"
	ModeStaged   ReviewMode = "staged"
	ModeCommit   ReviewMode = "commit"
	ModeRange    ReviewMode = "range"
	ModeCodebase ReviewMode = "codebase"
)

type ReviewOptions struct {
	Mode               ReviewMode
	RepoPath           string
	Diff               string
	Files              []string
	Snippet            string
	SnippetPath        string
	SnippetLang        string
	SnippetBase        string
	Revision           string
	Parent             string
	MergeBase          bool
	PerCommit          bool
	Provider           string
	Model              string
	Compare            []string
	Format             string
	FailOn             string
	MaxFindings        int
	MaxFindingsPerFile int
	ContextLines       int
	Include            []string
	Exclude            []string
	MaxDiffBytes       int
	RulesFile          string
	CacheEnabled       *bool
	CacheDir           string
	CacheTTLSeconds    int
	RedactSecrets      *bool
	RedactPaths        []string
}

type ReviewResult struct {
	Report *Report
}

type ModelInfo struct {
	Provider string   `json:"provider"`
	Models   []string `json:"models"`
}

var chdirMu sync.Mutex

func DefaultReviewOptions() ReviewOptions {
	cfg := config.Default()
	return ReviewOptions{
		Mode:            ModeRawDiff,
		Provider:        cfg.Provider,
		Model:           cfg.Model,
		Format:          cfg.Format,
		FailOn:          cfg.FailOn,
		MaxFindings:     cfg.MaxFindings,
		ContextLines:    cfg.ContextLines,
		Include:         append([]string(nil), cfg.Include...),
		Exclude:         append([]string(nil), cfg.Exclude...),
		MaxDiffBytes:    cfg.MaxDiffBytes,
		CacheDir:        cfg.Cache.Dir,
		CacheTTLSeconds: cfg.Cache.TTLSeconds,
		RedactPaths:     append([]string(nil), cfg.Privacy.RedactPaths...),
		MergeBase:       true,
	}
}

func Review(ctx context.Context, opts ReviewOptions) (*ReviewResult, error) {
	cfg := configFromOptions(opts)
	diff, err := diffFromOptions(ctx, opts, cfg)
	if err != nil {
		return nil, err
	}
	report, err := runReview(ctx, diff, cfg, opts)
	if err != nil {
		return nil, err
	}
	return &ReviewResult{Report: report}, nil
}

func RenderReport(report *Report, format string) ([]byte, error) {
	if format == "" {
		format = "json"
	}
	writer, err := output.GetWriter(format)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writer.Write(&buf, report); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func FilterReportBySeverity(report *Report, threshold string) *Report {
	if report == nil || threshold == "" || threshold == "none" {
		return report
	}
	filtered := cloneReport(report)
	findings := filtered.Findings[:0]
	for _, finding := range filtered.Findings {
		if review.MeetsThreshold(finding.Severity, threshold) {
			findings = append(findings, finding)
		}
	}
	filtered.Findings = findings
	filtered.Summary = review.ComputeSummary(findings)
	filtered.Provenance = review.CollectProvenance(findings)
	return filtered
}

func FailOnMet(report *Report, threshold string) bool {
	if report == nil || threshold == "" || threshold == "none" {
		return false
	}
	for _, finding := range report.Findings {
		if review.MeetsThreshold(finding.Severity, threshold) {
			return true
		}
	}
	return false
}

func KnownModels() []ModelInfo {
	return []ModelInfo{
		{Provider: "anthropic", Models: []string{"claude-sonnet-4-6", "claude-opus-4-6", "claude-haiku-4-5"}},
		{Provider: "openai", Models: []string{"gpt-5.3-codex", "gpt-5.3-codex-spark", "gpt-5.2-codex", "gpt-5.2", "gpt-4.1-mini", "o3-mini"}},
		{Provider: "gemini", Models: []string{"gemini-3-flash-preview", "gemini-3-pro-preview", "gemini-2.5-flash", "gemini-2.5-pro"}},
		{Provider: "ollama", Models: []string{"llama3.3", "llama3.2", "llama3.1", "codellama", "qwen2.5-coder", "deepseek-coder-v2"}},
	}
}

func IsSupportedProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic", "openai", "gemini", "google", "ollama", "lmstudio":
		return true
	default:
		return false
	}
}

func ProviderForModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(model, "claude"):
		return "anthropic"
	case strings.HasPrefix(model, "gpt"), strings.HasPrefix(model, "o3"):
		return "openai"
	case strings.HasPrefix(model, "gemini"):
		return "gemini"
	case strings.Contains(model, "llama"), strings.Contains(model, "qwen"), strings.Contains(model, "codellama"), strings.Contains(model, "deepseek"):
		return "ollama"
	default:
		return ""
	}
}

func configFromOptions(opts ReviewOptions) config.Config {
	cfg := config.Default()
	if opts.Provider != "" {
		cfg.Provider = opts.Provider
	}
	if opts.Model != "" {
		cfg.Model = opts.Model
	}
	if len(opts.Compare) > 0 {
		cfg.Compare = append([]string(nil), opts.Compare...)
	}
	if opts.Format != "" {
		cfg.Format = opts.Format
	}
	if opts.FailOn != "" {
		cfg.FailOn = opts.FailOn
	}
	if opts.MaxFindings > 0 {
		cfg.MaxFindings = opts.MaxFindings
	}
	if opts.ContextLines > 0 {
		cfg.ContextLines = opts.ContextLines
	}
	if len(opts.Include) > 0 {
		cfg.Include = append([]string(nil), opts.Include...)
	}
	if len(opts.Exclude) > 0 {
		cfg.Exclude = append([]string(nil), opts.Exclude...)
	}
	if opts.MaxDiffBytes > 0 {
		cfg.MaxDiffBytes = opts.MaxDiffBytes
	}
	if opts.RulesFile != "" {
		cfg.RulesFile = opts.RulesFile
	}
	if opts.CacheEnabled != nil {
		cfg.Cache.Enabled = *opts.CacheEnabled
	}
	if opts.CacheDir != "" {
		cfg.Cache.Dir = opts.CacheDir
	}
	if opts.CacheTTLSeconds > 0 {
		cfg.Cache.TTLSeconds = opts.CacheTTLSeconds
	}
	if opts.RedactSecrets != nil {
		cfg.Privacy.RedactSecrets = *opts.RedactSecrets
	}
	if len(opts.RedactPaths) > 0 {
		cfg.Privacy.RedactPaths = append([]string(nil), opts.RedactPaths...)
	}
	return cfg
}

func diffFromOptions(ctx context.Context, opts ReviewOptions, cfg config.Config) (gitctx.DiffResult, error) {
	diffOpts := gitctx.DiffOptions{
		ContextLines: cfg.ContextLines,
		MaxDiffBytes: cfg.MaxDiffBytes,
		Include:      cfg.Include,
		Exclude:      cfg.Exclude,
	}
	mode := opts.Mode
	if mode == "" {
		mode = ModeRawDiff
	}
	switch mode {
	case ModeRawDiff:
		return gitctx.DiffResult{Diff: opts.Diff, Files: append([]string(nil), opts.Files...), Mode: string(ModeRawDiff)}, nil
	case ModeSnippet:
		path := opts.SnippetPath
		if path == "" {
			path = "snippet"
		}
		return gitctx.Snippet(opts.Snippet, path, opts.SnippetLang, opts.SnippetBase)
	case ModeUnstaged:
		return withRepoPath(opts.RepoPath, func() (gitctx.DiffResult, error) { return gitctx.Unstaged(ctx, diffOpts) })
	case ModeStaged:
		return withRepoPath(opts.RepoPath, func() (gitctx.DiffResult, error) { return gitctx.Staged(ctx, diffOpts) })
	case ModeCommit:
		if opts.Revision == "" {
			return gitctx.DiffResult{}, fmt.Errorf("revision is required for commit mode")
		}
		return withRepoPath(opts.RepoPath, func() (gitctx.DiffResult, error) { return gitctx.Commit(ctx, opts.Revision, opts.Parent, diffOpts) })
	case ModeRange:
		if opts.Revision == "" {
			return gitctx.DiffResult{}, fmt.Errorf("revision is required for range mode")
		}
		return withRepoPath(opts.RepoPath, func() (gitctx.DiffResult, error) { return gitctx.Range(ctx, opts.Revision, opts.MergeBase, diffOpts) })
	case ModeCodebase:
		return withRepoPath(opts.RepoPath, func() (gitctx.DiffResult, error) { return gitctx.Codebase(ctx, diffOpts) })
	default:
		return gitctx.DiffResult{}, fmt.Errorf("unsupported review mode: %s", mode)
	}
}

func runReview(ctx context.Context, diff gitctx.DiffResult, cfg config.Config, opts ReviewOptions) (*Report, error) {
	if opts.Mode == ModeRange && opts.PerCommit {
		return runPerCommit(ctx, opts, cfg)
	}
	compareModels := cfg.Compare
	if len(compareModels) >= 2 && strings.TrimSpace(diff.Diff) != "" {
		return runCompare(ctx, diff, cfg, compareModels, opts.MaxFindingsPerFile)
	}
	if opts.Mode == ModeCodebase {
		return review.RunCodebase(ctx, diff, review.CodebaseConfig{
			Config:             cfg,
			MaxFindingsPerFile: opts.MaxFindingsPerFile,
		})
	}
	return review.Run(ctx, diff, cfg)
}

func runCompare(ctx context.Context, diff gitctx.DiffResult, cfg config.Config, models []string, maxFindingsPerFile int) (*Report, error) {
	rules, err := review.LoadRules(cfg.RulesFile)
	if err != nil {
		return nil, fmt.Errorf("loading rules: %w", err)
	}
	cr, err := review.RunCompare(ctx, diff.Diff, diff.Files, models, cfg, rules)
	if err != nil {
		return nil, err
	}
	findings := cr.All
	if cfg.MaxFindings > 0 && len(findings) > cfg.MaxFindings {
		findings = findings[:cfg.MaxFindings]
	}
	_ = maxFindingsPerFile
	report := review.BuildReport(diff, findings, cr.LLMMs, 0)
	report.Provenance = compareProvenance(models)
	return report, nil
}

func runPerCommit(ctx context.Context, opts ReviewOptions, cfg config.Config) (*Report, error) {
	if opts.Revision == "" {
		return nil, fmt.Errorf("revision is required for range mode")
	}
	var (
		allFindings []Finding
		totalLLMMs  int64
		meta        gitctx.RepoMeta
		start       = time.Now()
	)
	err := withRepoPathNoResult(opts.RepoPath, func() error {
		commits, err := gitctx.ListCommits(opts.Revision, opts.MergeBase)
		if err != nil {
			return err
		}
		diffOpts := gitctx.DiffOptions{
			ContextLines: cfg.ContextLines,
			MaxDiffBytes: cfg.MaxDiffBytes,
			Include:      cfg.Include,
			Exclude:      cfg.Exclude,
		}
		for _, commit := range commits {
			diff, err := gitctx.Commit(ctx, commit.SHA, "", diffOpts)
			if err != nil || strings.TrimSpace(diff.Diff) == "" {
				continue
			}
			report, err := review.Run(ctx, diff, cfg)
			if err != nil {
				return err
			}
			shortSHA := commit.SHA
			if len(shortSHA) > 7 {
				shortSHA = shortSHA[:7]
			}
			for i := range report.Findings {
				for j := range report.Findings[i].Locations {
					report.Findings[i].Locations[j].Commit = shortSHA
				}
			}
			allFindings = append(allFindings, report.Findings...)
			totalLLMMs += report.Timing.LLMMs
		}
		meta, _ = gitctx.GetRepoMeta(ctx)
		return nil
	})
	if err != nil {
		return nil, err
	}
	allFindings = review.DeduplicateFindings(allFindings)
	review.SortFindings(allFindings)
	if cfg.MaxFindings > 0 && len(allFindings) > cfg.MaxFindings {
		allFindings = allFindings[:cfg.MaxFindings]
	}
	return review.BuildReport(gitctx.DiffResult{Mode: "range", Range: opts.Revision, Repo: meta}, allFindings, totalLLMMs, time.Since(start).Milliseconds()), nil
}

func withRepoPath(repoPath string, fn func() (gitctx.DiffResult, error)) (gitctx.DiffResult, error) {
	if repoPath == "" {
		return fn()
	}
	chdirMu.Lock()
	defer chdirMu.Unlock()
	wd, err := os.Getwd()
	if err != nil {
		return gitctx.DiffResult{}, err
	}
	if err := os.Chdir(repoPath); err != nil {
		return gitctx.DiffResult{}, err
	}
	defer func() { _ = os.Chdir(wd) }()
	return fn()
}

func withRepoPathNoResult(repoPath string, fn func() error) error {
	_, err := withRepoPath(repoPath, func() (gitctx.DiffResult, error) {
		return gitctx.DiffResult{}, fn()
	})
	return err
}

func compareProvenance(models []string) []Provenance {
	out := make([]Provenance, 0, len(models))
	for _, spec := range models {
		parts := strings.SplitN(spec, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		out = append(out, Provenance{AIGenerated: true, Provider: parts[0], Model: parts[1]})
	}
	return out
}

func cloneReport(report *Report) *Report {
	clone := *report
	clone.Findings = make([]Finding, len(report.Findings))
	for i, f := range report.Findings {
		cf := f
		cf.Locations = append([]Location(nil), f.Locations...)
		cf.Tags = append([]string(nil), f.Tags...)
		cf.References = append([]string(nil), f.References...)
		clone.Findings[i] = cf
	}
	clone.Provenance = append([]Provenance(nil), report.Provenance...)
	return &clone
}

func IsAuthError(err error) bool {
	return providers.IsAuthError(err)
}
