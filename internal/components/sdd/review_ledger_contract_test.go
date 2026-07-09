package sdd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gentleman-programming/gentle-ai/internal/agents"
	"github.com/gentleman-programming/gentle-ai/internal/assets"
	"github.com/gentleman-programming/gentle-ai/internal/model"
)

// judgeFacingLedgerClauses are the normative sentence fragments that govern
// the reviewing role itself (4R lenses and JD judges): the 4R v2 sweep
// budget, the precision gate, the persisted findings-ledger schema, and the
// artifact-store persistence branches. They belong both in every adopting
// asset AND inside the Judge Prompt fenced template.
var judgeFacingLedgerClauses = []string{
	// 4R v2 sweep budget: exactly 1 exhaustive sweep for standard reviews,
	// at most 2 for full-4R (hot path / >400 changed lines). Replaces the
	// v1 loop-until-dry mechanism entirely.
	"Standard review: run exactly 1 exhaustive sweep of the diff per lens, then stop",
	"run at most 2 sweeps per lens",
	"There is no loop-until-dry mechanism; the sweep budget is the entire first pass",
	// 4R v2 precision gate: precision over recall for every lens.
	"Report a finding only if it is a real, user-impacting defect you would defend with concrete evidence",
	"When in doubt, stay silent: a missed nitpick costs nothing; a false positive costs a full fix cycle",
	"Style and preference findings are banned unless they obscure a defect",
	// Ledger schema fields (design ADR 2). Full enum strings, not truncated
	// prefixes: a prefix match would still pass if a replicated asset dropped
	// a trailing enum value (JD-004). v2 adds `refuted` to the status enum.
	"`id` | `{LENS}-{NNN}`",
	"`lens` | risk \\| readability \\| reliability \\| resilience \\| judgment-day |",
	"`location` | `path/to/file.ext:line` or `:start-end`",
	"`severity` | BLOCKER \\| CRITICAL \\| WARNING \\| SUGGESTION |",
	"`status` | open \\| fixed \\| verified \\| refuted \\| wont-fix \\| info |",
	"`evidence` | why it matters |",
	"persist an empty ledger record rather than skip persistence",
	// Persistence branches on the artifact store (design ADR 4).
	"write `openspec/changes/{change-name}/review-ledger.md`",
	"upsert topic `sdd/{change-name}/review-ledger`",
	"ad-hoc judgment-day without a change: `review/{target-slug}/ledger`",
	// target-slug derivation rule (JD-002): deterministic so ad-hoc sessions
	// don't guess divergent keys.
	"`target-slug` = `pr-{number}` when reviewing a PR, else the current branch name kebab-cased, else a kebab-case slug of the user-stated review target",
	"do not write files or Engram artifacts",
	"the ledger lives only in this conversation",
	// Compaction caveat for the `none` store (JD-005), folded into the
	// hand-copied `none` bullet instead of living only in a non-copied note.
	"complete the review → fix → re-review loop within the session because it is not persisted across compaction",
}

// gatingLedgerClauses are the 4R v2 precision-gating clauses that govern what
// happens AFTER findings are emitted: adversarial verification of
// BLOCKER/CRITICAL candidates by refuter(s), the severity floor for the fix
// loop, and the convergence budget. They apply to the orchestration side of a
// review, not the judge's own prompt, so they are excluded from the Judge
// Prompt fence but required in every adopting asset.
var gatingLedgerClauses = []string{
	// Adversarial verification: only BLOCKER/CRITICAL candidates are verified,
	// with fixed review-level batching rather than per-candidate fan-out.
	"Only BLOCKER/CRITICAL candidates are verified; WARNING/SUGGESTION findings are never verified because they never drive fixes",
	"Standard review: exactly ONE general refuter total evaluates the complete merged list of all BLOCKER/CRITICAL candidates and returns one verdict per finding",
	"Full-4R review: exactly THREE refuters total evaluate that same complete merged candidate list through distinct lenses (correctness, exploitability/impact, reproducibility), each returning one verdict per finding",
	"Voting is independent per finding: refute a finding only when at least 2 of 3 lens verdicts refute it; a 1-of-3 result or tie keeps it",
	// Refutation protocol: WHO refutes, WHEN, and with WHAT delegation shape
	// (R2-001/R3-002). The refuter role itself lives in review-refuter.md.
	"The orchestrator invokes refutation once after merging lens ledgers and before any fix work; only BLOCKER/CRITICAL candidates are included",
	"The task ceiling is review-level and structural: 1 refuter task for a standard review or 3 total for full-4R, whether the list has 2 candidates or 20; NEVER spawn one refuter task per candidate",
	"Every task receives the complete merged candidate list",
	"Any malformed or missing per-finding verdict defaults to `stands` for that finding",
	"Judgment Day is the exception: its two-judge convergence satisfies adversarial verification and it spawns no `review-refuter` tasks",
	// Severity floor for the fix loop.
	"Only BLOCKER/CRITICAL findings that survive adversarial verification enter the fix → re-review loop",
	"reported once with status `info`, are never re-reviewed, and never block",
	"canonical severity remains `WARNING` and canonical status remains `info`; a WARNING is never `open`",
	// Convergence budget: bounded fix rounds, with the round and the fix actor
	// defined (R2-002).
	"Maximum 2 fix rounds per review",
	"One fix round = the orchestrator (directly or via a single writer sub-agent) applies fixes for all open verified BLOCKER/CRITICAL findings, then a scoped re-review verifies the fix diff against the ledger; in judgment-day the fix actor is `jd-fix-agent`",
	"still open after round 2 is reported to the user as open — the loop never extends",
}

