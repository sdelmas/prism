package gitctx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dshills/prism/internal/diffutil"
)

// DiffOptions controls how diffs are gathered.
type DiffOptions struct {
	ContextLines int
	MaxDiffBytes int
	Include      []string
	Exclude      []string
}

// DiffResult holds the collected diff and metadata.
type DiffResult struct {
	Diff  string
	Files []string
	Mode  string
	Range string
	Repo  RepoMeta
}

// RepoMeta contains git repository metadata.
type RepoMeta struct {
	Root   string
	Head   string
	Branch string
}

// repoMetaTimeout is the per-command deadline for GetRepoMeta git calls.
const repoMetaTimeout = 10 * time.Second

// GetRepoMeta collects repository metadata from git.
// The three rev-parse calls run concurrently to cut latency.
// Each subprocess is bounded by repoMetaTimeout to prevent indefinite hangs
// caused by locked indexes or filesystem issues.
func GetRepoMeta(ctx context.Context) (RepoMeta, error) {
	type gitResult struct {
		val string
		err error
	}

	// Bound all three subprocess calls with a single shared deadline.
	tCtx, tCancel := context.WithTimeout(ctx, repoMetaTimeout)
	defer tCancel()

	rootCh := make(chan gitResult, 1)
	headCh := make(chan gitResult, 1)
	branchCh := make(chan gitResult, 1)

	go func() { v, e := gitOutputCtx(tCtx, "rev-parse", "--show-toplevel"); rootCh <- gitResult{v, e} }()
	go func() { v, e := gitOutputCtx(tCtx, "rev-parse", "HEAD"); headCh <- gitResult{v, e} }()
	go func() {
		v, e := gitOutputCtx(tCtx, "rev-parse", "--abbrev-ref", "HEAD")
		branchCh <- gitResult{v, e}
	}()

	// Buffered channels (cap 1) guarantee goroutines exit once they send,
	// so early return on root error does not leak goroutines.
	rootRes := <-rootCh
	if rootRes.err != nil {
		return RepoMeta{}, fmt.Errorf("not a git repository: %w", rootRes.err)
	}

	headRes := <-headCh
	branchRes := <-branchCh

	head := ""
	if headRes.err == nil {
		head = strings.TrimSpace(headRes.val)
	}
	branch := ""
	if branchRes.err == nil {
		branch = strings.TrimSpace(branchRes.val)
	}
	return RepoMeta{
		Root:   strings.TrimSpace(rootRes.val),
		Head:   head,
		Branch: branch,
	}, nil
}

// Unstaged returns the diff of working tree vs index.
func Unstaged(ctx context.Context, opts DiffOptions) (DiffResult, error) {
	args := buildDiffArgs(opts)
	diff, err := gitOutputCtx(ctx, append([]string{"diff"}, args...)...)
	if err != nil {
		return DiffResult{}, fmt.Errorf("git diff: %w", err)
	}
	return buildResult(ctx, diff, "unstaged", "", opts)
}

// Staged returns the diff of index vs HEAD.
func Staged(ctx context.Context, opts DiffOptions) (DiffResult, error) {
	args := buildDiffArgs(opts)
	diff, err := gitOutputCtx(ctx, append([]string{"diff", "--cached"}, args...)...)
	if err != nil {
		return DiffResult{}, fmt.Errorf("git diff --cached: %w", err)
	}
	return buildResult(ctx, diff, "staged", "", opts)
}

// Commit returns the diff for a specific commit vs its parent.
func Commit(ctx context.Context, sha string, parent string, opts DiffOptions) (DiffResult, error) {
	args := buildDiffArgs(opts)
	if parent != "" {
		cmdArgs := append([]string{"diff", parent, sha}, args...)
		diff, err := gitOutputCtx(ctx, cmdArgs...)
		if err != nil {
			return DiffResult{}, fmt.Errorf("git diff %s %s: %w", parent, sha, err)
		}
		return buildResult(ctx, diff, "commit", sha, opts)
	}
	cmdArgs := append([]string{"diff", sha + "~1", sha}, args...)
	diff, err := gitOutputCtx(ctx, cmdArgs...)
	if err != nil {
		// Might be initial commit, try show
		showArgs := append([]string{"show", "--format=", sha, "--"}, args[1:]...) // skip -U flag reuse
		diff, err = gitOutputCtx(ctx, showArgs...)
		if err != nil {
			return DiffResult{}, fmt.Errorf("git show %s: %w", sha, err)
		}
	}
	return buildResult(ctx, diff, "commit", sha, opts)
}

