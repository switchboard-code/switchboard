package credential

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// memStore stands in for the platform credential store.
type memStore struct {
	mu    sync.Mutex
	items map[string]string
}

func newMemStore() *memStore { return &memStore{items: map[string]string{}} }

func (m *memStore) Name() string { return "test store" }

func (m *memStore) Get(_ context.Context, ref Ref) (Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[ref.String()]
	if !ok {
		return Secret{}, ErrNotFound
	}
	return New(v, SourceKeychain, "test"), nil
}

func (m *memStore) Set(_ context.Context, ref Ref, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[ref.String()] = value
	return nil
}

func (m *memStore) Delete(_ context.Context, ref Ref) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, ref.String())
	return nil
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

const (
	early = "2026-08-13T12:00:00Z"
	later = "2026-08-13T13:00:00Z"
)

func oauthRef() Ref { return Ref{Provider: "example", Account: "first-party"} }

func seed(t *testing.T, store *memStore, tokens tokenSet) {
	t.Helper()
	body, err := json.Marshal(tokens)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), oauthAccount(oauthRef()), string(body)); err != nil {
		t.Fatal(err)
	}
}

// A token document and an API key for one provider are different credentials.
// Sharing a slot would have a login silently destroy a stored key, and the user
// would find out when a request failed rather than when it happened.
func TestOAuthDoesNotCollideWithAnAPIKey(t *testing.T) {
	if oauthAccount(oauthRef()).String() == oauthRef().String() {
		t.Fatal("the OAuth token is stored under the same reference as the API key")
	}
	if !strings.Contains(oauthAccount(oauthRef()).String(), "example") {
		t.Error("the OAuth reference lost the provider, so a keychain audit could not attribute it")
	}
}

func TestStoredTokenIsReturnedWithoutRefreshing(t *testing.T) {
	store := newMemStore()
	seed(t, store, tokenSet{AccessToken: "live-token", ExpiresAt: at(later)})

	s := &OAuthStore{
		Settings: OAuthSettings{ClientID: "c", AuthorizeURL: "https://x/a", TokenURL: "https://x/t"},
		Store:    store,
		Now:      func() time.Time { return at(early) },
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("the token endpoint must not be called for a token that is still valid")
		})},
	}

	got, err := s.Get(context.Background(), oauthRef())
	if err != nil {
		t.Fatal(err)
	}
	if got.Expose() != "live-token" {
		t.Errorf("token = %q", got.Expose())
	}
	if got.Source != SourceOAuth {
		t.Errorf("source = %q", got.Source)
	}
}

func TestExpiredTokenIsRefreshedAndStored(t *testing.T) {
	store := newMemStore()
	seed(t, store, tokenSet{AccessToken: "stale", RefreshToken: "refresh-1", ExpiresAt: at(early)})

	var sawGrant, sawRefresh string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		sawGrant = form.Get("grant_type")
		sawRefresh = form.Get("refresh_token")
		fmt.Fprint(w, `{"access_token":"fresh","expires_in":3600}`)
	}))
	defer srv.Close()

	s := &OAuthStore{
		Settings: OAuthSettings{ClientID: "c", AuthorizeURL: "https://x/a", TokenURL: srv.URL},
		Store:    store,
		Now:      func() time.Time { return at(later) },
	}

	got, err := s.Get(context.Background(), oauthRef())
	if err != nil {
		t.Fatal(err)
	}
	if got.Expose() != "fresh" {
		t.Errorf("token = %q", got.Expose())
	}
	if sawGrant != "refresh_token" || sawRefresh != "refresh-1" {
		t.Errorf("refresh request sent grant %q with token %q", sawGrant, sawRefresh)
	}

	// The refreshed token has to land in the store, or every call pays for a
	// round trip to rediscover it.
	stored, err := s.read(context.Background(), oauthRef())
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "fresh" {
		t.Errorf("stored access token = %q", stored.AccessToken)
	}
	// The server returned no new refresh token, so the old one must survive.
	// Dropping it turns a session that renews indefinitely into one that needs
	// a browser again.
	if stored.RefreshToken != "refresh-1" {
		t.Errorf("refresh token = %q; a server that does not rotate must not cost the user their session", stored.RefreshToken)
	}
}

