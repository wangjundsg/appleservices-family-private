package icloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestHideMyEmailLifecycleBindsCurrentAlias(t *testing.T) {
	var mu sync.Mutex
	active := true
	selectedForward := "primary@example.test"
	paths := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("dsid") != "123" || r.URL.Query().Get("clientBuildNumber") != hmeBuildNumber {
			http.Error(w, `{"success":false}`, http.StatusBadRequest)
			return
		}
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/hme/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"forwardToEmails": []string{"primary@example.test", "backup@example.test"}, "selectedForwardTo": selectedForward, "hmeEmails": []map[string]any{{"hme": "alias@icloud.com", "label": "shop", "note": "test", "isActive": active, "anonymousId": "private-id", "forwardToEmail": selectedForward}}}})
		case "/v1/hme/generate":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"hme": "candidate@icloud.com"}})
		case "/v1/hme/reserve", "/v1/hme/updateMetaData", "/v1/hme/reactivate", "/v1/hme/delete":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{}})
		case "/v1/hme/updateForwardTo":
			var request map[string]string
			if json.NewDecoder(r.Body).Decode(&request) != nil || request["forwardToEmail"] != "backup@example.test" {
				t.Fatal("forward target request was not bound to the selected address")
			}
			selectedForward = request["forwardToEmail"]
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{}})
		case "/v1/hme/deactivate":
			active = false
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &HideMyEmailClient{
		appleID:     "member@example.test",
		sessionPath: filepath.Join(t.TempDir(), "hme-session.json"),
		http:        server.Client(),
		jar:         jar,
		session:     hmeSession{AppleID: "member@example.test", DSID: "123", ClientID: "client", ServiceURL: server.URL},
	}
	client.http.Jar = jar
	ctx := context.Background()
	info, err := client.List(ctx)
	if err != nil || info.ActiveCount != 1 || len(info.Aliases) != 1 || info.Aliases[0].anonymousID != "private-id" || info.SelectedForward != "primary@example.test" || len(info.ForwardToEmails) != 2 {
		t.Fatalf("unexpected list info=%#v err=%v", info, err)
	}
	candidate, err := client.Generate(ctx, "zh-cn")
	if err != nil || candidate != "candidate@icloud.com" {
		t.Fatalf("unexpected candidate=%q err=%v", candidate, err)
	}
	if err := client.Reserve(ctx, candidate, "label", "note"); err != nil {
		t.Fatal(err)
	}
	if err := client.UpdateForwardTo(ctx, "unverified@example.test"); err == nil {
		t.Fatal("unverified forwarding target was accepted")
	}
	if err := client.UpdateForwardTo(ctx, "backup@example.test"); err != nil {
		t.Fatal(err)
	}
	forwarded, err := client.List(ctx)
	if err != nil || forwarded.SelectedForward != "backup@example.test" || forwarded.Aliases[0].ForwardToEmail != "backup@example.test" {
		t.Fatalf("forwarding change was not verified: %#v err=%v", forwarded, err)
	}
	if err := client.Update(ctx, "alias@icloud.com", "updated", "updated note"); err != nil {
		t.Fatal(err)
	}
	if err := client.Delete(ctx, "alias@icloud.com"); err == nil {
		t.Fatal("active alias was deleted")
	}
	if err := client.SetActive(ctx, "alias@icloud.com", false); err != nil {
		t.Fatal(err)
	}
	if err := client.Delete(ctx, "alias@icloud.com"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) < 8 || paths[len(paths)-1] != "/v1/hme/delete" {
		t.Fatalf("unexpected request paths %v", paths)
	}
}

