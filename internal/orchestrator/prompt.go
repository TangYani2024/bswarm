package orchestrator

import (
	"fmt"
	"os"
	"strings"

	"github.com/TangYani2024/bswarm/internal/store"
)

func cliCommandName() string {
	if v := strings.TrimSpace(os.Getenv("BSWARM_BIN")); v != "" {
		return v
	}
	return "bswarm"
}

// BuildSpawnPrompt constructs an ultra-lean startup brief (<= 20 lines) for a new tree node.
func BuildSpawnPrompt(t *store.Tree, n *store.Node, tk *store.Task, artifactsDir string) string {
	bin := cliCommandName()
	var b strings.Builder
	fmt.Fprintf(&b, "[bswarm task %s] You are node %q (role=%s, parent=%s, depth=%d/%d).\n",
		tk.ID, n.ID, n.Role, n.ParentID, n.Depth, t.MaxDepth)
	fmt.Fprintf(&b, "Workspace (%s): %s\n", n.WorkspaceMode, n.WorkspacePath)
	if n.Branch != "" {
		fmt.Fprintf(&b, "Git branch: %s (commit all edits on this branch inside the workspace path above).\n", n.Branch)
	}
	if len(n.Owns) > 0 {
		fmt.Fprintf(&b, "Owned paths (hard-enforced): %s\n", strings.Join(n.Owns, ", "))
	}
	fmt.Fprintf(&b, "Artifacts dir: %s\n", artifactsDir)
	fmt.Fprintf(&b, "\nGoal: %s\n", tk.Goal)
	if tk.SpecFile != "" {
		fmt.Fprintf(&b, "Full spec file: %s (read on demand)\n", tk.SpecFile)
	}
	if tk.VerifyCmd != "" {
		fmt.Fprintf(&b, "Required verify command: %s\n", tk.VerifyCmd)
	}
	b.WriteString("\nRules:\n")
	b.WriteString("1. Pass-by-reference: write long reports/logs/diffs to a file in your Artifacts dir; keep messages <= 400 chars.\n")
	if n.Depth < t.MaxDepth {
		fmt.Fprintf(&b, "2. You may delegate sub-pieces via `%s spawn` (your children report only to you; integrate before reporting up).\n", bin)
	} else {
		b.WriteString("2. You are at max depth; execute directly without spawning further subagents.\n")
	}
	fmt.Fprintf(&b, "3. When finished, run `%s done --summary \"<concise result>\" [--artifact <path>]`, then end your turn (0-token idle for reuse).\n", bin)
	fmt.Fprintf(&b, "4. If blocked on a decision from your parent, run `%s ask parent --summary \"<question>\"` and end your turn.\n", bin)
	return b.String()
}

// BuildAssignNotice builds a compact wake message for reusing an idle node with a new task.
func BuildAssignNotice(n *store.Node, tk *store.Task) string {
	bin := cliCommandName()
	var b strings.Builder
	fmt.Fprintf(&b, "[bswarm assign %s] New task for %q:\nGoal: %s\n", tk.ID, n.ID, tk.Goal)
	if tk.SpecFile != "" {
		fmt.Fprintf(&b, "Full spec: %s\n", tk.SpecFile)
	}
	if tk.VerifyCmd != "" {
		fmt.Fprintf(&b, "Verify cmd: %s\n", tk.VerifyCmd)
	}
	fmt.Fprintf(&b, "Run `%s done --summary \"...\" [--artifact <path>]` when finished, then end your turn.", bin)
	return b.String()
}

// BuildMailWakeNotice builds a minimal <= 5 line doorbell message delivered via agentapi send-message.
func BuildMailWakeNotice(m store.Mail) string {
	bin := cliCommandName()
	var b strings.Builder
	fmt.Fprintf(&b, "[bswarm %s #%d] from %q", m.Kind, m.Seq, m.From)
	if m.TaskID != "" {
		fmt.Fprintf(&b, " (task %s)", m.TaskID)
	}
	fmt.Fprintf(&b, ":\n%s\n", m.Summary)
	if m.Artifact != "" {
		fmt.Fprintf(&b, "Artifact: %s\n", m.Artifact)
	}
	fmt.Fprintf(&b, "Inspect with `%s status` or `%s inbox`.", bin, bin)
	return b.String()
}