// scopedReReviewLedgerClauses govern the re-review round that follows the fix
// agent. They are excluded from the Judge Prompt fence (JD-013) but required
// in every adopting asset.
var scopedReReviewLedgerClauses = []string{
	"receives ONLY the persisted ledger and the fix diff as input — never the original full diff",
	"MUST verify each ledger finding's resolution and MUST review only fix-touched lines",
	"MUST NOT re-read the full original diff",
	"MUST be logged with status `info` as a first-pass quality signal",
	"MUST NOT by itself trigger another full round",
}

// requiredLedgerClauses is the full clause set that MUST appear verbatim in
// every review-*, jd-judge-*, orchestrator "Review Execution Contract", and
// judgment-day skill asset.
var requiredLedgerClauses = concatClauses(
	judgeFacingLedgerClauses,
	gatingLedgerClauses,
	scopedReReviewLedgerClauses,
)

func concatClauses(groups ...[]string) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// requiredSubagentReviewModeClause is the execution-mode sentence every
// subagent-mode review-* lens asset must carry (design ADR 5).
const requiredSubagentReviewModeClause = "This is a subagent-mode review lens: emit your own ledger rows above; the orchestrator merges them into the persisted ledger."

// requiredOrchestratorMergeModeClause is the execution-mode sentence the
// dedicated-agent orchestrators must carry: even though the lenses and
// refuters run as subagents, the orchestrator still owns merge/persist/voting.
const requiredOrchestratorMergeModeClause = "Dedicated-agent mode (Claude, Cursor, Kimi, Kiro, OpenCode/Kilocode): each review-* agent runs its own sweep-budgeted pass and returns its own ledger rows; merge those rows into the persisted ledger. Refutation uses the fixed review-level fan-out above: exactly 1 batched task for standard review or exactly 3 batched tasks for full-4R; only the 3 full-4R tasks may run in parallel."

// requiredOrchestratorInlineModeClause is the execution-mode sentence every
// inline-mode orchestrator (no dedicated review-*/jd-* subagents) must carry.
const requiredOrchestratorInlineModeClause = "Inline mode (this adapter has no dedicated review-*/jd-* or `review-refuter` subagents): run each review lens sequentially inside your own orchestrator context and maintain the merged ledger directly. This clause overrides the generic delegation wording above: do not spawn refuter tasks; after merging candidates, run one general refutation pass for standard review or three lens passes sequentially for full-4R, each over the complete candidate list, then apply verdicts per finding with the same malformed/missing = `stands` and 2-of-3 rules."

// requiredJDSubagentModeClause is the execution-mode sentence every jd-judge-*.md
// subagent asset (Claude, Kiro) must carry. jd-fix-agent.md files do NOT carry
// this judge-oriented clause — see requiredFixAgentClauses (JD-001).
const requiredJDSubagentModeClause = "Judgment-day judges run as delegated agents; when this agent is a named sub-agent (Claude, Kiro), emit your own ledger rows and hand them to the orchestrator, which merges both judges' rows into the persisted ledger."