func TestHideMyEmailSafeAliasOmitsPrivateIdentifier(t *testing.T) {
	data, err := json.Marshal(HideMyEmailAlias{Email: "alias@icloud.com", anonymousID: "private-id"})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"email":"alias@icloud.com","active":false}` {
		t.Fatalf("private identifier leaked in %s", data)
	}
}

func TestValidateHMEWebCookiesExplainsRepairState(t *testing.T) {
	if err := validateHMEWebCookies([]*http.Cookie{{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "token"}}); err != nil {
		t.Fatal(err)
	}
	err := validateHMEWebCookies([]*http.Cookie{{Name: "X-APPLE-WEBAUTH-REPAIR", Value: "repair"}})
	if !errors.Is(err, ErrHMEWebRepairRequired) {
		t.Fatalf("repair state was not explained: %v", err)
	}
}

func TestAcceptHMEWebTermsUsesAdvertisedVersion(t *testing.T) {
	var accepted int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getTerms":
			if r.Method != http.MethodGet {
				t.Fatalf("getTerms method = %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"iCloudTerms": map[string]any{"version": 634301}})
		case "/repairDone":
			if r.Method != http.MethodGet {
				t.Fatalf("repairDone method = %s", r.Method)
			}
			var body struct {
				Accepted int `json:"acceptedICloudTerms"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Fatal("repairDone body was not valid JSON")
			}
			accepted = body.Accepted
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &HideMyEmailClient{http: server.Client(), jar: jar}
	if err := client.acceptHMEWebTerms(context.Background(), server.URL, "zh-cn"); err != nil {
		t.Fatal(err)
	}
	if accepted != 634301 {
		t.Fatalf("accepted terms version = %d", accepted)
	}
}

func TestAcceptPendingTermsRequiresRepairCookie(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	client := &HideMyEmailClient{setupBase: "https://setup.icloud.com/setup/ws/1", http: http.DefaultClient, jar: jar}
	if err := client.AcceptPendingTerms(context.Background()); !errors.Is(err, ErrHMEWebRepairSessionUnavailable) {
		t.Fatalf("missing repair session was not rejected: %v", err)
	}
}

