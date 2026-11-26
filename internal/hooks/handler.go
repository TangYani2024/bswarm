package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/TangYani2024/bswarm/internal/store"
)

func isWriteTool(name string) bool {
	switch name {
	case "edit_file", "write_file", "write_to_file", "replace_file_content", "multi_replace_file_content":
		return true
	default:
		return false
	}
}

type ToolCallPayload struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type HookInput struct {
	ConversationID        string           `json:"conversationId"`
	WorkspacePaths        []string         `json:"workspacePaths"`
	TranscriptPath        string           `json:"transcriptPath"`
	ArtifactDirectoryPath string           `json:"artifactDirectoryPath"`
	ModelName             string           `json:"modelName"`
	HookEventName         string           `json:"hookEventName"`
	CWD                   string           `json:"cwd"`
	StepIdx               int              `json:"stepIdx"`
	ToolCall              *ToolCallPayload `json:"toolCall,omitempty"`
	Error                 string           `json:"error,omitempty"`
	TerminationReason     string           `json:"terminationReason,omitempty"`
	ContinuationActive    bool             `json:"continuationActive,omitempty"`
}

type PreToolOutput struct {
	Decision  string         `json:"decision,omitempty"`
	Reason    string         `json:"reason,omitempty"`
	Overwrite map[string]any `json:"overwrite,omitempty"`
}

type InjectSystemMessage struct {
	SystemMessage string         `json:"systemMessage"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

type InjectedStep struct {
	SystemMessage *InjectSystemMessage `json:"systemMessage,omitempty"`
}

type PreInvocationOutput struct {
	InjectSteps         []InjectedStep `json:"injectSteps,omitempty"`
	TerminationBehavior string         `json:"terminationBehavior,omitempty"`
}

type StopOutput struct {
	Decision string `json:"decision,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type Handler struct {
	Store             *store.Store
	GuardNative       bool
	NativePostMaxByte int
}

func NewHandler(st *store.Store) *Handler {
	if st == nil {
		st = store.New("")
	}
	guardNative := strings.TrimSpace(os.Getenv("BSWARM_GUARD_NATIVE")) != "0"
	maxBytes := 4096
	if v := strings.TrimSpace(os.Getenv("BSWARM_NATIVE_POST_MAX_BYTES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 512 {
			maxBytes = n
		}
	}
	return &Handler{
		Store:             st,
		GuardNative:       guardNative,
		NativePostMaxByte: maxBytes,
	}
}

func cliBinName() string {
	if v := strings.TrimSpace(os.Getenv("BSWARM_BIN")); v != "" {
		return filepath.Base(v)
	}
	return "bswarm"
}

func (h *Handler) convIDOf(in *HookInput) string {
	if in != nil && strings.TrimSpace(in.ConversationID) != "" {
		return strings.TrimSpace(in.ConversationID)
	}
	return strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
}

func (h *Handler) resolveActor(in *HookInput) *store.ConvRef {
	if convID := h.convIDOf(in); convID != "" {
		if ref, err := h.Store.ResolveConversation(convID); err == nil && ref != nil {
			return ref
		}
	}
	envTree := strings.TrimSpace(os.Getenv("BSWARM_TREE_ID"))
	envNode := strings.TrimSpace(os.Getenv("BSWARM_NODE_ID"))
	if envTree != "" && envNode != "" {
		return &store.ConvRef{TreeID: envTree, NodeID: envNode}
	}
	return nil
}

// HandlePreToolUse enforces file ownership boundaries for bswarm nodes and optional anti-bloat
// / anti-deadlock guards for native /swarm post_message calls.
func (h *Handler) HandlePreToolUse(in *HookInput) any {
	allow := &PreToolOutput{Decision: "allow"}
	if in == nil || in.ToolCall == nil {
		return allow
	}
	name := in.ToolCall.Name
	convID := h.convIDOf(in)

	// 1. Native /swarm guardrails (lightweight, toggleable via BSWARM_GUARD_NATIVE=0).
	if h.GuardNative && name == "post_message" {
		if out := h.guardNativePostMessage(in, convID); out != nil {
			return out
		}
	}

	// 2. bswarm active node guardrails.
	ref := h.resolveActor(in)
	if ref == nil {
		return allow
	}
	t, err := h.Store.LoadTree(ref.TreeID)
	if err != nil {
		return allow
	}
	self, ok := t.Nodes[ref.NodeID]
	if !ok || !self.IsActive() {
		return allow
	}

	if isWriteTool(name) {
		targetPath := extractTargetPath(in.ToolCall.Args)
		if targetPath != "" {
			if deny := h.checkWriteBoundary(t, self, targetPath, in.CWD); deny != nil {
				return deny
			}
		}
	}
	return allow
}

