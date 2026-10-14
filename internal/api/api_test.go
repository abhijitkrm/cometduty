package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abhijitkrm/cometduty/internal/alert"
)

const token = "0123456789abcdef-token"

func server(t *testing.T) (*httptest.Server, *alert.Engine) {
	t.Helper()
	eng := alert.NewEngine(func(*alert.Alert) []alert.ResolvedDest { return nil }, 0, 0)
	mux := http.NewServeMux()
	Register(mux, token, eng)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, eng
}

func call(t *testing.T, method, url, tok, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// The body amtool and Grafana send.
func amSilence(d time.Duration) string {
	b, _ := json.Marshal(map[string]any{
		"matchers":  []map[string]any{{"name": "node", "value": "val1", "isRegex": false, "isEqual": true}},
		"startsAt":  time.Now().Format(time.RFC3339),
		"endsAt":    time.Now().Add(d).Format(time.RFC3339),
		"createdBy": "ops", "comment": "upgrade",
	})
	return string(b)
}

func TestSilenceAPIAlertmanagerCompatible(t *testing.T) {
	srv, eng := server(t)
	resp := call(t, "POST", srv.URL+"/api/v2/silences", token, amSilence(time.Hour))
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	var created struct{ SilenceID string }
	_ = json.NewDecoder(resp.Body).Decode(&created)
	if created.SilenceID == "" || eng.ActiveSilences() != 1 {
		t.Fatalf("created %+v, active %d", created, eng.ActiveSilences())
	}
	var list []struct {
		ID     string `json:"id"`
		Status struct{ State string }
	}
	_ = json.NewDecoder(call(t, "GET", srv.URL+"/api/v2/silences", token, "").Body).Decode(&list)
	if len(list) != 1 || list[0].ID != created.SilenceID || list[0].Status.State != "active" {
		t.Fatalf("list %+v", list)
	}
	if r := call(t, "DELETE", srv.URL+"/api/v2/silence/"+created.SilenceID, token, ""); r.StatusCode != 200 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if eng.ActiveSilences() != 0 {
		t.Fatal("still active after delete")
	}
	if r := call(t, "DELETE", srv.URL+"/api/v2/silence/nope", token, ""); r.StatusCode != 404 {
		t.Fatalf("unknown id: %d", r.StatusCode)
	}
}

func TestSilenceAPINeedsToken(t *testing.T) {
	srv, eng := server(t)
	for _, tok := range []string{"", "wrong-token-wrong-token"} {
		if r := call(t, "POST", srv.URL+"/api/v2/silences", tok, amSilence(time.Hour)); r.StatusCode != 401 {
			t.Errorf("token %q: %d", tok, r.StatusCode)
		}
		if r := call(t, "GET", srv.URL+"/api/v2/silences", tok, ""); r.StatusCode != 401 {
			t.Errorf("list with token %q: %d", tok, r.StatusCode)
		}
	}
	if eng.ActiveSilences() != 0 {
		t.Fatal("unauthenticated silence stored")
	}
	if r := call(t, "POST", srv.URL+"/api/v2/silences", token, amSilence(-time.Minute)); r.StatusCode != 400 {
		t.Errorf("expired silence: %d", r.StatusCode)
	}
}

func TestNoTokenNoAPI(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, "", nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if r := call(t, "GET", srv.URL+"/api/v2/silences", "", ""); r.StatusCode != 404 {
		t.Fatalf("api served without a token: %d", r.StatusCode)
	}
}