// requiredFixAgentClauses are the fix-specific clauses jd-fix-agent.md files
// must carry instead of the judge-oriented requiredLedgerClauses/
// requiredJDSubagentModeClause. The fix agent applies confirmed fixes; it
// does not run the first-pass review sweep and does not emit a findings
// ledger, so pasting the judge contract verbatim contradicts its own
// "fix ONLY confirmed issues" rules (JD-001).
var requiredFixAgentClauses = []string{
	"does NOT run the first-pass review sweep and does NOT emit a findings ledger",
	"Read the ledger entries the orchestrator confirmed and passed in the delegate prompt",
	"set that entry's `status` to `fixed`",
	"Never add new ledger rows: if fixing surfaces a new problem, report it back to the orchestrator instead of fixing it or logging it yourself",
	"it receives confirmed findings from the orchestrator, applies them, and hands control back to the orchestrator, which runs the scoped re-judge",
}

// requiredJudgePromptClauses is the subset of requiredLedgerClauses that
// belongs INSIDE the Judge Prompt fenced template itself: the sweep budget,
// the precision gate, the findings-ledger schema, and the ledger-persistence
// branches. The gating clauses (adversarial verification, severity floor,
// convergence budget) and the scoped re-review contract clauses govern the
// orchestration rounds around the judge, not the judge's own prompt, so they
// are excluded here (JD-013).
var requiredJudgePromptClauses = judgeFacingLedgerClauses

// assertContainsClauses fails the test for every clause missing from the
// embedded asset at path.
func assertContainsClauses(t *testing.T, path string, clauses []string) {
	t.Helper()
	content, err := assets.Read(path)
	if err != nil {
		t.Fatalf("assets.Read(%q) error = %v", path, err)
	}
	assertTextContainsClauses(t, path, content, clauses)
}

func assertTextContainsClauses(t *testing.T, label, content string, clauses []string) {
	t.Helper()
	for _, clause := range clauses {
		if !strings.Contains(content, clause) {
			t.Errorf("%s missing required ledger clause: %q", label, clause)
		}
	}
}

// reviewLedgerSubagentAssets enumerates the 16 review-* subagent files across
// the 4 adapter families with dedicated review-*/jd-* subagents, plus the 4
// jd-judge-* files for the 2 adapters that carry named judgment-day judges.
// jd-fix-agent.md files are enumerated separately in reviewLedgerFixAgentAssets
// because they carry a fix-specific clause set, not the judge-oriented one
// (JD-001).
var reviewLedgerSubagentAssets = map[string][]string{
	"claude": {
		"claude/agents/review-risk.md",
		"claude/agents/review-readability.md",
		"claude/agents/review-reliability.md",
		"claude/agents/review-resilience.md",
		"claude/agents/jd-judge-a.md",
		"claude/agents/jd-judge-b.md",
	},
	"cursor": {
		"cursor/agents/review-risk.md",
		"cursor/agents/review-readability.md",
		"cursor/agents/review-reliability.md",
		"cursor/agents/review-resilience.md",
	},
	"kimi": {
		"kimi/agents/review-risk.md",
		"kimi/agents/review-readability.md",
		"kimi/agents/review-reliability.md",
		"kimi/agents/review-resilience.md",
	},
	"kiro": {
		"kiro/agents/review-risk.md",
		"kiro/agents/review-readability.md",
		"kiro/agents/review-reliability.md",
		"kiro/agents/review-resilience.md",
		"kiro/agents/jd-judge-a.md",
		"kiro/agents/jd-judge-b.md",
	},
}

// reviewLedgerFixAgentAssets enumerates the jd-fix-agent.md files for the 2
// adapters that carry named judgment-day agents. These carry
// requiredFixAgentClauses instead of requiredLedgerClauses/
// requiredJDSubagentModeClause (JD-001).
var reviewLedgerFixAgentAssets = []string{
	"claude/agents/jd-fix-agent.md",
	"kiro/agents/jd-fix-agent.md",
}

