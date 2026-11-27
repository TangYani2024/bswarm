package gateway

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/TangYani2024/bswarm/internal/store"
)

type mirrorToolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type mirrorTranscriptStep struct {
	StepIndex int              `json:"step_index"`
	Type      string           `json:"type"`
	Status    string           `json:"status"`
	Content   *string          `json:"content,omitempty"`
	Thinking  string           `json:"thinking,omitempty"`
	Error     string           `json:"error,omitempty"`
	ToolCalls []mirrorToolCall `json:"tool_calls,omitempty"`
}

// transcriptMirror mirrors external worker activity into the host Antigravity
// brain/<convID>/.system_generated/logs/transcript.jsonl watched by official swarm_board.
type transcriptMirror struct {
	mu              sync.Mutex
	hostAppDataDir  string
	explicitPath    string
	skipPath        string
	convID          string
	path            string
	dirReady        bool
	scannedExisting bool
	stepIndex       int
	wroteUserInput  bool
	lastType        string
	lastStatus      string
	lastHasTools    bool
}

func newTranscriptMirror(hostAppDataDir, explicitPath, convID string) *transcriptMirror {
	m := &transcriptMirror{
		hostAppDataDir: strings.TrimSpace(hostAppDataDir),
		explicitPath:   strings.TrimSpace(explicitPath),
	}
	if cid := strings.TrimSpace(convID); cid != "" {
		m.SetConvID(cid)
	}
	return m
}

func (m *transcriptMirror) SetSkipPath(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.skipPath = strings.TrimSpace(path)
	m.resolvePathLocked()
}

func (m *transcriptMirror) SetConvID(convID string) {
	cid := strings.TrimSpace(convID)
	if cid == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.convID == cid && (m.path != "" || (m.explicitPath == "" && m.hostAppDataDir == "")) {
		return
	}
	m.convID = cid
	m.resolvePathLocked()
}

func (m *transcriptMirror) Path() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.path
}

func (m *transcriptMirror) resolvePathLocked() {
	cand := ""
	if m.explicitPath != "" {
		cand = m.explicitPath
	} else if m.hostAppDataDir != "" && m.convID != "" {
		cand = filepath.Join(m.hostAppDataDir, "brain", m.convID, ".system_generated", "logs", "transcript.jsonl")
	}
	if cand != "" && m.skipPath != "" && sameTranscriptPath(cand, m.skipPath) {
		cand = ""
	}
	if cand != m.path {
		m.path = cand
		m.dirReady = false
		m.scannedExisting = false
	}
	if m.path != "" && !m.scannedExisting {
		m.scannedExisting = true
		m.scanExistingLocked()
	}
}

func (m *transcriptMirror) scanExistingLocked() {
	f, err := os.Open(m.path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 16*1024), 4*1024*1024)
	for scanner.Scan() {
		ln := strings.TrimSpace(scanner.Text())
		if ln == "" {
			continue
		}
		m.trackRawLineLocked(ln)
	}
}

