package shuffle

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func testOIDCOrg(clientSecret string) Org {
	return Org{
		Id:   "org-oidc-test",
		Name: "OIDC Test Org",
		SSOConfig: SSOConfig{
			OpenIdClientId:      "shuffle-client",
			OpenIdClientSecret:  clientSecret,
			OpenIdAuthorization: "https://idp.example.com/realms/shuffle/protocol/openid-connect/auth",
			OpenIdToken:         "https://idp.example.com/realms/shuffle/protocol/openid-connect/token",
		},
	}
}

func newOIDCTestRequest(t *testing.T) *http.Request {
	t.Helper()

	t.Setenv("BASE_URL", "https://shuffle.example.com")
	t.Setenv("SSO_REDIRECT_URL", "")

	oldEnvironment := project.Environment
	project.Environment = "onprem"
	t.Cleanup(func() {
		project.Environment = oldEnvironment
	})

	return httptest.NewRequest(http.MethodGet, "https://shuffle.example.com/login", nil)
}

func TestGetOpenIdUrlOnPremUsesAuthorizationCodeWithPKCEWhenClientSecretConfigured(t *testing.T) {
	authURL, err := GetOpenIdUrl(newOIDCTestRequest(t), testOIDCOrg("super-secret-client-secret"), User{}, "")
	if err != nil {
		t.Fatalf("GetOpenIdUrl returned error: %s", err)
	}

	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("failed parsing generated OIDC URL %q: %s", authURL, err)
	}

	query := parsed.Query()
	if got := query.Get("response_type"); got != "code" {
		t.Fatalf("response_type = %q, want code", got)
	}
	if got := query.Get("code_challenge_method"); got != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", got)
	}
	if got := query.Get("code_challenge"); got == "" {
		t.Fatal("code_challenge is empty")
	}
}

func TestGetOpenIdUrlOnPremDoesNotUseImplicitFormPost(t *testing.T) {
	authURL, err := GetOpenIdUrl(newOIDCTestRequest(t), testOIDCOrg("super-secret-client-secret"), User{}, "")
	if err != nil {
		t.Fatalf("GetOpenIdUrl returned error: %s", err)
	}

	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("failed parsing generated OIDC URL %q: %s", authURL, err)
	}

	query := parsed.Query()
	if got := query.Get("response_type"); got == "id_token" {
		t.Fatal("generated OIDC URL uses legacy implicit id_token flow")
	}
	if got := query.Get("response_mode"); got == "form_post" {
		t.Fatal("generated OIDC URL uses legacy form_post response mode")
	}
}

func TestGetOpenIdUrlOnPremDoesNotLeakClientSecret(t *testing.T) {
	clientSecret := "super-secret-client-secret"
	authURL, err := GetOpenIdUrl(newOIDCTestRequest(t), testOIDCOrg(clientSecret), User{}, "")
	if err != nil {
		t.Fatalf("GetOpenIdUrl returned error: %s", err)
	}

	if strings.Contains(authURL, clientSecret) {
		t.Fatal("generated OIDC URL leaks the client secret")
	}

	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("failed parsing generated OIDC URL %q: %s", authURL, err)
	}

	query := parsed.Query()
	for key, values := range query {
		for _, value := range values {
			if strings.Contains(value, clientSecret) {
				t.Fatalf("query parameter %q leaks the client secret", key)
			}
			if decodedContains(value, clientSecret) {
				t.Fatalf("query parameter %q leaks the client secret after base64 decoding", key)
			}
		}
	}
}