func (h *Handler) checkWriteBoundary(t *store.Tree, self *store.Node, targetPath, cwd string) *PreToolOutput {
	absTarget := targetPath
	if !filepath.IsAbs(absTarget) {
		base := cwd
		if base == "" {
			base = self.WorkspacePath
		}
		absTarget = filepath.Join(base, absTarget)
	}
	absTarget = resolveRealPath(absTarget)
	repoDir := resolveRealPath(t.RepoRoot)
	wsDir := resolveRealPath(self.WorkspacePath)

	// Always allow writing inside this node's own artifacts dir.
	artDir := resolveRealPath(h.Store.ArtifactsDir(t.ID, self.ID))
	if isUnderDir(absTarget, artDir) {
		return nil
	}
	// Allow system temp files only when they lie outside both the repo root and the node workspace.
	if isUnderDir(absTarget, resolveRealPath(os.TempDir())) &&
		!isUnderDir(absTarget, repoDir) &&
		!isUnderDir(absTarget, wsDir) {
		return nil
	}

	// Worktree node must edit ONLY inside its own worktree (never main repo or sibling worktrees)
	// and must also respect its explicit Owns whitelist if set.
	if self.WorkspaceMode == store.WorkspaceWorktree && wsDir != "" {
		if !isUnderDir(absTarget, wsDir) {
			return &PreToolOutput{
				Decision: "deny",
				Reason: fmt.Sprintf("[bswarm] node %q runs in isolated worktree %s; do not write outside it (%s). Tip: writing inside your artifacts dir (%s) is always allowed.",
					self.ID, wsDir, targetPath, artDir),
			}
		}
		if len(self.Owns) > 0 {
			relWT := store.NormalizeRelPath(wsDir, absTarget)
			if relWT != "" && !strings.HasPrefix(relWT, "..") && !store.MatchesAnyOwn(self.Owns, relWT) && !store.MatchesAnyOwn(self.Owns, filepath.ToSlash(absTarget)) {
				return &PreToolOutput{
					Decision: "deny",
					Reason: fmt.Sprintf("[bswarm] file %q is outside node %q's owned scope (%s). Tip: writing inside your artifacts dir (%s) is always allowed.",
						relWT, self.ID, strings.Join(self.Owns, ", "), artDir),
				}
			}
		}
		return nil
	}

	// Shared workspace ownership lock.
	// Allow explicit absolute --owns globs targeting paths outside repoDir.
	if len(self.Owns) > 0 && store.MatchesAnyOwn(self.Owns, filepath.ToSlash(absTarget)) {
		return nil
	}
	rel := store.NormalizeRelPath(repoDir, absTarget)
	if rel == "" || strings.HasPrefix(rel, "..") {
		return nil
	}

	// Check if another active shared node owns this path.
	for _, other := range t.Nodes {
		if other.ID == self.ID || !other.IsActive() || other.WorkspaceMode != store.WorkspaceShared || len(other.Owns) == 0 {
			continue
		}
		// Parent/root is allowed to touch files owned by idle descendants only when descendants are not actively working.
		if other.ParentID == self.ID && other.State == store.NodeIdle {
			continue
		}
		if store.MatchesAnyOwn(other.Owns, rel) {
			return &PreToolOutput{
				Decision: "deny",
				Reason: fmt.Sprintf("[bswarm] file %q is exclusively owned by active node %q (%s); coordinate via parent or use --worktree. Tip: writing inside your artifacts dir (%s) is always allowed.",
					rel, other.ID, strings.Join(other.Owns, ", "), artDir),
			}
		}
	}

	// If self has explicit Owns whitelist, stay inside it.
	if len(self.Owns) > 0 && !store.MatchesAnyOwn(self.Owns, rel) {
		return &PreToolOutput{
			Decision: "deny",
			Reason: fmt.Sprintf("[bswarm] file %q is outside node %q's owned scope (%s; repo=%s). Tip: writing inside your artifacts dir (%s) is always allowed.",
				rel, self.ID, strings.Join(self.Owns, ", "), repoDir, artDir),
		}
	}
	return nil
}