// reviewLedgerRefuterAssets enumerates the review-refuter.md files for the 4
// adapter families that ship dedicated review-* agent assets. Like
// jd-fix-agent.md, the refuter carries its own role-specific clause set
// (requiredRefuterClauses) instead of the judge-oriented
// requiredLedgerClauses — it verifies a complete candidate list and never reviews
// a diff or emits a findings ledger (R2-001/R3-002).
var reviewLedgerRefuterAssets = []string{
	"claude/agents/review-refuter.md",
	"cursor/agents/review-refuter.md",
	"kimi/agents/review-refuter.md",
	"kiro/agents/review-refuter.md",
}

// requiredRefuterClauses are the refuter-specific clauses every
// review-refuter.md asset must carry: the batched candidate-list input contract, the
// four refutation lenses, the concrete-counter-evidence bar, the
// ties-favor-the-finding default, the read-only rule, and the verdict output
// contract (R2-001/R3-002).
var requiredRefuterClauses = []string{
	"`general` (standard single-refuter mode)",
	"complete merged list of BLOCKER/CRITICAL candidates",
	"`correctness`: is the claimed defect actually wrong behavior?",
	"`exploitability-impact`: can a real user or attacker ever hit it, and does it matter?",
	"`reproducibility`: can the failure scenario be concretely reproduced from the cited code?",
	"A refutation requires concrete counter-evidence",
	"\"Seems unlikely\" does not refute",
	"Default to `stands` when evidence is inconclusive: ties favor the finding",
	"Return one verdict for every candidate",
	"Do not omit candidates",
	"Never edit files",
	"Do not report new findings",
	"`verdict: refuted` or `verdict: stands`",
}

// judgeOnlyMarkers are judge-role clauses that must NOT appear in fix-agent
// assets. If the judge contract block (sweep budget, precision gate, findings
// ledger emission, judge execution mode) is ever pasted back into a
// fix-agent file alongside the fix clauses, these markers catch it (JD-011).
var judgeOnlyMarkers = []string{
	"**Sweep budget.**",
	"**Precision gate.**",
	"Emit a findings ledger with this schema for every entry",
	requiredJDSubagentModeClause,
}

// assertNotContainsMarkers fails the test if any marker is present in content.
func assertNotContainsMarkers(t *testing.T, path, content string, markers []string) {
	t.Helper()
	for _, marker := range markers {
		if strings.Contains(content, marker) {
			t.Errorf("%s must NOT contain judge-only marker: %q", path, marker)
		}
	}
}

// extractFencedBlockAfterHeading returns the contents of the first fenced
// code block that follows the given markdown heading in content. It fails
// the test if the heading or a subsequent fence cannot be found, so a clause
// that lives in prose outside the fence (placement drift, JD-013) cannot
// silently satisfy a whole-file strings.Contains check.
func extractFencedBlockAfterHeading(t *testing.T, label, content, heading string) string {
	t.Helper()
	headingIdx := strings.Index(content, heading)
	if headingIdx == -1 {
		t.Fatalf("%s: heading %q not found", label, heading)
	}
	rest := content[headingIdx+len(heading):]
	fenceStart := strings.Index(rest, "```")
	if fenceStart == -1 {
		t.Fatalf("%s: no fenced block found after heading %q", label, heading)
	}
	afterFenceOpen := rest[fenceStart+3:]
	newlineIdx := strings.Index(afterFenceOpen, "\n")
	if newlineIdx == -1 {
		t.Fatalf("%s: malformed fence after heading %q", label, heading)
	}
	body := afterFenceOpen[newlineIdx+1:]
	fenceEnd := strings.Index(body, "```")
	if fenceEnd == -1 {
		t.Fatalf("%s: unterminated fenced block after heading %q", label, heading)
	}
	return body[:fenceEnd]
}

// reviewLedgerOrchestratorAssets enumerates the 12 sdd-orchestrator.md source
// files (OpenCode's single source file also renders the Kilocode variant —
// see TestRequiredLedgerClauses/kilocode below).
var reviewLedgerOrchestratorAssets = map[string]string{
	"claude":      "claude/sdd-orchestrator.md",
	"cursor":      "cursor/sdd-orchestrator.md",
	"kimi":        "kimi/sdd-orchestrator.md",
	"kiro":        "kiro/sdd-orchestrator.md",
	"codex":       "codex/sdd-orchestrator.md",
	"gemini":      "gemini/sdd-orchestrator.md",
	"qwen":        "qwen/sdd-orchestrator.md",
	"windsurf":    "windsurf/sdd-orchestrator.md",
	"antigravity": "antigravity/sdd-orchestrator.md",
	"hermes":      "hermes/sdd-orchestrator.md",
	"generic":     "generic/sdd-orchestrator.md",
	"opencode":    "opencode/sdd-orchestrator.md",
}

