package credential

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

// OAuthSettings configures an authorization-code flow with PKCE.
//
// Every field is data the user supplies. Switchboard registers no OAuth client
// of its own and ships no client ID, which is deliberate: presenting this
// program to an authorization server as some other program's registered client
// is a decision about identity, and it belongs to whoever runs it rather than
// to a constant compiled into a binary. §5.3 allows a published flow and
// nothing more, so the flow is here and the registration is not.
type OAuthSettings struct {
	ClientID     string
	AuthorizeURL string
	TokenURL     string
	Scopes       []string

	// Audience and ExtraAuthParams cover the parameters providers add beyond
	// the specification. They are passed through rather than enumerated,
	// because guessing which ones a given server wants is how this ends up with
	// a per-vendor branch for something that is configuration.
	Audience        string
	ExtraAuthParams map[string]string

	// RedirectPort pins the loopback port when a provider requires the
	// redirect URI to match a registration exactly. Zero picks a free one,
	// which RFC 8252 permits and which avoids a collision with whatever else
	// is listening.
	RedirectPort int

	// RedirectURI overrides the redirect entirely, host and path included.
	//
	// RFC 8252 says a native client should use the literal 127.0.0.1 and may
	// use any port, and this program does that by default. A fixed client
	// registration overrides the specification in practice: the authorization
	// server compares the redirect against what was registered as a string, so
	// "localhost" and "127.0.0.1" are different values and a random port is
	// simply wrong. When a registration pins one, it goes here verbatim.
	RedirectURI string
}

func (s OAuthSettings) configured() bool {
	return s.ClientID != "" && s.AuthorizeURL != "" && s.TokenURL != ""
}

// oauthAccount suffixes the reference so a token document and an API key for
// the same provider do not overwrite one another. It is visible in the
// credential store, which is the point: a user auditing their keychain should
// be able to tell which item is which.
func oauthAccount(ref Ref) Ref {
	ref.Account = account(ref) + "#oauth"
	return ref
}

// tokenSet is what gets stored. It is a document rather than a bare string
// because a refresh token and an expiry have to survive alongside the access
// token, and losing the refresh token turns a silent renewal into a login
// prompt in the middle of a turn.
type tokenSet struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

// String redacts, for the same reason Secret does: this struct holds two
// credentials and passes through error paths.
func (t tokenSet) String() string   { return redacted }
func (t tokenSet) GoString() string { return redacted }

// expiryMargin renews early. A token that expires between the check and the
// request arrives at the server already dead, and the turn fails for a reason
// that has nothing to do with what the user asked.
const expiryMargin = 60 * time.Second

const (
	maxOAuthTokenResponseBytes = 1 << 20
	maxOAuthErrorRunes         = 1024
)

func (t tokenSet) expired(now time.Time) bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return now.Add(expiryMargin).After(t.ExpiresAt)
}

// OAuthStore supplies an access token, refreshing it when it has aged out.
//
// It reads and writes through the platform credential store rather than keeping
// its own file, so an OAuth token is protected exactly as well as an API key is
// and §5.3's rule against a plaintext fallback covers both.
type OAuthStore struct {
	Settings OAuthSettings

	// Store holds the token document. Nil uses the platform store.
	Store Writer

	// Browser opens the consent page. Nil means the URL is printed and nothing
	// is launched.
	//
	// Opening a window is a side effect on somebody's desktop, so a library does
	// not do it unless asked. The nil default also keeps a test suite from
	// spawning browser windows pointed at whatever ephemeral port it happened to
	// bind, which is exactly what this did before the field existed.
	Browser func(string)

	// Now and HTTP exist so the refresh path can be tested without a clock or a
	// network.
	Now  func() time.Time
	HTTP *http.Client
}

func (s *OAuthStore) Name() string { return "OAuth" }

func (s *OAuthStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *OAuthStore) store() Writer {
	if s.Store != nil {
		return s.Store
	}
	return NewOSStore()
}

func (s *OAuthStore) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (s *OAuthStore) Get(ctx context.Context, ref Ref) (Secret, error) {
	if !s.Settings.configured() {
		return Secret{}, ErrNotFound
	}

	tokens, err := s.read(ctx, ref)
	if err != nil {
		return Secret{}, err
	}
	if !tokens.expired(s.now()) {
		return New(tokens.AccessToken, SourceOAuth, "stored token"), nil
	}

	if tokens.RefreshToken == "" {
		return Secret{}, fmt.Errorf(
			"the stored token for %s has expired and carries no refresh token; run: sb auth oauth login %s", ref, ref)
	}
	refreshed, err := s.refresh(ctx, tokens.RefreshToken)
	if err != nil {
		return Secret{}, err
	}
	if refreshed.RefreshToken == "" {
		// Some servers rotate the refresh token and some do not. Keeping the
		// old one when none is returned is the difference between a session
		// that renews indefinitely and one that has to be logged in again.
		refreshed.RefreshToken = tokens.RefreshToken
	}
	if err := s.write(ctx, ref, refreshed); err != nil {
		return Secret{}, err
	}
	return New(refreshed.AccessToken, SourceOAuth, "refreshed token"), nil
}

