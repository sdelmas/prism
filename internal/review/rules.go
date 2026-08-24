package review

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Rules represents a rules pack loaded from --rules.
type Rules struct {
	Focus             []string          `json:"focus,omitempty"`
	SeverityOverrides map[string]string `json:"severityOverrides,omitempty"`
	Required          []RequiredCheck   `json:"required,omitempty"`
}

// RequiredCheck is a policy check that should always be enforced.
type RequiredCheck struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// LoadRules loads a rules file from disk. Returns nil Rules and nil error if path is empty.
// DefaultRulesPath is the repository-local rules file used when none is named.
const DefaultRulesPath = ".prism/rules.json"

func LoadRules(path string) (*Rules, error) {
	if path == "" {
		// Fall back to the repository-local rules file. Without this, a repo
		// that ships .prism/rules.json gets none of it -- no focus, no
		// severityOverrides, none of its required checks -- unless every
		// invocation remembers --rules. Silence is the worst failure mode
		// here: the review still runs and still reports findings, so nothing
		// tells the caller their policy was never applied.
		if _, err := os.Stat(DefaultRulesPath); err != nil {
			return nil, nil
		}
		path = DefaultRulesPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading rules file: %w", err)
	}
	var rules Rules
	if err := json.Unmarshal(data, &rules); err != nil {
		return nil, fmt.Errorf("parsing rules file: %w", err)
	}
	return &rules, nil
}

// BuildRulesPromptSection returns additional prompt instructions derived from rules.
func BuildRulesPromptSection(rules *Rules) string {
	if rules == nil {
		return ""
	}

	var b strings.Builder

	if len(rules.Focus) > 0 {
		fmt.Fprintf(&b, "\nFocus areas: %s. Prioritize findings in these areas.\n",
			strings.Join(rules.Focus, ", "))
	}

	if len(rules.SeverityOverrides) > 0 {
		b.WriteString("\nSeverity policy:\n")
		for cat, sev := range rules.SeverityOverrides {
			fmt.Fprintf(&b, "- %s findings should be rated as %s severity.\n", cat, sev)
		}
	}

	if len(rules.Required) > 0 {
		b.WriteString("\nRequired checks (always evaluate these):\n")
		for _, req := range rules.Required {
			fmt.Fprintf(&b, "- [%s] %s\n", req.ID, req.Text)
		}
	}

	return b.String()
}

// ApplySeverityOverrides post-processes findings to enforce severity overrides from rules.
func ApplySeverityOverrides(findings []Finding, rules *Rules) []Finding {
	if rules == nil || len(rules.SeverityOverrides) == 0 {
		return findings
	}

	for i := range findings {
		cat := string(findings[i].Category)
		if override, ok := rules.SeverityOverrides[cat]; ok {
			findings[i].Severity = Severity(override)
			// Regenerate ID since severity change may affect dedup
			findings[i].ID = generateFindingID(findings[i])
		}
	}
	return findings
}