// subagentFamilyOrchestrators are the orchestrators that also need the
// merge-side clause because their review/refuter roles run as subagents.
var subagentFamilyOrchestrators = map[string]bool{
	"claude":   true,
	"cursor":   true,
	"kimi":     true,
	"kiro":     true,
	"opencode": true,
}

// reviewLedgerJudgmentDaySkillAssets enumerates the 2 judgment-day skill docs
// shared by every adapter's judgment-day flow.
var reviewLedgerJudgmentDaySkillAssets = []string{
	"skills/judgment-day/SKILL.md",
	"skills/judgment-day/references/prompts-and-formats.md",
}

// TestRequiredLedgerClauses is the RED->GREEN consistency test for this
// change: it asserts every one of the 16 review-* subagent files, 13
// orchestrator variants (12 source files + the Kilocode-rendered variant),
// and 8 judgment-day files carries the full requiredLedgerClauses set.
//
// RED evidence: on the first run (before any asset edit), every subtest below
// fails because no asset yet carries the contract — see apply-progress.md for
// the captured failing-test output.
func TestRequiredLedgerClauses(t *testing.T) {
	t.Run("subagent_review_and_jd_assets", func(t *testing.T) {
		for family, paths := range reviewLedgerSubagentAssets {
			t.Run(family, func(t *testing.T) {
				for _, p := range paths {
					t.Run(filepath.Base(p), func(t *testing.T) {
						assertContainsClauses(t, p, requiredLedgerClauses)
						if strings.HasPrefix(filepath.Base(p), "review-") {
							content := assets.MustRead(p)
							assertTextContainsClauses(t, p, content, []string{requiredSubagentReviewModeClause})
						}
						if strings.HasPrefix(filepath.Base(p), "jd-") {
							content := assets.MustRead(p)
							assertTextContainsClauses(t, p, content, []string{requiredJDSubagentModeClause})
						}
					})
				}
			})
		}
	})

	t.Run("fix_agent_assets", func(t *testing.T) {
		for _, p := range reviewLedgerFixAgentAssets {
			t.Run(filepath.Base(p), func(t *testing.T) {
				content := assets.MustRead(p)
				assertTextContainsClauses(t, p, content, requiredFixAgentClauses)
				assertNotContainsMarkers(t, p, content, judgeOnlyMarkers)
			})
		}
	})

	t.Run("refuter_assets", func(t *testing.T) {
		for _, p := range reviewLedgerRefuterAssets {
			t.Run(strings.SplitN(p, "/", 2)[0], func(t *testing.T) {
				content := assets.MustRead(p)
				assertTextContainsClauses(t, p, content, requiredRefuterClauses)
				assertNotContainsMarkers(t, p, content, judgeOnlyMarkers)
			})
		}
	})

	t.Run("orchestrator_assets", func(t *testing.T) {
		for family, path := range reviewLedgerOrchestratorAssets {
			t.Run(family, func(t *testing.T) {
				assertContainsClauses(t, path, requiredLedgerClauses)
				content := assets.MustRead(path)
				if subagentFamilyOrchestrators[family] {
					assertTextContainsClauses(t, path, content, []string{requiredOrchestratorMergeModeClause})
				} else {
					assertTextContainsClauses(t, path, content, []string{requiredOrchestratorInlineModeClause})
				}
			})
		}
	})

	// Kilocode/OpenCode have no dedicated source asset: sddOrchestratorAsset maps
	// both AgentOpenCode and AgentKilocode to "opencode/sdd-orchestrator.md".
	// Render through the real Inject() path to prove the shared source actually
	// reaches each adapter's Kilocode/OpenCode-rendered gentle-orchestrator
	// prompt, not just a static file read.
	for _, tc := range []struct {
		name    string
		adapter agents.Adapter
	}{
		{name: "opencode", adapter: opencodeAdapter()},
		{name: "kilocode", adapter: kilocodeAdapter()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if _, err := Inject(home, tc.adapter, model.SDDModeMulti); err != nil {
				t.Fatalf("Inject(%s) error = %v", tc.name, err)
			}
			settingsPath := tc.adapter.SettingsPath(home)
			prompt := readGentleOrchestratorPrompt(t, settingsPath)
			assertTextContainsClauses(t, tc.name+" gentle-orchestrator prompt", prompt, requiredLedgerClauses)
			assertTextContainsClauses(t, tc.name+" gentle-orchestrator prompt", prompt, []string{requiredOrchestratorMergeModeClause})
			assertRenderedReviewRefuter(t, settingsPath)
			assertRenderedOpenCodeReviewPrompts(t, settingsPath)
		})
	}

	t.Run("judgment_day_skill_assets", func(t *testing.T) {
		for _, p := range reviewLedgerJudgmentDaySkillAssets {
			t.Run(filepath.Base(p), func(t *testing.T) {
				assertContainsClauses(t, p, requiredLedgerClauses)
			})
		}
	})

	// prompts-and-formats.md carries both templates: the Judge Prompt template
	// and the Fix Agent Prompt template. Both assertions below extract the
	// actual fenced block under each heading and assert clauses against that
	// EXTRACTED FENCE, not the whole file — a whole-file strings.Contains
	// check would still pass if a clause lived in prose outside its fence
	// (JD-013), which is exactly the placement-drift class this guards
	// against. Fix clauses must match requiredFixAgentClauses verbatim so the
	// template can't silently drift from jd-fix-agent.md (JD-009).
	t.Run("judgment_day_skill_fix_agent_clauses", func(t *testing.T) {
		const p = "skills/judgment-day/references/prompts-and-formats.md"
		content := assets.MustRead(p)

		fixAgentBlock := extractFencedBlockAfterHeading(t, p, content, "## Fix Agent Prompt")
		assertTextContainsClauses(t, p+" Fix Agent Prompt fence", fixAgentBlock, requiredFixAgentClauses)

		judgeBlock := extractFencedBlockAfterHeading(t, p, content, "## Judge Prompt")
		assertTextContainsClauses(t, p+" Judge Prompt fence", judgeBlock, requiredJudgePromptClauses)
	})
}