// Range returns the combined diff for a revision range.
func Range(ctx context.Context, revRange string, mergeBase bool, opts DiffOptions) (DiffResult, error) {
	args := buildDiffArgs(opts)
	diffRange := revRange
	if mergeBase && strings.Contains(revRange, "..") && !strings.Contains(revRange, "...") {
		diffRange = strings.Replace(revRange, "..", "...", 1)
	}
	cmdArgs := append([]string{"diff", diffRange}, args...)
	diff, err := gitOutputCtx(ctx, cmdArgs...)
	if err != nil {
		return DiffResult{}, fmt.Errorf("git diff %s: %w", revRange, err)
	}
	return buildResult(ctx, diff, "range", revRange, opts)
}

// Snippet wraps raw content as a "diff" for review. If base is provided, computes a real diff.
func Snippet(content, path, lang, base string) (DiffResult, error) {
	var diff string
	if base != "" {
		tmpDir, err := os.MkdirTemp("", "prism-snippet-*")
		if err != nil {
			return DiffResult{}, fmt.Errorf("creating temp dir: %w", err)
		}
		defer func() { _ = os.RemoveAll(tmpDir) }()

		aDir := filepath.Join(tmpDir, "a")
		bDir := filepath.Join(tmpDir, "b")
		baseName := filepath.Base(path)

		if err := os.MkdirAll(aDir, 0o755); err != nil {
			return DiffResult{}, err
		}
		if err := os.MkdirAll(bDir, 0o755); err != nil {
			return DiffResult{}, err
		}
		if err := os.WriteFile(filepath.Join(aDir, baseName), []byte(base), 0o644); err != nil {
			return DiffResult{}, err
		}
		if err := os.WriteFile(filepath.Join(bDir, baseName), []byte(content), 0o644); err != nil {
			return DiffResult{}, err
		}

		// git diff --no-index returns exit code 1 when files differ (expected).
		// Only treat it as an error if the output is empty AND there's an error.
		diff, err = gitOutput("diff", "--no-index",
			filepath.Join(aDir, baseName),
			filepath.Join(bDir, baseName))
		if err != nil && diff == "" {
			return DiffResult{}, fmt.Errorf("git diff --no-index: %w", err)
		}
	} else {
		lines := strings.Split(content, "\n")
		var b strings.Builder
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n", path, path)
		fmt.Fprintf(&b, "new file mode 100644\n")
		fmt.Fprintf(&b, "--- /dev/null\n")
		fmt.Fprintf(&b, "+++ b/%s\n", path)
		fmt.Fprintf(&b, "@@ -0,0 +1,%d @@\n", len(lines))
		for _, line := range lines {
			fmt.Fprintf(&b, "+%s\n", line)
		}
		diff = b.String()
	}

	return DiffResult{
		Diff:  diff,
		Files: []string{path},
		Mode:  "snippet",
	}, nil
}

func buildDiffArgs(opts DiffOptions) []string {
	var args []string
	if opts.ContextLines > 0 {
		args = append(args, fmt.Sprintf("-U%d", opts.ContextLines))
	}
	args = append(args, "--")
	if len(opts.Include) > 0 {
		for _, p := range opts.Include {
			if p != "**/*" {
				args = append(args, p)
			}
		}
	}
	return args
}

func buildResult(ctx context.Context, diff, mode, rangeStr string, opts DiffOptions) (DiffResult, error) {
	meta, err := GetRepoMeta(ctx)
	if err != nil {
		meta = RepoMeta{}
	}

	files := extractFiles(diff)

	// Filter excludes before truncating so excluded files don't consume the byte budget
	if len(opts.Exclude) > 0 {
		diff = filterExcluded(diff, opts.Exclude)
		files = filterFileList(files, opts.Exclude)
	}

	if opts.MaxDiffBytes > 0 && len(diff) > opts.MaxDiffBytes {
		// Say so out of band as well as in the diff text. The in-band marker
		// is addressed to the model; a caller reading "Findings: 0" otherwise
		// has no way to tell a clean review from one that never saw most of
		// the change.
		fmt.Fprintf(os.Stderr, "warning: diff truncated from %d to %d bytes by max-diff-bytes; the remainder was not reviewed\n",
			len(diff), opts.MaxDiffBytes)
		diff = diff[:opts.MaxDiffBytes] + "\n... (diff truncated at max-diff-bytes limit)\n"
	}

	return DiffResult{
		Diff:  diff,
		Files: files,
		Mode:  mode,
		Range: rangeStr,
		Repo:  meta,
	}, nil
}

