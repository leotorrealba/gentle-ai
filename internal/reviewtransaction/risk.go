package reviewtransaction

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

const LargeChangeLines = 400
const MaxCorrectionChangedLines = 200

type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

type RiskSignal string

const (
	SignalAuth         RiskSignal = "auth"
	SignalUpdate       RiskSignal = "update"
	SignalSecurity     RiskSignal = "security"
	SignalPayments     RiskSignal = "payments"
	SignalDataExposure RiskSignal = "data_exposure"
	SignalDataLoss     RiskSignal = "data_loss"
	SignalPermissions  RiskSignal = "permissions"
	SignalShellProcess RiskSignal = "shell_process"
)

type DiffStat struct {
	Path      string
	Additions int
	Deletions int
	Binary    bool
	Generated bool
	ModeOnly  bool
}

type RiskInput struct {
	Stats                    []DiffStat
	Signals                  []RiskSignal
	OnlyNonExecutableChanges bool
	TouchesConfiguration     bool
}

// ClassifyRisk evaluates high, low, then medium. The first matching tier wins.
// Model, provider, profile, and effort are intentionally not classifier inputs.
func ClassifyRisk(input RiskInput) (RiskLevel, error) {
	changedLines, err := CountChangedLines(input.Stats)
	if err != nil {
		return "", err
	}
	for _, signal := range input.Signals {
		if !validRiskSignal(signal) {
			return "", fmt.Errorf("unknown risk signal %q", signal)
		}
	}

	if hasHighSignal(input.Signals) || touchesHotPath(input.Stats) || changedLines > LargeChangeLines {
		return RiskHigh, nil
	}
	if input.OnlyNonExecutableChanges && !input.TouchesConfiguration {
		return RiskLow, nil
	}
	return RiskMedium, nil
}

// CountChangedLines is the cross-adapter counting contract. Callers provide the
// canonical base-to-candidate union with one entry per repository-relative path.
// Authored text additions and deletions count once, including complete
// untracked/deleted text. Recognized generated goldens, binary, mode-only, and
// unchanged rename entries count as zero while remaining in snapshot identity.
func CountChangedLines(stats []DiffStat) (int, error) {
	total := 0
	seen := make(map[string]struct{}, len(stats))
	for _, stat := range stats {
		logicalPath, err := normalizeLogicalPath(stat.Path)
		if err != nil {
			return 0, err
		}
		if _, duplicate := seen[logicalPath]; duplicate {
			return 0, fmt.Errorf("duplicate diff stat path %q", logicalPath)
		}
		seen[logicalPath] = struct{}{}
		if stat.Additions < 0 || stat.Deletions < 0 {
			return 0, fmt.Errorf("negative diff stat for %q", logicalPath)
		}
		if isGeneratedGoldenPath(logicalPath) || stat.Binary || stat.ModeOnly {
			continue
		}
		total += stat.Additions + stat.Deletions
	}
	return total, nil
}

// CorrectionBudget freezes the maximum correction size from the original
// authored candidate. Odd line counts round up and the budget is capped at 200.
func CorrectionBudget(originalChangedLines int) (int, error) {
	if originalChangedLines < 0 {
		return 0, errors.New("original changed lines cannot be negative")
	}
	return min(MaxCorrectionChangedLines, originalChangedLines/2+originalChangedLines%2), nil
}

// ClassifySnapshotRisk derives both risk and changed lines from one immutable
// repository tree boundary and the canonical CountChangedLines contract.
func (builder SnapshotBuilder) ClassifySnapshotRisk(ctx context.Context, snapshot Snapshot) (RiskLevel, int, error) {
	stats, err := builder.DiffStats(ctx, snapshot)
	if err != nil {
		return "", 0, err
	}
	changedLines, err := CountChangedLines(stats)
	if err != nil {
		return "", 0, err
	}
	onlyNonExecutable := true
	touchesConfiguration := false
	for _, stat := range stats {
		if isGeneratedGoldenPath(stat.Path) {
			continue
		}
		onlyNonExecutable = onlyNonExecutable && isNonExecutableReviewPath(stat.Path)
		touchesConfiguration = touchesConfiguration || isConfigurationReviewPath(stat.Path)
	}
	risk, err := ClassifyRisk(RiskInput{
		Stats: stats, OnlyNonExecutableChanges: onlyNonExecutable, TouchesConfiguration: touchesConfiguration,
	})
	return risk, changedLines, err
}

func (builder SnapshotBuilder) ChangedLines(ctx context.Context, snapshot Snapshot) (int, error) {
	stats, err := builder.DiffStats(ctx, snapshot)
	if err != nil {
		return 0, err
	}
	return CountChangedLines(stats)
}

func isNonExecutableReviewPath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".mdx", ".rst", ".adoc", ".png", ".jpg", ".jpeg", ".gif", ".svg":
		return true
	default:
		return false
	}
}

func isConfigurationReviewPath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return true
	}
	switch base {
	case "go.mod", "go.sum", "package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "dockerfile", "makefile":
		return true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json", ".yaml", ".yml", ".toml", ".ini", ".env":
		return true
	default:
		return false
	}
}

func hasHighSignal(signals []RiskSignal) bool {
	return len(signals) > 0
}

func validRiskSignal(signal RiskSignal) bool {
	switch signal {
	case SignalAuth, SignalUpdate, SignalSecurity, SignalPayments,
		SignalDataExposure, SignalDataLoss, SignalPermissions, SignalShellProcess:
		return true
	default:
		return false
	}
}

func touchesHotPath(stats []DiffStat) bool {
	for _, stat := range stats {
		if isGeneratedGoldenPath(stat.Path) {
			continue
		}
		lower := strings.ToLower(stat.Path)
		for _, token := range strings.FieldsFunc(lower, func(r rune) bool {
			return r == '/' || r == '\\' || r == '.' || r == '-' || r == '_'
		}) {
			switch token {
			case "auth", "update", "security", "payments":
				return true
			}
		}
	}
	return false
}

func normalizeLogicalPath(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, '\x00') || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("invalid logical path %q", value)
	}
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned != value {
		return "", fmt.Errorf("logical path is not canonical: %q", value)
	}
	return cleaned, nil
}