func TestReviewRefutersAreStructurallyReadOnly(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		frontmatter := markdownFrontmatter(t, "claude/agents/review-refuter.md")
		if !strings.Contains(frontmatter, "tools: Read, Grep, Glob") {
			t.Fatalf("Claude refuter tools must be the read-only allowlist; got:\n%s", frontmatter)
		}
		assertNoCommandOrWriteTools(t, "Claude refuter", frontmatter)
	})

	t.Run("kiro", func(t *testing.T) {
		frontmatter := markdownFrontmatter(t, "kiro/agents/review-refuter.md")
		if !strings.Contains(frontmatter, `tools: ["read"]`) {
			t.Fatalf("Kiro refuter tools must contain only read; got:\n%s", frontmatter)
		}
		assertNoCommandOrWriteTools(t, "Kiro refuter", frontmatter)
	})

	t.Run("cursor", func(t *testing.T) {
		frontmatter := markdownFrontmatter(t, "cursor/agents/review-refuter.md")
		if !strings.Contains(frontmatter, "readonly: true") {
			t.Fatalf("Cursor refuter must set readonly: true; got:\n%s", frontmatter)
		}
		assertNoCommandOrWriteTools(t, "Cursor refuter", frontmatter)
	})

	t.Run("kimi", func(t *testing.T) {
		const path = "kimi/agents/review-refuter.yaml"
		content := assets.MustRead(path)
		for _, excluded := range []string{
			"kimi_cli.tools.multiagent:Task",
			"kimi_cli.tools.shell:Shell",
			"kimi_cli.tools.file:WriteFile",
			"kimi_cli.tools.file:StrReplaceFile",
		} {
			if !strings.Contains(content, excluded) {
				t.Errorf("%s must exclude %q", path, excluded)
			}
		}
		if strings.Contains(content, "\n  tools:") {
			t.Fatalf("%s must not add a positive tool allowlist that can restore command/write tools", path)
		}
	})

	for _, path := range []string{"opencode/sdd-overlay-single.json", "opencode/sdd-overlay-multi.json"} {
		t.Run(path, func(t *testing.T) {
			var root map[string]any
			if err := json.Unmarshal([]byte(assets.MustRead(path)), &root); err != nil {
				t.Fatalf("Unmarshal(%s) error = %v", path, err)
			}
			agentMap := root["agent"].(map[string]any)
			refuter, ok := agentMap["review-refuter"].(map[string]any)
			if !ok {
				t.Fatalf("%s missing review-refuter definition", path)
			}
			tools, ok := refuter["tools"].(map[string]any)
			if !ok {
				t.Fatalf("%s review-refuter tools has type %T, want object", path, refuter["tools"])
			}
			assertOpenCodeRefuterToolsReadOnly(t, path, tools)
			prompt, _ := refuter["prompt"].(string)
			for _, clause := range []string{
				"complete merged list of BLOCKER/CRITICAL candidates",
				"one verdict entry per candidate",
				"malformed or missing per-finding verdict as stands",
			} {
				if !strings.Contains(prompt, clause) {
					t.Errorf("%s review-refuter prompt missing %q", path, clause)
				}
			}
		})
	}
}

