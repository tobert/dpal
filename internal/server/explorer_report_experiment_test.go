//go:build e2e

// Experiment harness for the "explorer writes a curated report" idea
// (see docs/issues.md). Runs the cheap flash model through the explore
// tool-loop ALONE — no synthesizer — with several curator prompt
// variants, and logs each resulting report so we can eyeball fidelity
// vs. compression before deciding whether to wire any of it into the
// two-phase Consult flow.
//
// This is white-box (package server) so it can call s.chat directly and
// KEEP flash's final text — the report — which the production path
// strips via stripExplorerSynthesis.
//
//	DEEPSEEK_API_KEY=... go test -tags=e2e -run TestExperiment_ExplorerReport -v ./internal/server/
//
// It is a measurement tool, not a pass/fail test: it never fails on
// content, only on transport errors. Read the -v log to compare variants.
package server

import (
	"context"
	"os"
	"strings"
	"testing"

	deepseek "github.com/cohesion-org/deepseek-go"

	"github.com/tobert/dpal/internal/explorer"
)

// curatorBase is the shared loading discipline for every variant. It
// keeps the production explorer's good parts (tool list, two-shot search
// policy, read-once hygiene) but REVERSES the "do not synthesize, emit
// one sentence" ending: here flash's job is to load AND THEN write a
// curated report. Per-variant report specs are appended to this.
const curatorBase = `You are an exploration assistant inside dpal. You read a project on behalf of a
separate, more capable model that will write the final answer. You have two jobs,
in order: (1) LOAD the context the answer depends on, then (2) CURATE it into a
report the answering model can work from.

Thinking mode is on. Aim your reasoning at one question while loading — which
files does the answer depend on? — then at curation: what does the answering
model actually need to see, and what can be condensed or dropped?

# Tools (available only to you)
- project_tree(path)        — the project's file layout in one call (honors .gitignore)
- list_directory(path)      — list entries under the project root
- read_file(path)           — read one file under the project root
- search_project(pattern, glob) — Go RE2 regex search across files under the root

# Loading discipline
- project_tree('.') maps the layout in one call; use it to pick the few files you need.
- Two searches per concept maximum. If two well-formed searches return nothing,
  treat the concept as absent and move on.
- Read each file once — re-reading wastes a call and tells you nothing new.
- Tool results are ground truth; do not re-verify them.
- If a tool errors, do not retry it; note the gap in your report instead.

# When you have enough
Stop loading as soon as the answering model could ground a good answer from what
you've seen. Then write the report.`

// The three report specs sit on one axis: how aggressively flash
// editorializes. A compresses most (prose digest), C least (cleaned
// verbatim pack). All three share Amy's constants: explicit BEGIN/END
// markers, err toward including too much, summarize the tree instead of
// dumping it, and cut out anything loaded by mistake.
const reportSpecA = `
# Report: a curated digest
After loading, write your report between these exact markers, on their own lines:

BEGIN EXPLORATION
END OF EXPLORATION

Inside the markers, write a prose digest for the answering model:
- State the few facts the answer turns on, each with its file:line citation.
- Quote VERBATIM only the handful of code spans the answer genuinely hinges on
  (fence them and label with path:line). Everything else, describe in prose.
- Summarize the repo layout in a sentence or two — do NOT paste the whole tree.
- Omit files and searches that turned out irrelevant. If you opened something and
  it didn't matter, leave it out (one line noting you checked is fine).
- When uncertain whether a detail matters, INCLUDE it. Erring slightly long is
  far cheaper than making the answering model miss the thing that mattered.

Write nothing outside the markers.`

const reportSpecB = `
# Report: a reading guide with selective quotes
After loading, write your report between these exact markers, on their own lines:

BEGIN EXPLORATION
END OF EXPLORATION

Inside the markers, write a reading guide for the answering model:
- For the spans the answer hinges on, QUOTE them verbatim (fenced, labeled path:line).
- For everything else relevant, give a pointer: path:line-range plus a one-line
  description of what's there and why it might matter. Prefer listing one pointer
  too many over omitting one that could matter.
- Condense the repo tree to just the subtree that's relevant, as an indented list.
- Drop files/searches that turned out irrelevant; a single line listing what you
  checked and ruled out is welcome (it tells the answering model the negative space).

Write nothing outside the markers.`