func (m *transcriptMirror) trackRawLineLocked(rawJSON string) {
	var meta struct {
		StepIndex *int   `json:"step_index"`
		Type      string `json:"type"`
		Status    string `json:"status"`
		ToolCalls []any  `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &meta); err != nil {
		m.stepIndex++
		return
	}
	if meta.StepIndex != nil && *meta.StepIndex >= m.stepIndex {
		m.stepIndex = *meta.StepIndex + 1
	} else {
		m.stepIndex++
	}
	m.lastType = meta.Type
	m.lastStatus = meta.Status
	m.lastHasTools = len(meta.ToolCalls) > 0
}

func (m *transcriptMirror) appendRawLocked(rawLine string) {
	if m.path == "" {
		return
	}
	if !m.dirReady {
		if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
			return
		}
		m.dirReady = true
	}
	f, err := os.OpenFile(m.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	clean := strings.TrimRight(rawLine, "\r\n")
	if clean == "" {
		return
	}
	_, _ = f.WriteString(clean + "\n")
}

func (m *transcriptMirror) emitStepLocked(step mirrorTranscriptStep) {
	b, err := json.Marshal(step)
	if err != nil {
		return
	}
	m.appendRawLocked(string(b))
	m.stepIndex = step.StepIndex + 1
	m.lastType = step.Type
	m.lastStatus = step.Status
	m.lastHasTools = len(step.ToolCalls) > 0
}

// WriteRawLine appends a complete JSONL line verbatim (used by Desktop-LS to forward isolated transcript lines).
func (m *transcriptMirror) WriteRawLine(rawJSON string) {
	clean := strings.TrimSpace(rawJSON)
	if clean == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.path == "" {
		return
	}
	m.trackRawLineLocked(clean)
	if m.lastType == "USER_INPUT" {
		m.wroteUserInput = true
	}
	m.appendRawLocked(clean)
}

// WriteUserInput writes a compact USER_INPUT step at turn start if not already written.
func (m *transcriptMirror) WriteUserInput(prompt string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeUserInputLocked(prompt)
}

func (m *transcriptMirror) writeUserInputLocked(prompt string) {
	if m.path == "" || m.wroteUserInput {
		return
	}
	content := strings.TrimSpace(prompt)
	if content != "" {
		content, _ = store.TruncateRunes(content, 400)
	}
	m.emitStepLocked(mirrorTranscriptStep{
		StepIndex: m.stepIndex,
		Type:      "USER_INPUT",
		Status:    "DONE",
		Content:   &content,
	})
	m.wroteUserInput = true
}

// WriteStreamStep translates an agy stream-json step_update event into a compact official transcript.jsonl line.
func (m *transcriptMirror) WriteStreamStep(stepType, state, errText, toolName, summary string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.path == "" {
		return
	}
	cleanType := strings.TrimSpace(stepType)
	if strings.EqualFold(cleanType, "error_message") {
		msg := strings.TrimSpace(errText)
		if msg == "" {
			msg = "upstream model error"
		}
		msg, _ = store.TruncateRunes(msg, 400)
		m.emitStepLocked(mirrorTranscriptStep{
			StepIndex: m.stepIndex,
			Type:      "ERROR_MESSAGE",
			Status:    "ERROR",
			Error:     msg,
		})
		return
	}
	if strings.EqualFold(cleanType, "user_input") {
		if !m.wroteUserInput {
			m.writeUserInputLocked(summary)
		}
		return
	}

	status := "RUNNING"
	if strings.EqualFold(strings.TrimSpace(state), "DONE") ||
		strings.EqualFold(strings.TrimSpace(state), "COMPLETED") ||
		strings.EqualFold(strings.TrimSpace(state), "FINISHED") {
		status = "DONE"
	}

	cleanTool := strings.TrimSpace(toolName)
	cleanSum := strings.TrimSpace(summary)
	if cleanSum != "" {
		cleanSum, _ = store.TruncateRunes(strings.Join(strings.Fields(cleanSum), " "), 160)
	}
	if cleanTool == "" && cleanType != "" &&
		!strings.EqualFold(cleanType, "planner_response") &&
		!strings.EqualFold(cleanType, "thinking") {
		cleanTool = cleanType
	}

	step := mirrorTranscriptStep{
		StepIndex: m.stepIndex,
		Type:      "PLANNER_RESPONSE",
		Status:    status,
	}
	if cleanSum != "" {
		if strings.HasPrefix(cleanSum, "**") && strings.HasSuffix(cleanSum, "**") {
			step.Thinking = cleanSum
		} else {
			step.Thinking = "**" + cleanSum + "**"
		}
	}
	if cleanTool != "" {
		toolSum := cleanSum
		if toolSum == "" {
			toolSum = cleanTool
		}
		step.ToolCalls = []mirrorToolCall{{
			Name: cleanTool,
			Args: map[string]any{"toolSummary": toolSum},
		}}
	}
	if step.Thinking == "" && len(step.ToolCalls) == 0 && status == "RUNNING" {
		step.Thinking = "**Working...**"
	}
	m.emitStepLocked(step)
}

// Finish guarantees the transcript ends cleanly with either ERROR_MESSAGE or a tool-less
// PLANNER_RESPONSE (status="DONE") so official swarm_board exits shimmering working state.
func (m *transcriptMirror) Finish(outcome TurnOutcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cid := strings.TrimSpace(outcome.ConversationID); cid != "" && m.convID != cid {
		m.convID = cid
		m.resolvePathLocked()
	}
	if m.path == "" {
		return
	}

	isErr := strings.EqualFold(strings.TrimSpace(outcome.Status), "error") ||
		strings.TrimSpace(outcome.ResultError) != "" ||
		outcome.ExitErr != nil ||
		strings.TrimSpace(outcome.EarlyStopReason) != "" ||
		len(outcome.DeniedActions) > 0

	if isErr {
		if m.lastType == "ERROR_MESSAGE" {
			return
		}
		errText := strings.TrimSpace(outcome.ResultError)
		if errText == "" {
			errText = strings.TrimSpace(outcome.EarlyStopReason)
		}
		if errText == "" && len(outcome.DeniedActions) > 0 {
			errText = "denied actions: " + strings.Join(outcome.DeniedActions, ", ")
		}
		if errText == "" && outcome.ExitErr != nil {
			errText = outcome.ExitErr.Error()
		}
		if errText == "" {
			errText = "external worker turn failed"
		}
		errText, _ = store.TruncateRunes(errText, 400)
		m.emitStepLocked(mirrorTranscriptStep{
			StepIndex: m.stepIndex,
			Type:      "ERROR_MESSAGE",
			Status:    "ERROR",
			Error:     errText,
		})
		return
	}

	if m.lastType == "PLANNER_RESPONSE" && strings.EqualFold(m.lastStatus, "DONE") && !m.lastHasTools {
		return
	}
	resp := strings.TrimSpace(outcome.Response)
	if resp != "" {
		resp, _ = store.TruncateRunes(resp, 400)
	}
	m.emitStepLocked(mirrorTranscriptStep{
		StepIndex: m.stepIndex,
		Type:      "PLANNER_RESPONSE",
		Status:    "DONE",
		Content:   &resp,
	})
}

func sameTranscriptPath(a, b string) bool {
	ca := canonicalTranscriptPath(a)
	cb := canonicalTranscriptPath(b)
	if ca == "" || cb == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		if strings.EqualFold(ca, cb) {
			return true
		}
	} else if ca == cb {
		return true
	}
	if fa, err := os.Stat(ca); err == nil {
		if fb, err := os.Stat(cb); err == nil {
			return os.SameFile(fa, fb)
		}
	}
	return false
}

func canonicalTranscriptPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(resolved)
	}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Clean(filepath.Join(parent, filepath.Base(p)))
	}
	return filepath.Clean(p)
}