func TestOpenCodeOverlayReviewPromptsCarryLedgerContract(t *testing.T) {
	for _, path := range []string{"opencode/sdd-overlay-single.json", "opencode/sdd-overlay-multi.json"} {
		t.Run(path, func(t *testing.T) {
			var root map[string]any
			if err := json.Unmarshal([]byte(assets.MustRead(path)), &root); err != nil {
				t.Fatalf("Unmarshal(%s) error = %v", path, err)
			}
			agentMap, ok := root["agent"].(map[string]any)
			if !ok {
				t.Fatalf("%s missing agent map", path)
			}
			assertOpenCodeReviewAgentPrompts(t, path, agentMap)
		})
	}
}

func markdownFrontmatter(t *testing.T, path string) string {
	t.Helper()
	content := assets.MustRead(path)
	parts := strings.SplitN(content, "---", 3)
	if len(parts) != 3 {
		t.Fatalf("%s missing YAML frontmatter", path)
	}
	return parts[1]
}

func assertNoCommandOrWriteTools(t *testing.T, label, frontmatter string) {
	t.Helper()
	lower := strings.ToLower(frontmatter)
	for _, forbidden := range []string{"bash", "shell", "command", "write", "edit"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("%s frontmatter grants forbidden command/write tool %q:\n%s", label, forbidden, frontmatter)
		}
	}
}

// readGentleOrchestratorPrompt reads back the rendered gentle-orchestrator
// agent prompt from an OpenCode/Kilocode opencode.json settings file.
func readGentleOrchestratorPrompt(t *testing.T, settingsPath string) string {
	t.Helper()
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", settingsPath, err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("Unmarshal(%q) error = %v", settingsPath, err)
	}
	agentsMap, ok := root["agent"].(map[string]any)
	if !ok {
		t.Fatalf("%q: missing top-level \"agent\" map", settingsPath)
	}
	orchestrator, ok := agentsMap["gentle-orchestrator"].(map[string]any)
	if !ok {
		t.Fatalf("%q: missing \"agent.gentle-orchestrator\"", settingsPath)
	}
	prompt, ok := orchestrator["prompt"].(string)
	if !ok {
		t.Fatalf("%q: \"agent.gentle-orchestrator.prompt\" is not a string", settingsPath)
	}
	return prompt
}

func assertRenderedReviewRefuter(t *testing.T, settingsPath string) {
	t.Helper()
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", settingsPath, err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("Unmarshal(%q) error = %v", settingsPath, err)
	}
	agentMap := root["agent"].(map[string]any)
	refuter, ok := agentMap[reviewRefuterAgentName].(map[string]any)
	if !ok {
		t.Fatalf("%q missing rendered %s agent", settingsPath, reviewRefuterAgentName)
	}
	tools, ok := refuter["tools"].(map[string]any)
	if !ok {
		t.Fatalf("%q rendered %s tools has type %T, want object", settingsPath, reviewRefuterAgentName, refuter["tools"])
	}
	assertOpenCodeRefuterToolsReadOnly(t, settingsPath, tools)
	orchestrator := agentMap["gentle-orchestrator"].(map[string]any)
	permission := orchestrator["permission"].(map[string]any)
	task := permission["task"].(map[string]any)
	if replacement, ok := task["__replace__"].(map[string]any); ok {
		task = replacement
	}
	if task[reviewRefuterAgentName] != "allow" {
		t.Fatalf("%q gentle-orchestrator cannot invoke %s: %#v", settingsPath, reviewRefuterAgentName, task)
	}
}