func TestRotatedRefreshTokenReplacesTheOldOne(t *testing.T) {
	store := newMemStore()
	seed(t, store, tokenSet{AccessToken: "stale", RefreshToken: "refresh-1", ExpiresAt: at(early)})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"access_token":"fresh","refresh_token":"refresh-2","expires_in":3600}`)
	}))
	defer srv.Close()

	s := &OAuthStore{
		Settings: OAuthSettings{ClientID: "c", AuthorizeURL: "https://x/a", TokenURL: srv.URL},
		Store:    store,
		Now:      func() time.Time { return at(later) },
	}
	if _, err := s.Get(context.Background(), oauthRef()); err != nil {
		t.Fatal(err)
	}

	stored, _ := s.read(context.Background(), oauthRef())
	if stored.RefreshToken != "refresh-2" {
		t.Errorf("refresh token = %q; a rotating server invalidates the old one, so keeping it would break the next renewal", stored.RefreshToken)
	}
}

// A token that expires between the check and the request arrives dead, and the
// turn fails for a reason unrelated to what the user asked.
func TestTokenAboutToExpireIsTreatedAsExpired(t *testing.T) {
	expiry := at(early)
	tokens := tokenSet{AccessToken: "x", ExpiresAt: expiry}

	if !tokens.expired(expiry.Add(-expiryMargin / 2)) {
		t.Error("a token expiring within the margin was treated as usable")
	}
	if tokens.expired(expiry.Add(-2 * expiryMargin)) {
		t.Error("a token well inside its lifetime was treated as expired")
	}
	// No expiry means the server did not say, and guessing one would throw away
	// a working token.
	if (tokenSet{AccessToken: "x"}).expired(expiry) {
		t.Error("a token with no stated expiry was treated as expired")
	}
}

func TestExpiredTokenWithNoRefreshTokenSaysWhatToDo(t *testing.T) {
	store := newMemStore()
	seed(t, store, tokenSet{AccessToken: "stale", ExpiresAt: at(early)})

	s := &OAuthStore{
		Settings: OAuthSettings{ClientID: "c", AuthorizeURL: "https://x/a", TokenURL: "https://x/t"},
		Store:    store,
		Now:      func() time.Time { return at(later) },
	}

	_, err := s.Get(context.Background(), oauthRef())
	if err == nil {
		t.Fatal("an expired token with no way to renew resolved successfully")
	}
	if !strings.Contains(err.Error(), "sb auth oauth login") {
		t.Errorf("err = %v; the user needs to be told the one thing that fixes it", err)
	}
}

// The token request carries an authorization code or a refresh token. Neither
// belongs in an error message that will be printed and pasted.
func TestTokenErrorsDoNotQuoteTheRequest(t *testing.T) {
	store := newMemStore()
	seed(t, store, tokenSet{AccessToken: "stale", RefreshToken: "super-secret-refresh", ExpiresAt: at(early)})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"token expired"}`)
	}))
	defer srv.Close()

	s := &OAuthStore{
		Settings: OAuthSettings{ClientID: "c", AuthorizeURL: "https://x/a", TokenURL: srv.URL},
		Store:    store,
		Now:      func() time.Time { return at(later) },
	}

	_, err := s.Get(context.Background(), oauthRef())
	if err == nil {
		t.Fatal("a refused refresh resolved successfully")
	}
	if strings.Contains(err.Error(), "super-secret-refresh") {
		t.Errorf("the error quoted the refresh token: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("the server's own words were dropped: %v", err)
	}
}

func TestOAuthTokenResponseIsCompleteBoundedJSON(t *testing.T) {
	prefix := `{"access_token":"ok","padding":"`
	suffix := `"}`
	exact := prefix + strings.Repeat("x", maxOAuthTokenResponseBytes-len(prefix)-len(suffix)) + suffix

	for name, response := range map[string][]byte{
		"exact boundary": []byte(exact),
		"one byte over":  []byte(exact + " "),
		"trailing value": []byte(`{"access_token":"ok"}{"access_token":"other"}`),
		"invalid utf8":   append([]byte(`{"access_token":"`), []byte{0xff, '"', '}'}...),
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(response)
			}))
			defer srv.Close()
			s := &OAuthStore{Settings: OAuthSettings{TokenURL: srv.URL}}
			tokens, err := s.token(context.Background(), url.Values{})
			if name == "exact boundary" {
				if err != nil || tokens.AccessToken != "ok" {
					t.Fatalf("exact boundary: tokens=%v err=%v", tokens, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("unsafe response was accepted: %+v", tokens)
			}
		})
	}
}

