package mcpfacade

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pashcock/agent-hub/internal/adapter"
	"github.com/pashcock/agent-hub/internal/adapter/fake"
	"github.com/pashcock/agent-hub/internal/hub"
	"github.com/pashcock/agent-hub/internal/store"
)

func TestProviderOperatorRequiresTokenAndReportsPause(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := hub.New(st, map[string]adapter.Adapter{"grok": fake.New()}, hub.Config{})
	defer h.Close()
	f := New(h, []string{"grok"})
	mux := http.NewServeMux()
	f.RegisterProviderOperator(mux, "test-token")
	for _, tc := range []struct {
		token string
		code  int
	}{{"", 401}, {"wrong", 401}, {"test-token", 200}} {
		req := httptest.NewRequest("POST", "http://localhost/operator/provider", strings.NewReader(`{"agent":"grok","action":"pause","reason":"quota"}`))
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		if res.Code != tc.code {
			t.Fatalf("status %d want %d", res.Code, tc.code)
		}
	}
	closeSession, sess := newTestSession(t, f)
	defer closeSession()
	out := callTool(t, sess, "peer_list", map[string]any{})
	agents := out["agents"].([]any)
	p := agents[0].(map[string]any)["availability"].(map[string]any)
	if p["paused"] != true || p["reason"] != "quota" {
		t.Fatalf("discovery lost pause: %v", p)
	}
}