func TestGetOpenIdUrlCloudSetupStoresPKCEAndUsesCodeFlow(t *testing.T) {
	oldEnvironment := project.Environment
	project.Environment = "cloud"
	t.Cleanup(func() {
		project.Environment = oldEnvironment
	})
	t.Setenv("BASE_URL", "https://cloud.shuffle.example.com")
	t.Setenv("SSO_REDIRECT_URL", "")

	var savedUser User
	oldSetOpenIDSetupUser := setOpenIDSetupUser
	setOpenIDSetupUser = func(ctx context.Context, user *User, updateOrg bool) error {
		savedUser = *user
		return nil
	}
	t.Cleanup(func() {
		setOpenIDSetupUser = oldSetOpenIDSetupUser
	})

	user := User{
		Id:       "user-cloud-setup",
		Username: "setup@example.com",
	}
	authURL, err := GetOpenIdUrl(httptest.NewRequest(http.MethodGet, "https://cloud.shuffle.example.com/login", nil), testOIDCOrg("cloud-secret"), user, "")
	if err != nil {
		t.Fatalf("GetOpenIdUrl returned error: %s", err)
	}

	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("failed parsing generated OIDC URL %q: %s", authURL, err)
	}
	query := parsed.Query()
	if got := query.Get("response_type"); got != "code" {
		t.Fatalf("response_type = %q, want code", got)
	}
	if query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" {
		t.Fatalf("cloud setup URL did not include PKCE challenge: %s", authURL)
	}

	ssoInfo, ok := savedUser.GetSSOInfo(testOIDCOrg("").Id)
	if !ok {
		t.Fatal("cloud setup did not persist SSO info for the user")
	}
	if ssoInfo.ClientID != testOIDCOrg("").SSOConfig.OpenIdClientId {
		t.Fatalf("saved SSO client ID = %q, want %q", ssoInfo.ClientID, testOIDCOrg("").SSOConfig.OpenIdClientId)
	}
	if ssoInfo.CodeVerifier == "" {
		t.Fatal("cloud setup did not save the PKCE verifier")
	}
}

func TestGetOpenIdUrlCloudLoginDoesNotRequireUserPersistence(t *testing.T) {
	oldEnvironment := project.Environment
	project.Environment = "cloud"
	t.Cleanup(func() {
		project.Environment = oldEnvironment
	})
	t.Setenv("BASE_URL", "https://cloud.shuffle.example.com")
	t.Setenv("SSO_REDIRECT_URL", "")

	oldSetOpenIDSetupUser := setOpenIDSetupUser
	setOpenIDSetupUser = func(ctx context.Context, user *User, updateOrg bool) error {
		t.Fatal("cloud login mode should not update setup SSO info before the callback")
		return nil
	}
	t.Cleanup(func() {
		setOpenIDSetupUser = oldSetOpenIDSetupUser
	})

	org := testOIDCOrg("cloud-secret")
	user := User{
		Id:       "user-cloud-login",
		Username: "login@example.com",
		SSOInfos: []SSOInfo{{
			OrgID:    org.Id,
			Sub:      "existing-sub",
			ClientID: org.SSOConfig.OpenIdClientId,
		}},
	}
	authURL, err := GetOpenIdUrl(httptest.NewRequest(http.MethodGet, "https://cloud.shuffle.example.com/login", nil), org, user, "login")
	if err != nil {
		t.Fatalf("GetOpenIdUrl returned error: %s", err)
	}

	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("failed parsing generated OIDC URL %q: %s", authURL, err)
	}
	state := parsed.Query().Get("state")
	if state == "" {
		t.Fatal("cloud login URL did not contain opaque state")
	}

	transaction, err := consumeOpenIDLoginTransaction(state)
	if err != nil {
		t.Fatalf("failed consuming cloud login transaction: %s", err)
	}
	if transaction.Mode != "login" {
		t.Fatalf("transaction mode = %q, want login", transaction.Mode)
	}
	if transaction.ExpectedUser != user.Id {
		t.Fatalf("transaction expected user = %q, want %q", transaction.ExpectedUser, user.Id)
	}
	if transaction.CodeVerifier == "" {
		t.Fatal("cloud login transaction did not store the PKCE verifier")
	}
}

func TestOpenIDLoginTransactionUsesCacheAndWarnsWhenLocal(t *testing.T) {
	oldMemcached := memcached
	memcached = ""
	openIDLocalStateWarningOnce = sync.Once{}
	t.Cleanup(func() {
		memcached = oldMemcached
		openIDLocalStateWarningOnce = sync.Once{}
	})

	var logOutput bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logOutput)
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
	})

	transaction := oidcLoginTransaction{
		OrgID:        "org-cache-test",
		RedirectURI:  "https://shuffle.example.com/api/v1/login_openid",
		CodeVerifier: "verifier",
		Nonce:        "nonce",
		ClientID:     "shuffle-client",
		ExpiresAt:    time.Now().Add(10 * time.Minute),
	}
	if err := storeOpenIDLoginTransaction("state-cache-test", transaction); err != nil {
		t.Fatalf("storeOpenIDLoginTransaction returned error: %s", err)
	}

	stored, err := consumeOpenIDLoginTransaction("state-cache-test")
	if err != nil {
		t.Fatalf("consumeOpenIDLoginTransaction returned error: %s", err)
	}
	if stored.CodeVerifier != transaction.CodeVerifier {
		t.Fatalf("stored verifier = %q, want %q", stored.CodeVerifier, transaction.CodeVerifier)
	}

	if !strings.Contains(logOutput.String(), "SHUFFLE_MEMCACHED") {
		t.Fatal("local OIDC state storage did not warn about configuring SHUFFLE_MEMCACHED for multiple backend replicas")
	}
}

