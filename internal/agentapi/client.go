package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type NewConversationOpts struct {
	Model   string
	Title   string
	Profile string
	Prompt  string
}

type Metadata struct {
	ConversationID string
	Model          string
	WorkspacePaths []string
	Raw            map[string]any
}

// Transport abstracts official `agy agentapi` invocations so orchestrator/watchdog stay 100% testable.
type Transport interface {
	NewConversation(ctx context.Context, opts NewConversationOpts) (string, error)
	SendMessage(ctx context.Context, recipientID, title, content string) error
	GetMetadata(ctx context.Context, convID string) (*Metadata, error)
}

// CLITransport invokes the bundled `agy agentapi` / `agentapi` binary using inherited env credentials.
type CLITransport struct {
	ExePath string
	Timeout time.Duration
}

func NewCLITransport() *CLITransport {
	return &CLITransport{
		ExePath: strings.TrimSpace(os.Getenv("ANTIGRAVITY_AGENTAPI_EXE")),
		Timeout: 30 * time.Second,
	}
}

func (c *CLITransport) Available() bool {
	return strings.TrimSpace(os.Getenv("ANTIGRAVITY_LS_ADDRESS")) != "" &&
		strings.TrimSpace(os.Getenv("ANTIGRAVITY_CSRF_TOKEN")) != ""
}

func (c *CLITransport) run(ctx context.Context, subcmd string, args ...string) ([]byte, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	var bin string
	var cmdArgs []string
	if c.ExePath != "" {
		bin = c.ExePath
		cmdArgs = append([]string{"agentapi", subcmd}, args...)
	} else if p, err := exec.LookPath("agentapi"); err == nil {
		bin = p
		cmdArgs = append([]string{subcmd}, args...)
	} else if p, err := exec.LookPath("agy"); err == nil {
		bin = p
		cmdArgs = append([]string{"agentapi", subcmd}, args...)
	} else {
		bin = "agentapi"
		cmdArgs = append([]string{subcmd}, args...)
	}
	cmd := exec.CommandContext(ctx, bin, cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := bytes.TrimSpace(stdout.Bytes())
	if err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" && len(out) > 0 {
			errMsg = string(out)
		}
		if errMsg == "" {
			errMsg = err.Error()
		}
		if strings.Contains(strings.ToLower(errMsg), "connection refused") {
			return out, fmt.Errorf("agentapi %s failed: %s (hint: run_command is inside a network-isolated sandbox and cannot reach ANTIGRAVITY_LS_ADDRESS=%s; enable sandboxAllowNetwork or disable terminal sandbox / pass BypassSandbox)",
				subcmd, errMsg, os.Getenv("ANTIGRAVITY_LS_ADDRESS"))
		}
		return out, fmt.Errorf("agentapi %s failed: %s", subcmd, errMsg)
	}
	if apiErr := extractError(out); apiErr != "" {
		if strings.Contains(strings.ToLower(apiErr), "connection refused") {
			return out, fmt.Errorf("agentapi %s error: %s (hint: run_command is inside a network-isolated sandbox and cannot reach ANTIGRAVITY_LS_ADDRESS=%s; enable sandboxAllowNetwork or disable sandbox / pass BypassSandbox)",
				subcmd, apiErr, os.Getenv("ANTIGRAVITY_LS_ADDRESS"))
		}
		return out, fmt.Errorf("agentapi %s error: %s", subcmd, apiErr)
	}
	return out, nil
}

func (c *CLITransport) NewConversation(ctx context.Context, opts NewConversationOpts) (string, error) {
	if strings.TrimSpace(opts.Prompt) == "" {
		return "", errors.New("prompt is required for new-conversation")
	}
	var args []string
	if opts.Model != "" {
		args = append(args, "--model="+opts.Model)
	}
	if opts.Title != "" {
		args = append(args, "--title="+opts.Title)
	}
	if opts.Profile != "" {
		args = append(args, "--profile="+opts.Profile)
	}
	args = append(args, "--", opts.Prompt)
	out, err := c.run(ctx, "new-conversation", args...)
	if err != nil {
		return "", err
	}
	id := ParseConversationID(out)
	if id == "" {
		return "", fmt.Errorf("agentapi new-conversation returned no conversation_id: %s", string(out))
	}
	return id, nil
}

func (c *CLITransport) SendMessage(ctx context.Context, recipientID, title, content string) error {
	if strings.TrimSpace(recipientID) == "" {
		return errors.New("recipient_id is required")
	}
	var args []string
	if title != "" {
		args = append(args, "--title="+title)
	}
	args = append(args, "--", recipientID, content)
	_, err := c.run(ctx, "send-message", args...)
	return err
}

func (c *CLITransport) GetMetadata(ctx context.Context, convID string) (*Metadata, error) {
	if strings.TrimSpace(convID) == "" {
		return nil, errors.New("conversation_id is required")
	}
	out, err := c.run(ctx, "get-conversation-metadata", convID)
	if err != nil {
		return nil, err
	}
	return ParseMetadata(convID, out)
}

// ParseConversationID recursively extracts conversationId / conversation_id / cascadeId from JSON.
func ParseConversationID(raw []byte) string {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Fallback: single line UUID token
		s := strings.TrimSpace(string(raw))
		if len(s) >= 8 && !strings.ContainsAny(s, " \t\n{}") {
			return s
		}
		return ""
	}
	if id := findStringKey(doc, "conversation_id", "conversationId", "cascadeId", "cascade_id"); id != "" {
		return id
	}
	return findStringKey(doc, "id")
}

// ParseMetadata extracts useful fields from get-conversation-metadata output.
func ParseMetadata(convID string, raw []byte) (*Metadata, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse metadata json: %w", err)
	}
	md := &Metadata{
		ConversationID: convID,
		Model:          findStringKey(m, "model", "modelName", "model_name"),
		Raw:            m,
	}
	if uris := findStringSliceKey(m, "workspaceUris", "workspace_uris"); len(uris) > 0 {
		for _, u := range uris {
			md.WorkspacePaths = append(md.WorkspacePaths, strings.TrimPrefix(u, "file://"))
		}
	}
	return md, nil
}

func extractError(raw []byte) string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	if v, ok := m["error"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := m["error"].(map[string]any); ok {
		if msg, ok := v["message"].(string); ok && strings.TrimSpace(msg) != "" {
			return strings.TrimSpace(msg)
		}
	}
	return ""
}

func findStringKey(v any, keys ...string) string {
	switch cur := v.(type) {
	case map[string]any:
		for _, k := range keys {
			if raw, ok := cur[k]; ok {
				if s, ok := raw.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		for _, child := range cur {
			if found := findStringKey(child, keys...); found != "" {
				return found
			}
		}
	case []any:
		for _, item := range cur {
			if found := findStringKey(item, keys...); found != "" {
				return found
			}
		}
	}
	return ""
}

func findStringSliceKey(v any, keys ...string) []string {
	switch cur := v.(type) {
	case map[string]any:
		for _, k := range keys {
			if raw, ok := cur[k]; ok {
				if arr, ok := raw.([]any); ok {
					var out []string
					for _, x := range arr {
						if s, ok := x.(string); ok && s != "" {
							out = append(out, s)
						}
					}
					if len(out) > 0 {
						return out
					}
				}
			}
		}
		for _, child := range cur {
			if found := findStringSliceKey(child, keys...); len(found) > 0 {
				return found
			}
		}
	case []any:
		for _, item := range cur {
			if found := findStringSliceKey(item, keys...); len(found) > 0 {
				return found
			}
		}
	}
	return nil
}
