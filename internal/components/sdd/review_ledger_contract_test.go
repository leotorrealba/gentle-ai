package sdd

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/gentleman-programming/gentle-ai/internal/assets"
)

var requiredLedgerClauses = boundedReviewRequiredClauses

const requiredOrchestratorMergeModeClause = "Only the parent orchestrator may launch a correction actor or scoped validator"

func TestBoundedReviewContractDistinguishesClaimsFromStrictNativeLedger(t *testing.T) {
	content := boundedReviewContract()
	for _, want := range []string{
		"`id` | `{LENS}-{NNN}`",
		"`evidence_class` | `deterministic | inferential | insufficient`",
		"`proof_refs` | Concrete command, output hash, or `file:line` references",
		"`status` | `open | corroborated | refuted | inconclusive | fixed | verified | info`",
		"exact native `Finding` fields `id`, `lens`, `location`, `severity`, `claim`, and `proof_refs`",
		"MUST NOT serialize `evidence_class` or `status` into the strict native ledger",
		"Unknown native finding fields remain rejected",
		"`approved | escalated`",
		"`scope-changed | invalidated`",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("canonical bounded review contract missing %q", want)
		}
	}
}

func TestDedicatedReviewAndJudgmentAssetsRenderCanonicalContract(t *testing.T) {
	assetsByFamily := map[string][]string{
		"claude": {
			"claude/agents/review-risk.md", "claude/agents/review-readability.md",
			"claude/agents/review-reliability.md", "claude/agents/review-resilience.md",
			"claude/agents/jd-judge-a.md", "claude/agents/jd-judge-b.md",
		},
		"cursor": {
			"cursor/agents/review-risk.md", "cursor/agents/review-readability.md",
			"cursor/agents/review-reliability.md", "cursor/agents/review-resilience.md",
		},
		"kimi": {
			"kimi/agents/review-risk.md", "kimi/agents/review-readability.md",
			"kimi/agents/review-reliability.md", "kimi/agents/review-resilience.md",
		},
		"kiro": {
			"kiro/agents/review-risk.md", "kiro/agents/review-readability.md",
			"kiro/agents/review-reliability.md", "kiro/agents/review-resilience.md",
			"kiro/agents/jd-judge-a.md", "kiro/agents/jd-judge-b.md",
		},
	}
	for family, paths := range assetsByFamily {
		for _, path := range paths {
			t.Run(family+"/"+path, func(t *testing.T) {
				content := renderBoundedReviewAsset(path)
				assertTextContainsClauses(t, path, content, boundedReviewRequiredClauses)
			})
		}
	}
}

func TestDedicatedReviewersAndRefutersAreStructurallyReadOnly(t *testing.T) {
	for _, path := range []string{
		"claude/agents/review-risk.md", "claude/agents/review-readability.md",
		"claude/agents/review-reliability.md", "claude/agents/review-resilience.md",
		"claude/agents/review-refuter.md",
	} {
		frontmatter := markdownFrontmatter(t, path)
		for _, forbidden := range []string{"Bash", "Write", "Edit"} {
			if strings.Contains(frontmatter, forbidden) {
				t.Errorf("%s frontmatter grants %s", path, forbidden)
			}
		}
	}
	for _, path := range []string{
		"kiro/agents/review-risk.md", "kiro/agents/review-readability.md",
		"kiro/agents/review-reliability.md", "kiro/agents/review-resilience.md",
		"kiro/agents/review-refuter.md", "kiro/agents/jd-judge-a.md", "kiro/agents/jd-judge-b.md",
	} {
		if frontmatter := markdownFrontmatter(t, path); !strings.Contains(frontmatter, `tools: ["read"]`) {
			t.Errorf("%s is not read-only:\n%s", path, frontmatter)
		}
	}
	for _, path := range []string{
		"cursor/agents/review-risk.md", "cursor/agents/review-readability.md",
		"cursor/agents/review-reliability.md", "cursor/agents/review-resilience.md",
		"cursor/agents/review-refuter.md",
	} {
		if frontmatter := markdownFrontmatter(t, path); !strings.Contains(frontmatter, "readonly: true") {
			t.Errorf("%s is not read-only", path)
		}
	}
	for _, path := range []string{
		"kimi/agents/review-risk.yaml", "kimi/agents/review-readability.yaml",
		"kimi/agents/review-reliability.yaml", "kimi/agents/review-resilience.yaml",
		"kimi/agents/review-refuter.yaml",
	} {
		content := assets.MustRead(path)
		for _, excluded := range []string{"multiagent:Task", "shell:Shell", "file:WriteFile", "file:StrReplaceFile"} {
			if !strings.Contains(content, excluded) {
				t.Errorf("%s does not exclude %s", path, excluded)
			}
		}
	}
}