func (s *OAuthStore) read(ctx context.Context, ref Ref) (tokenSet, error) {
	secret, err := s.store().Get(ctx, oauthAccount(ref))
	if err != nil {
		return tokenSet{}, err
	}
	var tokens tokenSet
	if err := json.Unmarshal([]byte(secret.Expose()), &tokens); err != nil {
		return tokenSet{}, fmt.Errorf("the stored token document for %s is unreadable; run: sb auth oauth login %s", ref, ref)
	}
	if tokens.AccessToken == "" {
		return tokenSet{}, ErrNotFound
	}
	return tokens, nil
}

func (s *OAuthStore) write(ctx context.Context, ref Ref, tokens tokenSet) error {
	// Compact, because the platform store takes a single line and a document
	// with newlines in it would be stored truncated.
	body, err := json.Marshal(tokens)
	if err != nil {
		return err
	}
	return s.store().Set(ctx, oauthAccount(ref), string(body))
}

func (s *OAuthStore) Logout(ctx context.Context, ref Ref) error {
	return s.store().Delete(ctx, oauthAccount(ref))
}

// Login runs the authorization-code flow with PKCE.
//
// PKCE is not optional here even though a loopback redirect is used: a public
// client has no secret to prove it is itself, and without the verifier any
// process that observes the authorization code can redeem it.
func (s *OAuthStore) Login(ctx context.Context, ref Ref, prompt func(url string)) error {
	if !s.Settings.configured() {
		return errors.New("no OAuth client is configured for this provider; " +
			"set client_id, authorize_url, and token_url under [auth.<provider>.oauth]")
	}

	verifier, challenge, err := pkce()
	if err != nil {
		return err
	}
	state, err := randomString(24)
	if err != nil {
		return err
	}

	listenAddr, redirectURI, err := s.redirect()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("opening %s for the redirect: %w\n"+
			"a fixed registration cannot use another port, so whatever holds this one has to be stopped first", listenAddr, err)
	}
	defer listener.Close()
	if redirectURI == "" {
		redirectURI = fmt.Sprintf("http://127.0.0.1:%d/callback", listener.Addr().(*net.TCPAddr).Port)
	}

	codes := make(chan string, 1)
	failures := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if desc := q.Get("error"); desc != "" {
			http.Error(w, "authorization failed", http.StatusBadRequest)
			failures <- fmt.Errorf("the authorization server refused: %s %s",
				sanitizeOAuthEndpointComponent(desc, nil),
				sanitizeOAuthEndpointComponent(q.Get("error_description"), nil))
			return
		}
		// The state check is what makes this flow safe to run on a loopback
		// port anything on the machine could have reached first.
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			failures <- errors.New("the redirect carried the wrong state, so it did not come from the request this flow started")
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "no code", http.StatusBadRequest)
			failures <- errors.New("the redirect carried no authorization code")
			return
		}
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "Signed in. You can close this tab and return to the terminal.")
		codes <- code
	})}
	go srv.Serve(listener)
	defer srv.Close()

	authURL := s.authorizeURL(redirectURI, state, challenge)
	if prompt != nil {
		prompt(authURL)
	}
	if s.Browser != nil {
		s.Browser(authURL)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-failures:
		return err
	case code := <-codes:
		tokens, err := s.exchange(ctx, code, verifier, redirectURI)
		if err != nil {
			return err
		}
		return s.write(ctx, ref, tokens)
	}
}

// redirect resolves where to listen and what to send.
//
// The two are not the same string. The authorization server compares the
// redirect it is sent against a registration, so that value has to be exact;
// the listener only needs the host and port out of it.
func (s *OAuthStore) redirect() (listenAddr, redirectURI string, err error) {
	if s.Settings.RedirectURI == "" {
		// 127.0.0.1 rather than localhost: RFC 8252 calls for the literal
		// address, because localhost can resolve to an interface the flow did
		// not intend.
		return fmt.Sprintf("127.0.0.1:%d", s.Settings.RedirectPort), "", nil
	}

	parsed, err := url.Parse(s.Settings.RedirectURI)
	if err != nil {
		return "", "", fmt.Errorf("the configured redirect_uri is not a URL: %w", err)
	}
	host := parsed.Hostname()
	if host == "" {
		return "", "", errors.New("the configured redirect_uri names no host")
	}
	port := parsed.Port()
	if port == "" {
		return "", "", errors.New("the configured redirect_uri names no port, and a loopback redirect needs one to listen on")
	}
	// Bind the loopback interface whatever name the registration uses for it.
	// Listening on "localhost" would depend on how the machine resolves it.
	return net.JoinHostPort("127.0.0.1", port), s.Settings.RedirectURI, nil
}

