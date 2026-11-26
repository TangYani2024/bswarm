package agentapi

import "testing"

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
