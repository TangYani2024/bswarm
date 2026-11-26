package agentapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type NewConversationOpts struct {
	Model                string
	Title                string
	Profile              string
	Prompt               string
	ParentConversationID string
	WorkspacePath        string
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

func isModelTierAlias(m string) bool {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "flash", "flash_lite", "pro", "high", "low":
		return true
	default:
		return false
	}
}

// PathToFileURI converts a local filesystem path (Unix, Windows drive, or UNC) into a file:// URI.
func PathToFileURI(p string) string {
	s := strings.TrimSpace(p)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(s), "file://") {
		return s
	}
	// Windows UNC path: \\host\share\dir -> file://host/share/dir
	if strings.HasPrefix(s, `\\`) {
		rest := strings.TrimPrefix(strings.ReplaceAll(s, `\`, "/"), "//")
		parts := strings.Split(rest, "/")
		for i := 1; i < len(parts); i++ {
			parts[i] = url.PathEscape(parts[i])
		}
		return "file://" + strings.Join(parts, "/")
	}
	// Windows drive path: C:\foo\bar or C:/foo/bar -> file:///C:/foo/bar
	if len(s) >= 2 && s[1] == ':' {
		slash := strings.ReplaceAll(s, `\`, "/")
		parts := strings.Split(slash, "/")
		for i := 1; i < len(parts); i++ {
			parts[i] = url.PathEscape(parts[i])
		}
		return "file:///" + strings.Join(parts, "/")
	}
	slash := filepath.ToSlash(s)
	if !strings.HasPrefix(slash, "/") {
		if abs, err := filepath.Abs(s); err == nil {
			slash = filepath.ToSlash(abs)
		}
	}
	parts := strings.Split(slash, "/")
	for i := range parts {
		if parts[i] != "" {
			parts[i] = url.PathEscape(parts[i])
		}
	}
	return "file://" + strings.Join(parts, "/")
}

func (c *CLITransport) NewConversation(ctx context.Context, opts NewConversationOpts) (string, error) {
	if strings.TrimSpace(opts.Prompt) == "" {
		return "", errors.New("prompt is required for new-conversation")
	}

	// Prefer native ConnectRPC StartCascade(source=SUBAGENT, parentConversationId=...) +
	// SendUserCascadeMessage so child conversations are attached under their parent tree
	// instead of cluttering the Antigravity Desktop top-level sidebar list.
	// Falls back transparently to `agentapi new-conversation` for tier aliases or older LS builds.
	parentConv := strings.TrimSpace(opts.ParentConversationID)
	if parentConv == "" {
		parentConv = strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
	}
	if parentConv != "" && c.Available() && strings.TrimSpace(os.Getenv("BSWARM_USE_AGENTAPI_NEW_CONV")) != "1" && !isModelTierAlias(opts.Model) {
		if id, err := c.startSubagentCascade(ctx, parentConv, opts); err == nil && id != "" {
			return id, nil
		}
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

func (c *CLITransport) startSubagentCascade(ctx context.Context, parentConv string, opts NewConversationOpts) (string, error) {
	lsAddr := strings.TrimSpace(os.Getenv("ANTIGRAVITY_LS_ADDRESS"))
	csrf := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CSRF_TOKEN"))
	if lsAddr == "" || csrf == "" {
		return "", errors.New("ANTIGRAVITY_LS_ADDRESS or ANTIGRAVITY_CSRF_TOKEN unset")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	startReq := map[string]any{
		"source":               "CORTEX_TRAJECTORY_SOURCE_SUBAGENT",
		"parentConversationId": parentConv,
	}
	if uri := PathToFileURI(opts.WorkspacePath); uri != "" {
		startReq["workspaceUris"] = []string{uri}
	}
	if strings.TrimSpace(opts.Title) != "" {
		startReq["title"] = strings.TrimSpace(opts.Title)
	}
	if strings.TrimSpace(opts.Profile) != "" {
		startReq["activeProfile"] = strings.TrimSpace(opts.Profile)
	}

	rawStart, scheme, err := callLocalLSConnectRPC(callCtx, lsAddr, csrf, "StartCascade", startReq)
	if err != nil {
		return "", err
	}
	childID := ParseConversationID(rawStart)
	if childID == "" {
		return "", fmt.Errorf("StartCascade returned no cascadeId: %s", string(rawStart))
	}

	msgReq := map[string]any{
		"cascadeId": childID,
		"items":     []map[string]any{{"text": opts.Prompt}},
	}
	if strings.TrimSpace(opts.Model) != "" {
		msgReq["cascadeConfig"] = map[string]any{
			"plannerConfig": map[string]any{
				"modelName": strings.TrimSpace(opts.Model),
			},
		}
	}
	if strings.TrimSpace(opts.Profile) != "" {
		msgReq["activeProfile"] = strings.TrimSpace(opts.Profile)
	}
	if _, _, err := callLocalLSConnectRPCWithScheme(callCtx, scheme, lsAddr, csrf, "SendUserCascadeMessage", msgReq); err != nil {
		return "", err
	}
	return childID, nil
}

func callLocalLSConnectRPC(ctx context.Context, lsAddr, csrf, method string, payload any) ([]byte, string, error) {
	if strings.HasPrefix(lsAddr, "http://") {
		b, _, err := callLocalLSConnectRPCWithScheme(ctx, "http", strings.TrimPrefix(lsAddr, "http://"), csrf, method, payload)
		return b, "http", err
	}
	if strings.HasPrefix(lsAddr, "https://") {
		b, _, err := callLocalLSConnectRPCWithScheme(ctx, "https", strings.TrimPrefix(lsAddr, "https://"), csrf, method, payload)
		return b, "https", err
	}
	b, status, err := callLocalLSConnectRPCWithScheme(ctx, "http", lsAddr, csrf, method, payload)
	if err == nil {
		return b, "http", nil
	}
	// Retry over loopback HTTPS when ANTIGRAVITY_LS_ADDRESS points to an HTTPS port.
	if status == 400 || strings.Contains(strings.ToLower(err.Error()), "https") || strings.Contains(strings.ToLower(err.Error()), "eof") {
		if b2, _, err2 := callLocalLSConnectRPCWithScheme(ctx, "https", lsAddr, csrf, method, payload); err2 == nil {
			return b2, "https", nil
		}
	}
	return b, "http", err
}

func callLocalLSConnectRPCWithScheme(ctx context.Context, scheme, hostPort, csrf, method string, payload any) ([]byte, int, error) {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	endpoint := fmt.Sprintf("%s://%s/exa.language_server_pb.LanguageServerService/%s", scheme, hostPort, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-codeium-csrf-token", csrf)
	req.Header.Set("Connect-Protocol-Version", "1")

	client := &http.Client{Timeout: 15 * time.Second}
	if scheme == "https" {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode != http.StatusOK {
		return raw, resp.StatusCode, fmt.Errorf("LS %s HTTP %d: %s", method, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, resp.StatusCode, nil
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