func (s *OAuthStore) authorizeURL(redirectURI, state, challenge string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", s.Settings.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	if len(s.Settings.Scopes) > 0 {
		q.Set("scope", strings.Join(s.Settings.Scopes, " "))
	}
	if s.Settings.Audience != "" {
		q.Set("audience", s.Settings.Audience)
	}
	for k, v := range s.Settings.ExtraAuthParams {
		q.Set(k, v)
	}

	sep := "?"
	if strings.Contains(s.Settings.AuthorizeURL, "?") {
		sep = "&"
	}
	return s.Settings.AuthorizeURL + sep + q.Encode()
}

func (s *OAuthStore) exchange(ctx context.Context, code, verifier, redirectURI string) (tokenSet, error) {
	return s.token(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {s.Settings.ClientID},
		"code_verifier": {verifier},
	})
}

func (s *OAuthStore) refresh(ctx context.Context, refreshToken string) (tokenSet, error) {
	return s.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {s.Settings.ClientID},
	})
}

func (s *OAuthStore) token(ctx context.Context, form url.Values) (tokenSet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Settings.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenSet{}, errors.New("the token endpoint request could not be built")
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "application/json")

	resp, err := s.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return tokenSet{}, ctx.Err()
		}
		return tokenSet{}, errors.New("the token endpoint request failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthTokenResponseBytes+1))
	if err != nil {
		return tokenSet{}, errors.New("the token endpoint response could not be read")
	}
	if len(raw) > maxOAuthTokenResponseBytes {
		return tokenSet{}, fmt.Errorf("the token endpoint response exceeded the %d-byte limit", maxOAuthTokenResponseBytes)
	}
	if !utf8.Valid(raw) {
		return tokenSet{}, errors.New("the token endpoint returned invalid UTF-8")
	}

	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`

		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	// json.Unmarshal, rather than Decoder.Decode, proves this is one complete
	// document and rejects a second trailing value.
	if err := json.Unmarshal(raw, &body); err != nil {
		return tokenSet{}, fmt.Errorf("the token endpoint returned something that is not JSON (http %d)", resp.StatusCode)
	}
	if body.Error != "" {
		// Endpoint text is external input. Scrub both returned credentials and
		// request-side grants before it can become an error, session note, or
		// terminal line.
		sensitive := []string{body.AccessToken, body.RefreshToken}
		for _, key := range []string{"refresh_token", "code", "code_verifier"} {
			sensitive = append(sensitive, form[key]...)
		}
		return tokenSet{}, fmt.Errorf("the token endpoint refused: %s %s",
			sanitizeOAuthEndpointComponent(body.Error, sensitive),
			sanitizeOAuthEndpointComponent(body.ErrorDescription, sensitive))
	}
	if resp.StatusCode >= 300 {
		return tokenSet{}, fmt.Errorf("the token endpoint returned http %d", resp.StatusCode)
	}
	if body.AccessToken == "" {
		return tokenSet{}, errors.New("the token endpoint returned no access token")
	}

	tokens := tokenSet{
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		TokenType:    body.TokenType,
	}
	if body.ExpiresIn > 0 {
		tokens.ExpiresAt = s.now().Add(time.Duration(body.ExpiresIn) * time.Second)
	}
	return tokens, nil
}

func sanitizeOAuthEndpointComponent(text string, sensitive []string) string {
	sensitive = append([]string(nil), sensitive...)
	// Replace longer exact secrets first so one value that is a prefix of
	// another cannot leave the latter's suffix behind.
	sort.Slice(sensitive, func(i, j int) bool { return len(sensitive[i]) > len(sensitive[j]) })
	seen := make(map[string]struct{}, len(sensitive))
	for _, secret := range sensitive {
		if secret == "" {
			continue
		}
		if _, ok := seen[secret]; ok {
			continue
		}
		seen[secret] = struct{}{}
		text = strings.ReplaceAll(text, secret, "[redacted: OAuth secret]")
	}
	if leaks := ScanPrompt(text); len(leaks) > 0 {
		text = Redact(text, leaks)
	}
	text = terminaltext.Escape(text)
	runes := []rune(text)
	if len(runes) > maxOAuthErrorRunes {
		text = string(runes[:maxOAuthErrorRunes]) + "…"
	}
	return text
}

// pkce returns a verifier and its S256 challenge.
func pkce() (verifier, challenge string, err error) {
	verifier, err = randomString(64)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func randomString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// OpenInBrowser is best effort, and a caller opts into it by assigning it to
// Browser. The URL is printed either way, because a headless machine has no
// browser to open and the flow still has to be completable by pasting it
// somewhere that does.
func OpenInBrowser(target string) {
	openBrowser(target)
}