func TestUseWebSessionReusesBrowserLoginWithoutPassword(t *testing.T) {
	t.Setenv("APPLESERVICES_SOCKS_PROXY", "")
	var serverURL string
	var mu sync.Mutex
	adoptions := 0
	active := true
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/accountLogin":
			var body struct {
				DSWebAuthToken string `json:"dsWebAuthToken"`
				Extended       bool   `json:"extended_login"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.DSWebAuthToken != "browser-session-token" || !body.Extended {
				http.Error(w, `{"success":false}`, http.StatusUnauthorized)
				return
			}
			mu.Lock()
			adoptions++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"dsInfo":      map[string]any{"dsid": "23087649739"},
				"webservices": map[string]any{"premiummailsettings": map[string]any{"url": serverURL + "/hme"}},
			})
		case "/hme/v2/hme/list":
			if r.URL.Query().Get("dsid") != "23087649739" {
				http.Error(w, `{"success":false}`, http.StatusBadRequest)
				return
			}
			mu.Lock()
			current, removed := active, deleted
			mu.Unlock()
			emails := []map[string]any{}
			if !removed {
				emails = append(emails, map[string]any{"hme": "alias@icloud.com", "label": "shop", "isActive": current, "anonymousId": "private-id", "forwardToEmail": "primary@example.test"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"selectedForwardTo": "primary@example.test", "forwardToEmails": []string{"primary@example.test"}, "hmeEmails": emails}})
		case "/hme/v1/hme/generate":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"hme": "candidate@icloud.com"}})
		case "/hme/v1/hme/reserve":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{}})
		case "/hme/v1/hme/deactivate":
			mu.Lock()
			active = false
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{}})
		case "/hme/v1/hme/delete":
			mu.Lock()
			deleted = true
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	serverURL = server.URL

	dir := t.TempDir()
	client, err := NewHideMyEmailClient(dir, "owner@example.test")
	if err != nil {
		t.Fatal(err)
	}
	client.setupBase = server.URL
	ctx := context.Background()
	web := WebSession{AppleID: "owner@example.test", DSID: "23087649739", Cookies: []WebSessionCookie{
		{Name: "X-APPLE-DS-WEB-SESSION-TOKEN", Value: "browser-session-token", Domain: "127.0.0.1", Path: "/"},
		{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "browser-auth-token", Domain: "127.0.0.1", Path: "/"},
	}}
	if err := client.UseWebSession(ctx, web); err != nil {
		t.Fatalf("web session adoption failed: %v", err)
	}
	if !client.SessionActive() {
		t.Fatal("web session was not bound to the client")
	}
	info, err := client.List(ctx)
	if err != nil || info.ActiveCount != 1 || len(info.Aliases) != 1 {
		t.Fatalf("list after adoption: info=%#v err=%v", info, err)
	}
	candidate, err := client.Generate(ctx, "zh-cn")
	if err != nil || candidate != "candidate@icloud.com" {
		t.Fatalf("generate after adoption: candidate=%q err=%v", candidate, err)
	}
	if err := client.Reserve(ctx, candidate, "label", ""); err != nil {
		t.Fatalf("reserve after adoption: %v", err)
	}
	if err := client.SetActive(ctx, "alias@icloud.com", false); err != nil {
		t.Fatalf("deactivate after adoption: %v", err)
	}
	if err := client.Delete(ctx, "alias@icloud.com"); err != nil {
		t.Fatalf("delete after adoption: %v", err)
	}

	// A restart must keep working from the saved session alone.
	reloaded, err := NewHideMyEmailClient(dir, "owner@example.test")
	if err != nil {
		t.Fatal(err)
	}
	reloaded.http = server.Client()
	reloaded.http.Jar = reloaded.jar
	if !reloaded.SessionActive() {
		t.Fatal("saved session was not reloaded")
	}
	if _, err := reloaded.List(ctx); err != nil {
		t.Fatalf("list after restart: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if adoptions != 1 {
		t.Fatalf("browser session was adopted %d times", adoptions)
	}
}

func TestUseWebSessionRejectsUnusableBrowserCookies(t *testing.T) {
	dir := t.TempDir()
	client, err := NewHideMyEmailClient(dir, "owner@example.test")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := client.UseWebSession(ctx, WebSession{Cookies: []WebSessionCookie{{Name: "aasp", Value: "cookie-value"}}}); err == nil {
		t.Fatal("a cookie set without a web session token was adopted")
	}
	if err := client.UseWebSession(ctx, WebSession{Cookies: []WebSessionCookie{{Name: "X-APPLE-WEBAUTH-REPAIR", Value: "cookie-value"}}}); !errors.Is(err, ErrHMEWebRepairRequired) {
		t.Fatalf("repair-only session was not explained: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "hme-session.json")); !os.IsNotExist(statErr) {
		t.Fatal("an unusable web session was written to the account directory")
	}
	if _, listErr := client.List(ctx); !errors.Is(listErr, ErrHMEAuthenticationRequired) {
		t.Fatalf("missing session did not fall back to authentication: %v", listErr)
	}
}

// Review item B6: the DSID returned by Apple is a cross-check on the caller's
// session, so it has to be compared in memory. Before this test the handshake
// persisted the session first and compared afterwards, which left a rejected
// web session in the account directory. A mismatch must leave the file exactly
// as it was (or absent when there was none).
func TestUseWebSessionDSIDMismatchLeavesSessionFileUntouched(t *testing.T) {
	t.Setenv("APPLESERVICES_SOCKS_PROXY", "")
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/accountLogin":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"dsInfo":      map[string]any{"dsid": "23087649739"},
				"webservices": map[string]any{"premiummailsettings": map[string]any{"url": serverURL + "/hme"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	serverURL = server.URL

	cookies := []WebSessionCookie{
		{Name: "X-APPLE-DS-WEB-SESSION-TOKEN", Value: "browser-session-token", Domain: "127.0.0.1", Path: "/"},
		{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "browser-auth-token", Domain: "127.0.0.1", Path: "/"},
	}
	newClient := func(t *testing.T) (*HideMyEmailClient, string) {
		t.Helper()
		dir := t.TempDir()
		client, err := NewHideMyEmailClient(dir, "owner@example.test")
		if err != nil {
			t.Fatal(err)
		}
		client.setupBase = server.URL
		return client, filepath.Join(dir, "hme-session.json")
	}

	// (1) No session file yet: a rejected adoption must not create one.
	client, sessionFile := newClient(t)
	if err := client.UseWebSession(context.Background(), WebSession{AppleID: "owner@example.test", DSID: "99999999999", Cookies: cookies}); err == nil {
		t.Fatal("web session with a mismatching DSID was accepted")
	}
	if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
		t.Fatalf("a rejected web session created %s", sessionFile)
	}
	if client.SessionActive() {
		t.Fatal("a rejected web session was left bound to the client")
	}

	// (2) Existing session file: its bytes must survive the rejection.
	client, sessionFile = newClient(t)
	before := []byte("{\n  \"apple_id\": \"owner@example.test\",\n  \"dsid\": \"23087649739\",\n  \"client_id\": \"previous\",\n  \"service_url\": \"" + server.URL + "/hme\",\n  \"cookies\": [],\n  \"updated_at\": \"2026-01-01T00:00:00Z\"\n}\n")
	if err := os.WriteFile(sessionFile, before, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.UseWebSession(context.Background(), WebSession{AppleID: "owner@example.test", DSID: "99999999999", Cookies: cookies}); err == nil {
		t.Fatal("web session with a mismatching DSID was accepted")
	}
	after, err := os.ReadFile(sessionFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("session file changed after a rejected web session:\nbefore=%s\nafter =%s", before, after)
	}
}

func TestUseWebSessionKeepsPreviousSessionWhenRejected(t *testing.T) {
	t.Setenv("APPLESERVICES_SOCKS_PROXY", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/accountLogin":
			http.Error(w, `{"success":false}`, http.StatusUnauthorized)
		case "/hme/v2/hme/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"hmeEmails": []map[string]any{}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewHideMyEmailClient(t.TempDir(), "owner@example.test")
	if err != nil {
		t.Fatal(err)
	}
	client.http = server.Client()
	client.http.Jar = client.jar
	client.session = hmeSession{AppleID: "owner@example.test", DSID: "23087649739", ClientID: "client", ServiceURL: server.URL + "/hme"}
	ctx := context.Background()
	web := WebSession{AppleID: "owner@example.test", AccountCountry: "USA", Cookies: []WebSessionCookie{
		{Name: "X-APPLE-DS-WEB-SESSION-TOKEN", Value: "expired-session-token", Domain: "127.0.0.1", Path: "/"},
		{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "browser-auth-token", Domain: "127.0.0.1", Path: "/"},
	}}
	if err := client.UseWebSession(ctx, web); err == nil {
		t.Fatal("expired browser session was accepted")
	}
	if !client.SessionActive() {
		t.Fatal("previous session state was discarded on failure")
	}
	if _, err := client.List(ctx); err != nil {
		t.Fatalf("previous session stopped working: %v", err)
	}
}

func TestSavedSessionCarriesWebCookiesToServiceHost(t *testing.T) {
	dir := t.TempDir()
	client, err := NewHideMyEmailClient(dir, "owner@example.test")
	if err != nil {
		t.Fatal(err)
	}
	client.session = hmeSession{AppleID: "owner@example.test", DSID: "23087649739", ClientID: "client", ServiceURL: "https://p68-maildomainws.icloud.com"}
	web, err := url.Parse("https://www.icloud.com")
	if err != nil {
		t.Fatal(err)
	}
	// A browser export scopes these to ".icloud.com"; the service host must
	// still receive them after the session is written and read back.
	client.jar.SetCookies(web, []*http.Cookie{{Name: "X-APPLE-WEBAUTH-TOKEN", Value: "cookie-value", Domain: ".icloud.com", Path: "/", Secure: true}})
	if err := client.saveSession(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewHideMyEmailClient(dir, "owner@example.test")
	if err != nil {
		t.Fatal(err)
	}
	service, err := url.Parse("https://p68-maildomainws.icloud.com")
	if err != nil {
		t.Fatal(err)
	}
	cookies := reloaded.jar.Cookies(service)
	if len(cookies) != 1 || cookies[0].Name != "X-APPLE-WEBAUTH-TOKEN" || cookies[0].Value != "cookie-value" {
		t.Fatalf("service host did not receive the saved web cookie: %#v", cookies)
	}
}