func TestOAuthEndpointDiagnosticsNeverExposeTokensOrTerminalControls(t *testing.T) {
	const (
		access   = "short-access-secret"
		returned = "short-refresh-secret"
		outbound = "outbound-refresh-secret"
	)
	credentialToken := "ghp_" + strings.Repeat("A", 40)
	response, err := json.Marshal(map[string]string{
		"access_token":      access,
		"refresh_token":     returned,
		"error":             "invalid_grant\x1b]2;forged\a",
		"error_description": "echo " + access + " " + returned + " " + outbound + " " + credentialToken + "\n\u202e",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(response)
	}))
	defer srv.Close()
	s := &OAuthStore{Settings: OAuthSettings{TokenURL: srv.URL}}
	_, err = s.token(context.Background(), url.Values{"refresh_token": {outbound}})
	if err == nil {
		t.Fatal("endpoint refusal was accepted")
	}
	message := err.Error()
	for _, secret := range []string{access, returned, outbound, credentialToken, "ghp_"} {
		if strings.Contains(message, secret) {
			t.Fatalf("OAuth error exposed %q: %q", secret, message)
		}
	}
	for _, control := range []string{"\x1b", "\a", "\n", "\u202e"} {
		if strings.Contains(message, control) {
			t.Fatalf("OAuth error retained terminal control %q: %q", control, message)
		}
	}
	for _, escaped := range []string{`\x1b`, `\x07`, `\x0a`, `\u202e`} {
		if !strings.Contains(message, escaped) {
			t.Errorf("OAuth error did not visibly escape %q: %q", escaped, message)
		}
	}
	if !strings.Contains(message, "invalid_grant") {
		t.Fatalf("OAuth error lost its useful nonsecret diagnostic: %q", message)
	}
}

// An unconfigured provider is a miss, not a failure: the resolver has to be able
// to carry on to the platform store, which is where most users' keys live.
func TestUnconfiguredOAuthIsAMiss(t *testing.T) {
	s := &OAuthStore{Store: newMemStore()}

	_, err := s.Get(context.Background(), oauthRef())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want a miss", err)
	}
	if len(Chain(Settings{}).Sources()) != 2 {
		t.Error("an unconfigured OAuth client added itself to the chain")
	}
	if len(Chain(Settings{OAuth: OAuthSettings{ClientID: "c", AuthorizeURL: "a", TokenURL: "t"}}).Sources()) != 3 {
		t.Error("a configured OAuth client did not join the chain")
	}
}

// PKCE is what stands in for a client secret a command-line tool cannot keep.
// Without the verifier, any process that observes the authorization code on the
// loopback redirect can redeem it.
func TestPKCEChallengeMatchesItsVerifier(t *testing.T) {
	verifier, challenge, err := pkce()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); challenge != want {
		t.Errorf("challenge = %q, want the S256 of the verifier", challenge)
	}
	if len(verifier) < 43 || len(verifier) > 128 {
		t.Errorf("verifier is %d characters, outside the 43 to 128 the specification allows", len(verifier))
	}

	other, _, err := pkce()
	if err != nil {
		t.Fatal(err)
	}
	if other == verifier {
		t.Error("two flows produced the same verifier, so it is not random")
	}
}