func assertOpenCodeRefuterToolsReadOnly(t *testing.T, label string, tools map[string]any) {
	t.Helper()
	want := map[string]bool{
		"read":  true,
		"write": false,
		"edit":  false,
		"bash":  false,
		"task":  false,
	}
	if len(tools) != len(want) {
		t.Fatalf("%s review-refuter tools = %#v, want explicit read-only overrides %#v", label, tools, want)
	}
	for tool, wantEnabled := range want {
		got, ok := tools[tool].(bool)
		if !ok || got != wantEnabled {
			t.Errorf("%s review-refuter tool %q = %v, want %v", label, tool, tools[tool], wantEnabled)
		}
	}
}

func assertRenderedOpenCodeReviewPrompts(t *testing.T, settingsPath string) {
	t.Helper()
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", settingsPath, err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("Unmarshal(%q) error = %v", settingsPath, err)
	}
	agentMap, ok := root["agent"].(map[string]any)
	if !ok {
		t.Fatalf("%q missing agent map", settingsPath)
	}
	assertOpenCodeReviewAgentPrompts(t, settingsPath, agentMap)
}

func assertOpenCodeReviewAgentPrompts(t *testing.T, label string, agentMap map[string]any) {
	t.Helper()
	reviewAgents := map[string]struct {
		idPrefix string
		lens     string
	}{
		"review-risk":        {idPrefix: "R1-NNN", lens: "risk"},
		"review-readability": {idPrefix: "R2-NNN", lens: "readability"},
		"review-reliability": {idPrefix: "R3-NNN", lens: "reliability"},
		"review-resilience":  {idPrefix: "R4-NNN", lens: "resilience"},
	}
	commonClauses := []string{
		"Standard review: run exactly 1 exhaustive sweep of the diff per lens, then stop",
		"run at most 2 sweeps per lens",
		"There is no loop-until-dry mechanism; the sweep budget is the entire first pass",
		"Report a finding only if it is a real, user-impacting defect you would defend with concrete evidence",
		"When in doubt, stay silent: a missed nitpick costs nothing; a false positive costs a full fix cycle",
		"Style and preference findings are banned unless they obscure a defect",
		"location path/to/file.ext:line or :start-end",
		"severity BLOCKER | CRITICAL | WARNING | SUGGESTION",
		"status open | fixed | verified | refuted | wont-fix | info",
		"evidence explaining the concrete defect and user impact",
		"BLOCKER/CRITICAL findings use status open; WARNING/SUGGESTION findings use status info",
		"deterministically merge candidates for batched per-finding refutation",
		"If clean, say exactly: No findings.",
	}
	for agentName, expected := range reviewAgents {
		agent, ok := agentMap[agentName].(map[string]any)
		if !ok {
			t.Errorf("%s missing %s agent", label, agentName)
			continue
		}
		prompt, ok := agent["prompt"].(string)
		if !ok {
			t.Errorf("%s %s prompt has type %T, want string", label, agentName, agent["prompt"])
			continue
		}
		assertTextContainsClauses(t, label+" "+agentName+" prompt", prompt, commonClauses)
		assertTextContainsClauses(t, label+" "+agentName+" prompt", prompt, []string{
			"stable id " + expected.idPrefix,
			"lens " + expected.lens,
		})
	}
}

// TestReviewLedgerContractSchema is the golden-string assertion test (task
// 1.2) against the canonical _shared source file: id format, severity enum,
// and status enum must be present exactly as the schema table defines them.
//
// RED evidence: fails until internal/assets/skills/_shared/review-ledger-contract.md
// is created (task 1.3).
func TestReviewLedgerContractSchema(t *testing.T) {
	const path = "skills/_shared/review-ledger-contract.md"
	content, err := assets.Read(path)
	if err != nil {
		t.Fatalf("assets.Read(%q) error = %v", path, err)
	}

	assertTextContainsClauses(t, path, content, requiredLedgerClauses)

	for _, want := range []string{
		"`{LENS}-{NNN}`",
		"BLOCKER \\| CRITICAL \\| WARNING \\| SUGGESTION",
		"open \\| fixed \\| verified \\| refuted \\| wont-fix \\| info",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("%s missing schema fixture %q", path, want)
		}
	}
}