func TestOpenCodeOverlaysRenderBoundedReadOnlyReviewRoles(t *testing.T) {
	for _, path := range []string{"opencode/sdd-overlay-single.json", "opencode/sdd-overlay-multi.json"} {
		t.Run(path, func(t *testing.T) {
			var root map[string]any
			if err := json.Unmarshal([]byte(assets.MustRead(path)), &root); err != nil {
				t.Fatal(err)
			}
			agentsMap := root["agent"].(map[string]any)
			expandOpenCodeBoundedReviewAgents(agentsMap)
			for _, name := range []string{"review-risk", "review-readability", "review-reliability", "review-resilience"} {
				agent := agentsMap[name].(map[string]any)
				prompt := agent["prompt"].(string)
				assertTextContainsClauses(t, path+" "+name, prompt, boundedReviewRequiredClauses)
				assertOpenCodeReadOnlyTools(t, path+" "+name, agent["tools"].(map[string]any))
			}
			refuter := agentsMap[reviewRefuterAgentName].(map[string]any)
			if prompt := refuter["prompt"].(string); !strings.Contains(prompt, "exactly ONE transaction-wide inferential batch") || !strings.Contains(prompt, "terminate") {
				t.Errorf("%s refuter prompt is not bounded: %s", path, prompt)
			}
			assertOpenCodeReadOnlyTools(t, path+" refuter", refuter["tools"].(map[string]any))
		})
	}
}

func markdownFrontmatter(t *testing.T, path string) string {
	t.Helper()
	parts := strings.SplitN(assets.MustRead(path), "---", 3)
	if len(parts) != 3 {
		t.Fatalf("%s missing frontmatter", path)
	}
	return parts[1]
}

func assertOpenCodeReadOnlyTools(t *testing.T, label string, tools map[string]any) {
	t.Helper()
	want := map[string]bool{"read": true, "write": false, "edit": false, "bash": false, "task": false}
	if len(tools) != len(want) {
		t.Fatalf("%s tools = %#v", label, tools)
	}
	for name, expected := range want {
		if got, ok := tools[name].(bool); !ok || got != expected {
			t.Errorf("%s tool %s = %v, want %v", label, name, tools[name], expected)
		}
	}
}

func assertTextContainsClauses(t *testing.T, label, content string, clauses []string) {
	t.Helper()
	for _, clause := range clauses {
		if !strings.Contains(content, clause) {
			t.Errorf("%s missing required clause %q", label, clause)
		}
	}
}

func readGentleOrchestratorPrompt(t *testing.T, settingsPath string) string {
	t.Helper()
	payload, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		t.Fatal(err)
	}
	agentsMap := root["agent"].(map[string]any)
	orchestrator := agentsMap["gentle-orchestrator"].(map[string]any)
	return orchestrator["prompt"].(string)
}

func assertOpenCodeRefuterToolsReadOnly(t *testing.T, label string, tools map[string]any) {
	t.Helper()
	assertOpenCodeReadOnlyTools(t, label, tools)
}