var (
	doneTagRe   = regexp.MustCompile(`(?i)\[done\]`)
	atMentionRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])@(?:#?[a-zA-Z0-9][a-zA-Z0-9_-]*)`)
)

func (h *Handler) guardNativePostMessage(in *HookInput, convID string) *PreToolOutput {
	rawText, _ := in.ToolCall.Args["text"].(string)
	if strings.TrimSpace(rawText) == "" {
		return nil
	}
	// 1. Block giant >4KB posts that bloat every member's context.
	if len(rawText) > h.NativePostMaxByte {
		return &PreToolOutput{
			Decision: "deny",
			Reason: fmt.Sprintf("[bswarm native-guard] post_message text is %d bytes (limit %d B). Write the full report/diff to a file and post a concise summary with the file path instead.",
				len(rawText), h.NativePostMaxByte),
		}
	}
	// 2. Block worker [done] posts missing any @mention (common deadlock cause).
	// Allow coordinator's unaddressed [done] (used to put native monitor to sleep).
	if doneTagRe.MatchString(rawText) && !atMentionRe.MatchString(rawText) {
		if !isNativeCoordinator(in.ArtifactDirectoryPath, convID) {
			return &PreToolOutput{
				Decision: "deny",
				Reason:   "[bswarm native-guard] your [done] post has no @mention (e.g. @coordinator or @verifier), which stalls the swarm. Add an explicit @target and repost.",
			}
		}
	}
	return nil
}