func extractFiles(diff string) []string {
	var files []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+++ b/") {
			f := strings.TrimPrefix(line, "+++ b/")
			if !seen[f] {
				seen[f] = true
				files = append(files, f)
			}
		}
	}
	return files
}

func filterExcluded(diff string, excludes []string) string {
	sections := diffutil.SplitSections(diff)
	var kept []string
	for _, section := range sections {
		path := diffutil.PathFromSection(section)
		if path == "" || !MatchesAny(path, excludes) {
			kept = append(kept, section)
		}
	}
	return strings.Join(kept, "")
}

func filterFileList(files []string, excludes []string) []string {
	var result []string
	for _, f := range files {
		if !MatchesAny(f, excludes) {
			result = append(result, f)
		}
	}
	return result
}

// MatchesAny returns true if the path matches any of the given glob patterns.
func MatchesAny(path string, patterns []string) bool {
	for _, pattern := range patterns {
		matched, err := filepath.Match(pattern, path)
		if err == nil && matched {
			return true
		}
		clean := strings.TrimPrefix(pattern, "**/")
		if clean != pattern {
			matched, err = filepath.Match(clean, filepath.Base(path))
			if err == nil && matched {
				return true
			}
			matched, err = filepath.Match(clean, path)
			if err == nil && matched {
				return true
			}
		}
	}
	return false
}

// maxFileBytes is the per-file size limit for codebase review.
const maxFileBytes = 1 << 20 // 1MB

// WalkFiles returns all git-tracked, non-binary files matching the
// include/exclude filters. Uses `git ls-files` for the file list and
// detects binaries via `git diff --no-index --numstat /dev/null <file>`.
func WalkFiles(opts DiffOptions) ([]string, error) {
	out, err := gitOutput("ls-files")
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}

	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Apply include filter
		if len(opts.Include) > 0 {
			if !MatchesAny(line, opts.Include) {
				continue
			}
		}
		// Apply exclude filter
		if len(opts.Exclude) > 0 {
			if MatchesAny(line, opts.Exclude) {
				continue
			}
		}
		// Skip binary files
		if isBinary(line) {
			continue
		}
		files = append(files, line)
	}

	sort.Strings(files)
	return files, nil
}

// isBinary detects binary files by scanning for null bytes in the first 512
// bytes — the same heuristic git uses. This avoids spawning a git subprocess
// per file (previously O(n) process launches for codebase mode).
func isBinary(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true // treat unreadable files as binary to skip them
	}
	var buf [512]byte
	n, readErr := f.Read(buf[:])
	_ = f.Close()
	// io.EOF with n==0 means the file is empty — not binary.
	// Any other read error with no bytes means unreadable; treat as binary to skip.
	if readErr != nil && n == 0 {
		return readErr != io.EOF
	}
	return bytes.IndexByte(buf[:n], 0) >= 0
}

// codebaseWorkers is the number of concurrent file-read goroutines used by
// Codebase. Disk I/O is the bottleneck on most systems, so 16 concurrent
// reads saturates SSDs without thrashing spinning-disk page caches.
const codebaseWorkers = 16