func decodedContains(value, needle string) bool {
	decoders := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}

	for _, decoder := range decoders {
		decoded, err := decoder.DecodeString(value)
		if err == nil && strings.Contains(string(decoded), needle) {
			return true
		}
	}

	return false
}

func TestVerifyIdTokenRejectsUnsignedForgedToken(t *testing.T) {
	forgedToken := forgedIDToken(t, map[string]any{
		"aud":   "shuffle-client",
		"sub":   "admin",
		"email": "admin@target.local",
		"roles": []string{"shuffle-admin"},
		"nonce": "attacker-controlled-nonce",
	})

	if _, err := VerifyIdToken(context.Background(), forgedToken); err == nil {
		t.Fatal("VerifyIdToken accepted an unsigned forged id_token")
	}
}

func TestHandleOpenIdRejectsDirectIdTokenPost(t *testing.T) {
	forgedToken := forgedIDToken(t, map[string]any{
		"aud":   "shuffle-client",
		"sub":   "admin",
		"email": "admin@target.local",
		"roles": []string{"shuffle-admin"},
		"nonce": "attacker-controlled-nonce",
	})

	req := httptest.NewRequest(http.MethodPost, "https://shuffle.example.com/api/v1/login_openid", strings.NewReader(url.Values{
		"id_token": []string{forgedToken},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	oldEnvironment := project.Environment
	project.Environment = "onprem"
	t.Cleanup(func() {
		project.Environment = oldEnvironment
	})

	recorder := httptest.NewRecorder()
	HandleOpenId(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("direct id_token POST status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == "session_token" || cookie.Name == "__session" {
			t.Fatalf("direct id_token POST set session cookie %q", cookie.Name)
		}
	}
}

func TestOpenIDIssuerFromConfigDerivesIssuerFromConfiguredEndpoints(t *testing.T) {
	tests := []struct {
		name       string
		config     SSOConfig
		wantIssuer string
	}{
		{
			name: "keycloak",
			config: SSOConfig{
				OpenIdAuthorization: "https://idp.example.com/realms/shuffle/protocol/openid-connect/auth",
				OpenIdToken:         "https://attacker.example.com/token",
			},
			wantIssuer: "https://idp.example.com/realms/shuffle",
		},
		{
			name: "azure_v2",
			config: SSOConfig{
				OpenIdAuthorization: "https://login.microsoftonline.com/tenant-id/oauth2/v2.0/authorize",
			},
			wantIssuer: "https://login.microsoftonline.com/tenant-id/v2.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotIssuer, err := OpenIDIssuerFromConfig(tt.config)
			if err != nil {
				t.Fatalf("OpenIDIssuerFromConfig returned error: %s", err)
			}
			if gotIssuer != tt.wantIssuer {
				t.Fatalf("issuer = %q, want %q", gotIssuer, tt.wantIssuer)
			}
		})
	}
}

func TestShuffleRoleFromOpenIDRolesRevokesAdminWhenProviderRoleChanges(t *testing.T) {
	tests := []struct {
		name       string
		roles      []string
		wantRole   string
		wantMapped bool
	}{
		{
			name:       "admin",
			roles:      []string{"shuffle-admin"},
			wantRole:   "admin",
			wantMapped: true,
		},
		{
			name:       "user_revokes_admin",
			roles:      []string{"shuffle-user"},
			wantRole:   "user",
			wantMapped: true,
		},
		{
			name:       "org_reader",
			roles:      []string{"shuffle-org-reader"},
			wantRole:   "org-reader",
			wantMapped: true,
		},
		{
			name:       "no_shuffle_role_defaults_to_user",
			roles:      []string{"unrelated-role"},
			wantRole:   "user",
			wantMapped: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRole, gotMapped := shuffleRoleFromOpenIDRoles(tt.roles)
			if gotRole != tt.wantRole {
				t.Fatalf("role = %q, want %q", gotRole, tt.wantRole)
			}
			if gotMapped != tt.wantMapped {
				t.Fatalf("mapped = %t, want %t", gotMapped, tt.wantMapped)
			}
		})
	}
}

func TestSyncOpenIDRoleToOrgUpdatesExistingRoleEveryLogin(t *testing.T) {
	org := Org{
		Users: []User{{
			Id:   "user-1",
			Role: "admin",
		}},
	}

	role, mapped := shuffleRoleFromOpenIDRoles([]string{"shuffle-user"})
	if !mapped {
		t.Fatal("shuffle-user role was not mapped")
	}
	if changed := syncOpenIDRoleToOrg(&org, "user-1", role); !changed {
		t.Fatal("syncOpenIDRoleToOrg did not report a changed role")
	}
	if got := org.Users[0].Role; got != "user" {
		t.Fatalf("org user role = %q, want user", got)
	}
}

func forgedIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()

	header := map[string]any{
		"alg": "none",
		"typ": "JWT",
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("failed marshaling JWT header: %s", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("failed marshaling JWT claims: %s", err)
	}

	return strings.Join([]string{
		base64.RawURLEncoding.EncodeToString(headerJSON),
		base64.RawURLEncoding.EncodeToString(claimsJSON),
		"unsigned",
	}, ".")
}

func TestRunOpenidLoginSendsOriginalPKCEVerifier(t *testing.T) {
	const wantVerifier = "original-pkce-verifier"
	var gotVerifier string

	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			t.Fatalf("failed parsing token request form: %s", err)
		}
		gotVerifier = request.Form.Get("code_verifier")
		resp.Header().Set("Content-Type", "application/json")
		_, _ = resp.Write([]byte(`{"access_token":"access-token","id_token":"id-token","token_type":"Bearer"}`))
	}))
	defer tokenEndpoint.Close()

	_, err := RunOpenidLogin(context.Background(), "shuffle-client", tokenEndpoint.URL, "https://shuffle.example.com/api/v1/login_openid", "auth-code", wantVerifier, "client-secret")
	if err != nil {
		t.Fatalf("RunOpenidLogin returned error: %s", err)
	}
	if gotVerifier != wantVerifier {
		t.Fatalf("code_verifier = %q, want %q", gotVerifier, wantVerifier)
	}
}