// isNativeCoordinator checks whether artifactDir belongs to the coordinator of a native swarm.
func isNativeCoordinator(artifactDir, convID string) bool {
	if artifactDir == "" {
		return false
	}
	membersFile := filepath.Join(artifactDir, ".system_generated", "swarm", "members.json")
	raw, err := os.ReadFile(membersFile)
	if err != nil {
		return false
	}
	var doc struct {
		Members map[string]struct {
			ConversationID string `json:"conversation_id"`
			Role           string `json:"role"`
		} `json:"members"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false
	}
	for _, m := range doc.Members {
		if m.ConversationID == convID && m.Role == "coordinator" {
			return true
		}
	}
	return false
}

// HandlePostToolUse records edited workspace file paths on active bswarm nodes (always returns {}).
func (h *Handler) HandlePostToolUse(in *HookInput) any {
	if in == nil || in.ToolCall == nil || in.Error != "" {
		return map[string]any{}
	}
	if !isWriteTool(in.ToolCall.Name) {
		return map[string]any{}
	}
	ref := h.resolveActor(in)
	if ref == nil {
		return map[string]any{}
	}
	targetPath := extractTargetPath(in.ToolCall.Args)
	if targetPath == "" {
		return map[string]any{}
	}
	_, _ = h.Store.UpdateTree(ref.TreeID, func(t *store.Tree) error {
		n, ok := t.Nodes[ref.NodeID]
		if !ok || !n.IsActive() {
			return nil
		}
		absTarget := targetPath
		if !filepath.IsAbs(absTarget) {
			base := in.CWD
			if base == "" {
				base = n.WorkspacePath
			}
			absTarget = filepath.Join(base, absTarget)
		}
		absTarget = resolveRealPath(absTarget)
		wsDir := resolveRealPath(n.WorkspacePath)
		artDir := resolveRealPath(h.Store.ArtifactsDir(t.ID, n.ID))
		if isUnderDir(absTarget, artDir) || !isUnderDir(absTarget, wsDir) {
			return nil
		}
		rel := store.NormalizeRelPath(wsDir, absTarget)
		if rel == "" || strings.HasPrefix(rel, "..") {
			return nil
		}
		for _, existing := range n.TouchedFiles {
			if existing == rel {
				n.UpdatedAt = time.Now().UTC()
				return nil
			}
		}
		if len(n.TouchedFiles) < 64 {
			n.TouchedFiles = append(n.TouchedFiles, rel)
		}
		n.UpdatedAt = time.Now().UTC()
		return nil
	})
	return map[string]any{}
}

// HandlePreInvocation injects a tiny (<=3 line) reminder ONLY for active bswarm nodes that
// have unread private mail. Non-swarm sessions get {} (0 bytes injected).
func (h *Handler) HandlePreInvocation(in *HookInput) any {
	ref := h.resolveActor(in)
	if ref == nil {
		return map[string]any{}
	}
	t, err := h.Store.LoadTree(ref.TreeID)
	if err != nil {
		return map[string]any{}
	}
	self, ok := t.Nodes[ref.NodeID]
	if !ok || !self.IsActive() {
		return map[string]any{}
	}
	unread := self.InboxTotalSeq - self.InboxReadSeq
	if unread <= 0 {
		return map[string]any{}
	}
	msg := fmt.Sprintf("[%s] node=%s (tree=%s) | %d unread private message(s) — run `%s inbox`.",
		cliBinName(), self.ID, t.ID, unread, cliBinName())
	if len(self.Owns) > 0 {
		msg += fmt.Sprintf(" | owns=%s", strings.Join(self.Owns, ","))
	}
	return PreInvocationOutput{
		InjectSteps: []InjectedStep{
			{SystemMessage: &InjectSystemMessage{SystemMessage: msg}},
		},
	}
}

// HandleStop prevents an active non-root child node from silently stopping without calling
// `bswarm done` or `bswarm ask parent`. Respects continuationActive to guarantee at most 1 nudge.
func (h *Handler) HandleStop(in *HookInput) any {
	stop := StopOutput{Decision: "stop"}
	if in == nil || in.ContinuationActive {
		return stop
	}
	if in.TerminationReason != "" && in.TerminationReason != "NO_TOOL_CALL" {
		return stop
	}
	ref := h.resolveActor(in)
	if ref == nil {
		return stop
	}
	t, err := h.Store.LoadTree(ref.TreeID)
	if err != nil {
		return stop
	}
	self, ok := t.Nodes[ref.NodeID]
	if !ok || self.IsRoot() || self.State != store.NodeWorking || self.CurrentTaskID == "" {
		return stop
	}
	// Allow intermediate leads (depth >= 1) to enter 0-token sleep while waiting for active children!
	if store.HasActiveChildren(t, self.ID) {
		return stop
	}
	bin := cliBinName()
	return StopOutput{
		Decision: "continue",
		Reason: fmt.Sprintf("[%s] Task %s is still open on node %q. Write detailed output to your artifacts dir and run `%s done --summary \"...\" [--artifact <path>]` (or `%s ask parent --summary \"...\"` if blocked) before ending your turn.",
			bin, self.CurrentTaskID, self.ID, bin, bin),
	}
}

func extractTargetPath(args map[string]any) string {
	if args == nil {
		return ""
	}
	for _, k := range []string{"Path", "AbsolutePath", "TargetFile", "path"} {
		if v, ok := args[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func resolveRealPath(p string) string {
	if p == "" {
		return ""
	}
	cleaned := filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(cleaned); err == nil {
		return filepath.Clean(r)
	}
	// Walk up arbitrary non-existent ancestor directories to resolve the deepest existing symlink ancestor.
	cur := cleaned
	var tail []string
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return cleaned
		}
		tail = append(tail, filepath.Base(cur))
		if r, err := filepath.EvalSymlinks(parent); err == nil {
			parts := make([]string, 0, len(tail)+1)
			parts = append(parts, r)
			for i := len(tail) - 1; i >= 0; i-- {
				parts = append(parts, tail[i])
			}
			return filepath.Clean(filepath.Join(parts...))
		}
		cur = parent
	}
}

func isUnderDir(target, dir string) bool {
	if dir == "" || target == "" {
		return false
	}
	tPath := target
	dPath := dir
	if runtime.GOOS == "windows" {
		tPath = strings.ToLower(tPath)
		dPath = strings.ToLower(dPath)
	}
	rel, err := filepath.Rel(dPath, tPath)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != "..")
}