// Codebase reads all tracked source files and assembles them as synthetic
// unified diffs. Returns a DiffResult with Mode="codebase".
//
// Reads run in parallel (up to codebaseWorkers goroutines) via a pipeline:
// each file gets a dedicated buffered channel; the assembly consumer reads
// results in sorted order and cancels the derived context once the byte
// budget is exhausted, causing queued goroutines to skip their file reads
// rather than loading data that will be discarded.
func Codebase(ctx context.Context, opts DiffOptions) (DiffResult, error) {
	meta, err := GetRepoMeta(ctx)
	if err != nil {
		return DiffResult{}, err
	}
	files, err := WalkFiles(opts)
	if err != nil {
		return DiffResult{}, err
	}

	// Derive a cancellable context so the consumer can abort queued reads.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, codebaseWorkers)
	// One buffered channel per file: each producer sends exactly once (section
	// string or ""), so producers never block on send regardless of consumer
	// progress.
	chans := make([]chan string, len(files))
	for i := range chans {
		chans[i] = make(chan string, 1)
	}

	launchDone := make(chan struct{})
	var wg sync.WaitGroup

	go func() {
		defer close(launchDone)
		for i, path := range files {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				// Budget exhausted. Goroutines for 0..i-1 fill their own
				// channels; fill i..n-1 here so the consumer never deadlocks.
				for j := i; j < len(files); j++ {
					chans[j] <- ""
				}
				wg.Wait()
				return
			}
			wg.Add(1)
			go func(i int, path string) {
				defer wg.Done()
				defer func() { <-sem }()
				if ctx.Err() != nil {
					chans[i] <- ""
					return
				}
				data, readErr := os.ReadFile(path)
				if readErr != nil || len(data) > maxFileBytes {
					chans[i] <- ""
					return
				}
				// Check for binary content using the already-read data (same
				// heuristic as isBinary but without a second file open).
				checkLen := len(data)
				if checkLen > 512 {
					checkLen = 512
				}
				if bytes.IndexByte(data[:checkLen], 0) >= 0 {
					chans[i] <- ""
					return
				}
				// Empty files have no lines and no diff content.
				if len(data) == 0 {
					chans[i] <- ""
					return
				}
				// Split on newlines, then drop the trailing empty element that
				// bytes.Split produces for newline-terminated files. This gives
				// the correct line count for the hunk header without a TrimSuffix
				// that would wrongly collapse single-newline files to 0 lines.
				hasTrailingNewline := data[len(data)-1] == '\n'
				lines := bytes.Split(data, []byte("\n"))
				if hasTrailingNewline {
					lines = lines[:len(lines)-1]
				}
				var b strings.Builder
				b.Grow(len(data) + len(lines) + 128)
				b.WriteString("diff --git a/")
				b.WriteString(path)
				b.WriteString(" b/")
				b.WriteString(path)
				b.WriteString("\nnew file mode 100644\n--- /dev/null\n+++ b/")
				b.WriteString(path)
				b.WriteByte('\n')
				fmt.Fprintf(&b, "@@ -0,0 +1,%d @@\n", len(lines))
				for _, line := range lines {
					b.WriteByte('+')
					b.Write(line)
					b.WriteByte('\n')
				}
				if !hasTrailingNewline {
					b.WriteString("\\ No newline at end of file\n")
				}
				chans[i] <- b.String()
			}(i, path)
		}
		wg.Wait()
	}()

	// Consume results in sorted order, enforcing the byte budget.
	var combined strings.Builder
	var includedFiles []string
	totalBytes := 0
	for i, path := range files {
		s := <-chans[i]
		if s == "" {
			continue
		}
		if opts.MaxDiffBytes > 0 && totalBytes+len(s) > opts.MaxDiffBytes {
			cancel() // abort remaining goroutines
			break
		}
		combined.WriteString(s)
		includedFiles = append(includedFiles, path)
		totalBytes += len(s)
	}
	<-launchDone // wait for all goroutines before returning

	return DiffResult{
		Diff:  combined.String(),
		Files: includedFiles,
		Mode:  "codebase",
		Repo:  meta,
	}, nil
}

// CommitInfo holds a commit SHA and its subject line.
type CommitInfo struct {
	SHA     string
	Subject string
}

// ListCommits returns commits in a revision range, oldest first.
// If mergeBase is true, ".." is converted to "..." for merge-base comparison.
func ListCommits(revRange string, mergeBase bool) ([]CommitInfo, error) {
	listRange := revRange
	if mergeBase && strings.Contains(revRange, "..") && !strings.Contains(revRange, "...") {
		listRange = strings.Replace(revRange, "..", "...", 1)
	}

	// Use --format to get SHA and subject in a single git call.
	// Output format: "commit <sha>\n<subject>\n" per commit.
	out, err := gitOutput("rev-list", "--reverse", "--format=%s", listRange)
	if err != nil {
		return nil, fmt.Errorf("git rev-list %s: %w", revRange, err)
	}

	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}

	lines := strings.Split(out, "\n")
	var commits []CommitInfo
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "commit ") {
			continue
		}
		sha := strings.TrimPrefix(line, "commit ")
		var subject string
		if i+1 < len(lines) {
			subject = strings.TrimSpace(lines[i+1])
			i++ // skip the subject line
		}
		commits = append(commits, CommitInfo{
			SHA:     sha,
			Subject: subject,
		})
	}
	return commits, nil
}

func gitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return string(out), fmt.Errorf("%s: %s", err, string(exitErr.Stderr))
		}
		return "", err
	}
	return string(out), nil
}

// gitOutputCtx runs a git command respecting ctx cancellation and deadlines.
// Stderr is captured explicitly so error messages include git's diagnostics.
func gitOutputCtx(ctx context.Context, args ...string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return string(out), fmt.Errorf("%w: %s", err, msg)
		}
		return string(out), err
	}
	return string(out), nil
}
