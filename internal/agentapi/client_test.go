package agentapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseConversationIDVariants(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"flat snake", `{"conversation_id":"abc-1234-5678"}`, "abc-1234-5678"},
		{"nested protojson", `{"response":{"conversationId":"ec15f799-0000-4000-8000-111111111111"}}`, "ec15f799-0000-4000-8000-111111111111"},
		{"cascade id", `{"cascadeId":"cas-9999-8888"}`, "cas-9999-8888"},
		{"error payload", `{"error":"connection refused"}`, ""},
	}
	for _, tc := range cases {
		got := ParseConversationID([]byte(tc.raw))
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	if errMsg := extractError([]byte(`{"error":"bad token"}`)); errMsg != "bad token" {
		t.Errorf("extractError got %q", errMsg)
	}
}

func TestPathToFileURI(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/tmp/my proj/demo", "file:///tmp/my%20proj/demo"},
		{`C:\Users\tester\demo`, "file:///C:/Users/tester/demo"},
		{`\\tsclient\share\demo`, "file://tsclient/share/demo"},
	}
	for _, tc := range cases {
		if got := PathToFileURI(tc.in); got != tc.want {
			t.Errorf("PathToFileURI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNewConversationUsesSubagentStartCascadeWithParent(t *testing.T) {
	var startReq, msgReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-codeium-csrf-token") != "tok-123" {
			http.Error(w, "bad csrf", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/StartCascade"):
			startReq = body
			_ = json.NewEncoder(w).Encode(map[string]any{"cascadeId": "child-sub-001"})
		case strings.HasSuffix(r.URL.Path, "/SendUserCascadeMessage"):
			msgReq = body
			_ = json.NewEncoder(w).Encode(map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("ANTIGRAVITY_LS_ADDRESS", strings.TrimPrefix(srv.URL, "http://"))
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "tok-123")
	t.Setenv("BSWARM_USE_AGENTAPI_NEW_CONV", "")

	tr := NewCLITransport()
	id, err := tr.NewConversation(context.Background(), NewConversationOpts{
		Title:                "[bswarm] w1 (worker)",
		Prompt:               "Do work",
		ParentConversationID: "root-conv-999",
		WorkspacePath:        "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	if id != "child-sub-001" {
		t.Fatalf("got id=%q, want child-sub-001", id)
	}
	if startReq["source"] != "CORTEX_TRAJECTORY_SOURCE_SUBAGENT" {
		t.Fatalf("expected SUBAGENT source, got %#v", startReq)
	}
	if startReq["parentConversationId"] != "root-conv-999" {
		t.Fatalf("expected parentConversationId=root-conv-999, got %#v", startReq)
	}
	if msgReq["cascadeId"] != "child-sub-001" {
		t.Fatalf("expected SendUserCascadeMessage to child-sub-001, got %#v", msgReq)
	}
}