func TestAuthorizeURLCarriesThePKCEParameters(t *testing.T) {
	s := &OAuthStore{Settings: OAuthSettings{
		ClientID:        "client-123",
		AuthorizeURL:    "https://auth.example.com/authorize",
		Scopes:          []string{"openid", "offline_access"},
		ExtraAuthParams: map[string]string{"prompt": "consent"},
	}}

	raw := s.authorizeURL("http://127.0.0.1:1234/callback", "state-abc", "challenge-xyz")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()

	for field, want := range map[string]string{
		"response_type":         "code",
		"client_id":             "client-123",
		"state":                 "state-abc",
		"code_challenge":        "challenge-xyz",
		"code_challenge_method": "S256",
		"scope":                 "openid offline_access",
		"prompt":                "consent",
		"redirect_uri":          "http://127.0.0.1:1234/callback",
	} {
		if got := q.Get(field); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
}

// The redirect lands on a loopback port anything on the machine could have
// reached first, so the state parameter is what proves the response belongs to
// the request this flow started.
func TestCallbackWithTheWrongStateIsRefused(t *testing.T) {
	store := newMemStore()
	s := &OAuthStore{
		Settings: OAuthSettings{ClientID: "c", AuthorizeURL: "https://x/a", TokenURL: "https://x/t"},
		Store:    store,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- s.Login(ctx, oauthRef(), func(raw string) {
			parsed, err := url.Parse(raw)
			if err != nil {
				return
			}
			redirect, err := url.Parse(parsed.Query().Get("redirect_uri"))
			if err != nil {
				return
			}
			// Answer with a code but the wrong state, which is what a hostile
			// local process would be able to do.
			http.Get(redirect.String() + "?code=stolen&state=not-the-right-state")
		})
	}()

	select {
	case err := <-errs:
		if err == nil {
			t.Fatal("a callback carrying the wrong state completed the login")
		}
		if !strings.Contains(err.Error(), "state") {
			t.Errorf("err = %v, want it to name the state mismatch", err)
		}
	case <-ctx.Done():
		t.Fatal("the login never returned")
	}

	if _, err := store.Get(context.Background(), oauthAccount(oauthRef())); !errors.Is(err, ErrNotFound) {
		t.Error("a refused callback still wrote a token")
	}
}

// The stored document holds two credentials and passes through error paths, so
// it redacts for the same reason Secret does.
func TestTokenSetNeverRenders(t *testing.T) {
	tokens := tokenSet{AccessToken: "access-secret", RefreshToken: "refresh-secret"}

	for name, got := range map[string]string{
		"%v":     fmt.Sprintf("%v", tokens),
		"%s":     fmt.Sprintf("%s", tokens),
		"%#v":    fmt.Sprintf("%#v", tokens),
		"errorf": fmt.Errorf("failed: %v", tokens).Error(),
	} {
		if strings.Contains(got, "access-secret") || strings.Contains(got, "refresh-secret") {
			t.Errorf("%s leaked a token: %s", name, got)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The whole flow, end to end, against a stand-in authorization server: consent,
// redirect, code exchange, storage, and then resolution through the chain the
// way a request would get it.
func TestFullLoginThenResolveThroughTheChain(t *testing.T) {
	store := newMemStore()

	var mu sync.Mutex
	var seenVerifier, seenCode string
	issuedCode := "code-from-the-server"

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/authorize":
			// Stand in for the user consenting in a browser.
			q := r.URL.Query()
			mu.Lock()
			challenge := q.Get("code_challenge")
			mu.Unlock()
			if challenge == "" {
				t.Error("the authorization request carried no PKCE challenge")
			}
			redirect := q.Get("redirect_uri") + "?code=" + issuedCode + "&state=" + q.Get("state")
			http.Redirect(w, r, redirect, http.StatusFound)

		case "/token":
			body, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(body))
			mu.Lock()
			seenVerifier = form.Get("code_verifier")
			seenCode = form.Get("code")
			mu.Unlock()
			fmt.Fprint(w, `{"access_token":"issued-access","refresh_token":"issued-refresh","expires_in":3600}`)
		}
	}))
	defer srv.Close()

	settings := OAuthSettings{
		ClientID:     "client-abc",
		AuthorizeURL: srv.URL + "/authorize",
		TokenURL:     srv.URL + "/token",
		Scopes:       []string{"openid", "offline_access"},
	}
	oauth := &OAuthStore{Settings: settings, Store: store, Now: func() time.Time { return at(early) }}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Following the URL is what a browser does; here the test does it.
	if err := oauth.Login(ctx, oauthRef(), func(raw string) {
		go func() {
			resp, err := srv.Client().Get(raw)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}); err != nil {
		t.Fatalf("login: %v", err)
	}

	mu.Lock()
	verifier, code := seenVerifier, seenCode
	mu.Unlock()
	if code != issuedCode {
		t.Errorf("the exchange sent code %q", code)
	}
	if verifier == "" {
		t.Error("the exchange carried no PKCE verifier, so the code could be redeemed by anyone who saw it")
	}

	// Now resolve the way a request would, through the ordered chain rather
	// than by reaching into the store.
	chain := NewResolver(
		&EnvStore{lookup: envOf(nil)},
		&OAuthStore{Settings: settings, Store: store, Now: func() time.Time { return at(early) }},
	)
	got, err := chain.Get(ctx, oauthRef())
	if err != nil {
		t.Fatalf("resolving after login: %v", err)
	}
	if got.Expose() != "issued-access" {
		t.Errorf("resolved %q", got.Expose())
	}
	if got.Source != SourceOAuth {
		t.Errorf("source = %q", got.Source)
	}

	// And logging out has to actually remove it.
	if err := oauth.Logout(ctx, oauthRef()); err != nil {
		t.Fatal(err)
	}
	if _, err := chain.Get(ctx, oauthRef()); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v; the tokens survived a logout", err)
	}
}