func TestRunOpenidLoginAllowsPublicClientWithoutClientSecret(t *testing.T) {
	const wantVerifier = "public-client-pkce-verifier"
	var gotClientID string
	var gotVerifier string
	var gotClientSecretParameter bool

	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			t.Fatalf("failed parsing token request form: %s", err)
		}

		gotClientID = request.Form.Get("client_id")
		gotVerifier = request.Form.Get("code_verifier")
		_, gotClientSecretParameter = request.Form["client_secret"]

		resp.Header().Set("Content-Type", "application/json")
		_, _ = resp.Write([]byte(`{"access_token":"access-token","id_token":"id-token","token_type":"Bearer"}`))
	}))
	defer tokenEndpoint.Close()

	_, err := RunOpenidLogin(context.Background(), "public-client", tokenEndpoint.URL, "https://shuffle.example.com/api/v1/login_openid", "auth-code", wantVerifier, "")
	if err != nil {
		t.Fatalf("RunOpenidLogin returned error: %s", err)
	}
	if gotClientID != "public-client" {
		t.Fatalf("client_id = %q, want public-client", gotClientID)
	}
	if gotVerifier != wantVerifier {
		t.Fatalf("code_verifier = %q, want %q", gotVerifier, wantVerifier)
	}
	if gotClientSecretParameter {
		t.Fatal("public client token exchange sent client_secret parameter")
	}
}

func TestRunOpenidLoginDoesNotLogTokenResponseBody(t *testing.T) {
	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, request *http.Request) {
		resp.Header().Set("Content-Type", "application/json")
		_, _ = resp.Write([]byte(`{"access_token":"access-token-secret","id_token":"id-token-secret","token_type":"Bearer"}`))
	}))
	defer tokenEndpoint.Close()

	var logOutput bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logOutput)
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
	})

	_, err := RunOpenidLogin(context.Background(), "shuffle-client", tokenEndpoint.URL, "https://shuffle.example.com/api/v1/login_openid", "auth-code", "pkce-verifier", "client-secret")
	if err != nil {
		t.Fatalf("RunOpenidLogin returned error: %s", err)
	}

	logs, err := io.ReadAll(&logOutput)
	if err != nil {
		t.Fatalf("failed reading captured logs: %s", err)
	}

	if strings.Contains(string(logs), "access-token-secret") {
		t.Fatal("logs contain access token material")
	}
	if strings.Contains(string(logs), "id-token-secret") {
		t.Fatal("logs contain ID token material")
	}
}