const reportSpecC = `
# Report: a cleaned context pack
After loading, write your report between these exact markers, on their own lines:

BEGIN EXPLORATION
END OF EXPLORATION

Inside the markers, assemble a clean context pack for the answering model:
- Reproduce VERBATIM the contents of each file (or the relevant span of each large
  file) that the answer depends on, each under a "## path:line-range" heading in a
  fenced block. This is the primary material — be generous; include a span if you
  think it might matter.
- Replace the full repo tree with a condensed listing of only the relevant paths.
- Exclude any file you opened that turned out not to bear on the answer, and any
  search that returned nothing useful. The pack should contain signal, not your
  whole browsing trail.

Write nothing outside the markers.`

func TestExperiment_ExplorerReport(t *testing.T) {
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		t.Skip("DEEPSEEK_API_KEY not set; skipping live explorer-report experiment")
	}

	// Dogfood: explore the dpal repo itself. From internal/server the
	// repo root is two levels up.
	exp, err := explorer.New("../..")
	if err != nil {
		t.Fatalf("explorer.New(repo root): %v", err)
	}

	s := New(deepseek.NewClient(key)).WithExplorer(exp)

	variants := []struct {
		name string
		spec string
	}{
		{"A_digest", reportSpecA},
		{"B_reading_guide", reportSpecB},
		{"C_cleaned_pack", reportSpecC},
	}

	questions := []struct {
		name string
		text string
	}{
		{"narrow", "How does stripExplorerSynthesis decide which messages to drop before the synthesizer runs?"},
		{"broad", "How does the two-phase consult_deepseek flow work end to end — explorer, synthesizer, and what's passed between them?"},
	}

	for _, q := range questions {
		for _, v := range variants {
			t.Run(q.name+"/"+v.name, func(t *testing.T) {
				sysPrompt := curatorBase + v.spec
				msgs := []deepseek.ChatCompletionMessage{
					{Role: deepseek.ChatMessageRoleUser, Content: q.text},
				}

				// flash, thinking on, no forced tool choice — same as the
				// production explore phase, just kept standalone.
				resp, history, err := s.chat(context.Background(), ModelV4Flash, sysPrompt, exp, msgs, true, "", "")
				if err != nil {
					t.Fatalf("explore loop: %v", err)
				}

				report := resp.Choices[0].Message.Content
				toolCalls, rawBytes := transcriptStats(history)

				t.Logf("\n=== %s / %s ===", q.name, v.name)
				t.Logf("tool calls: %d", toolCalls)
				t.Logf("raw transcript bytes (what pro pays today): %d", rawBytes)
				t.Logf("report bytes: %d", len(report))
				if rawBytes > 0 {
					t.Logf("compression: report is %.0f%% of raw transcript", 100*float64(len(report))/float64(rawBytes))
				}
				if !strings.Contains(report, "BEGIN EXPLORATION") || !strings.Contains(report, "END OF EXPLORATION") {
					t.Logf("WARNING: report missing BEGIN/END markers — variant may not be honoring the framing")
				}
				t.Logf("--- report ---\n%s\n--- end report ---", report)
			})
		}
	}
}

// transcriptStats walks the explore history and returns the number of
// tool calls flash issued and the total bytes of tool RESULTS (the file
// contents and tree dumps) — i.e. the raw context that goes to the
// synthesizer under today's hand-off, the thing the report is meant to
// replace.
func transcriptStats(history []deepseek.ChatCompletionMessage) (toolCalls int, rawBytes int) {
	for _, m := range history {
		if m.Role == deepseek.ChatMessageRoleAssistant {
			toolCalls += len(m.ToolCalls)
		}
		if m.Role == deepseek.ChatMessageRoleTool {
			rawBytes += len(m.Content)
		}
	}
	return toolCalls, rawBytes
}