// A fixed client registration is compared as a string, so the redirect has to
// be sent exactly as registered even where that contradicts RFC 8252's advice
// to use the literal loopback address and any free port. Getting it wrong comes
// back as a generic authentication error that names none of those things.
func TestPinnedRedirectIsSentVerbatimAndListenedOnLoopback(t *testing.T) {
	s := &OAuthStore{Settings: OAuthSettings{
		ClientID:     "c",
		AuthorizeURL: "https://x/a",
		TokenURL:     "https://x/t",
		RedirectURI:  "http://localhost:1455/auth/callback",
	}}

	listenAddr, redirectURI, err := s.redirect()
	if err != nil {
		t.Fatal(err)
	}
	if redirectURI != "http://localhost:1455/auth/callback" {
		t.Errorf("redirect_uri = %q; a registration is matched as a string and this one would be rejected", redirectURI)
	}
	// The listener binds the loopback interface directly rather than depending
	// on how this machine happens to resolve "localhost".
	if listenAddr != "127.0.0.1:1455" {
		t.Errorf("listen address = %q, want the loopback interface on the registered port", listenAddr)
	}
}

func TestUnpinnedRedirectPicksAFreePort(t *testing.T) {
	s := &OAuthStore{Settings: OAuthSettings{ClientID: "c", AuthorizeURL: "https://x/a", TokenURL: "https://x/t"}}

	listenAddr, redirectURI, err := s.redirect()
	if err != nil {
		t.Fatal(err)
	}
	if redirectURI != "" {
		t.Errorf("redirect_uri = %q; without a registration it is derived from whichever port was granted", redirectURI)
	}
	if listenAddr != "127.0.0.1:0" {
		t.Errorf("listen address = %q, want a free port on the loopback interface", listenAddr)
	}
}

func TestMalformedPinnedRedirectIsRejected(t *testing.T) {
	for _, raw := range []string{"http://localhost/auth/callback", "not-a-url", "http:///callback"} {
		s := &OAuthStore{Settings: OAuthSettings{ClientID: "c", AuthorizeURL: "a", TokenURL: "t", RedirectURI: raw}}
		if _, _, err := s.redirect(); err == nil {
			t.Errorf("redirect_uri %q was accepted; a loopback redirect needs a host and a port to listen on", raw)
		}
	}
}

// Running the test suite must not open browser windows on whoever's machine is
// running it. The default is nil rather than the real opener precisely so that
// forgetting to override it is harmless.
func TestLoginOpensNoBrowserUnlessAsked(t *testing.T) {
	settings := OAuthSettings{ClientID: "c", AuthorizeURL: "https://x/a", TokenURL: "https://x/t"}

	// Two stores rather than one mutated in place: the first Login is still
	// running when the second is set up, so assigning Browser on a shared store
	// races with the goroutine reading it.
	silent := &OAuthStore{Settings: settings, Store: newMemStore()}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = silent.Login(ctx, oauthRef(), func(string) {}) }()
	<-ctx.Done()

	var mu sync.Mutex
	var opened []string
	asked := &OAuthStore{
		Settings: settings,
		Store:    newMemStore(),
		Browser: func(url string) {
			mu.Lock()
			defer mu.Unlock()
			opened = append(opened, url)
		},
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	go func() { _ = asked.Login(ctx2, oauthRef(), nil) }()
	<-ctx2.Done()

	mu.Lock()
	defer mu.Unlock()
	if len(opened) == 0 {
		t.Error("an explicitly supplied browser was never called")
	}
	// The silent store had no Browser and could not have opened anything, which
	// is the property: forgetting to override it is harmless.
	if silent.Browser != nil {
		t.Error("a store with no browser configured acquired one")
	}
}
