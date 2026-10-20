package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/config"
	"github.com/Dicklesworthstone/ntm/internal/serve"
)

func TestServeCmdRejectsUnexpectedArguments(t *testing.T) {
	cmd := newServeCmd()
	cmd.SetArgs([]string{"unexpected"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("serve accepted an unexpected positional argument")
	}
	// serve declares cobra.NoArgs, whose message is `unknown command %q for %q`.
	// "accepts 0 arg(s)" is ExactArgs(0)'s wording and never applied here; the
	// contract under test is that the positional is rejected at all.
	if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("error = %q, want Cobra no-arguments error", err)
	}
}

// TestServeConfigCarriesAgentMailSettings locks bd-h90o7: ntm serve hands the
// loaded [agent_mail] settings to the server, so the mail endpoints reach the
// configured url with the configured token, and enabled = false keeps them
// off the server entirely.
func TestServeConfigCarriesAgentMailSettings(t *testing.T) {
	var (
		mu    sync.Mutex
		auths []string
	)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer mock.Close()
	t.Setenv("AGENT_MAIL_URL", "")
	t.Setenv("AGENT_MAIL_TOKEN", "")
	requests := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), auths...)
	}

	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg })

	mailHealth := func(t *testing.T, settings config.AgentMailConfig) bool {
		t.Helper()
		cfg = &config.Config{AgentMail: settings}
		opts := serveOptions{Host: "127.0.0.1", Port: serve.DefaultPort, AuthMode: "local"}
		srv := serve.New(newServeConfig(opts, serve.AuthModeLocal, nil, nil))
		t.Cleanup(srv.Stop)

		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/mail/health", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/mail/health = %d %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Available bool `json:"available"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode mail health %q: %v", rec.Body.String(), err)
		}
		return body.Available
	}

	t.Run("enabled", func(t *testing.T) {
		before := len(requests())
		available := mailHealth(t, config.AgentMailConfig{Enabled: true, URL: mock.URL + "/", Token: "cfg-token"})
		got := requests()[before:]
		if !available || len(got) == 0 || got[0] != "Bearer cfg-token" {
			t.Fatalf("available = %v, Authorization headers = %q; want the configured server reached with the configured token", available, got)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		before := len(requests())
		available := mailHealth(t, config.AgentMailConfig{Enabled: false, URL: mock.URL + "/", Token: "cfg-token"})
		if got := requests()[before:]; available || len(got) != 0 {
			t.Fatalf("available = %v, requests = %q; want Agent Mail unavailable and never contacted when [agent_mail] enabled = false", available, got)
		}
	})
}
