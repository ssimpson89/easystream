package youtube

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/ssimpson89/easystream/internal/atomicfile"
)

const ytScope = "https://www.googleapis.com/auth/youtube"

// googleEndpoint is Google's OAuth 2.0 endpoint. Hard-coded so we don't pull
// in golang.org/x/oauth2/google (which would add the entire google package).
var googleEndpoint = oauth2.Endpoint{
	AuthURL:   "https://accounts.google.com/o/oauth2/v2/auth",
	TokenURL:  "https://oauth2.googleapis.com/token",
	AuthStyle: oauth2.AuthStyleInParams,
}

// storedToken is the on-disk representation. We store the oauth2.Token fields
// plus channel info we cached from the YouTube API.
type storedToken struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	TokenType    string    `json:"tokenType"`
	Expiry       time.Time `json:"expiry"`
	ChannelName  string    `json:"channelName,omitempty"`
	ChannelID    string    `json:"channelId,omitempty"`
}

func (s *storedToken) toOauth2() *oauth2.Token {
	if s == nil {
		return nil
	}
	return &oauth2.Token{
		AccessToken:  s.AccessToken,
		RefreshToken: s.RefreshToken,
		TokenType:    s.TokenType,
		Expiry:       s.Expiry,
	}
}

// Auth manages YouTube OAuth tokens via golang.org/x/oauth2.
//
// The library handles the access-token refresh dance automatically when we
// call cfg.Client(ctx, token). We wrap its TokenSource with a persistTokenSource
// so refreshed tokens get written back to disk.
type Auth struct {
	cfg       *oauth2.Config
	tokenFile string
	logger    *log.Logger // optional; when set, refresh-persistence failures are logged

	mu     sync.Mutex
	token  *storedToken
	state  string // CSRF state for the current consent flow
	client *http.Client
}

// SetLogger installs a logger so token-refresh persistence failures surface
// somewhere instead of being silently dropped on disk-full / permission errors.
//
// Takes a.mu because persistTokenSource.Token reads a.logger under the same
// lock; an unlocked write here is a data race even though in practice we
// only call SetLogger at startup.
func (a *Auth) SetLogger(l *log.Logger) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.logger = l
	a.mu.Unlock()
}

// NewAuth creates an Auth that stores tokens at tokenFile.
// Returns nil if clientID or clientSecret are empty (YouTube features disabled).
func NewAuth(clientID, clientSecret, redirectURI, tokenFile string) *Auth {
	if clientID == "" || clientSecret == "" {
		return nil
	}
	a := &Auth{
		cfg: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURI,
			Scopes:       []string{ytScope},
			Endpoint:     googleEndpoint,
		},
		tokenFile: tokenFile,
	}
	_ = a.loadToken()
	return a
}

// IsAuthenticated returns true if we have a refresh token.
func (a *Auth) IsAuthenticated() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.token != nil && a.token.RefreshToken != ""
}

// AuthStatus returns a summary for the API.
func (a *Auth) AuthStatus() map[string]any {
	if a == nil {
		return map[string]any{"configured": false, "authenticated": false}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	result := map[string]any{
		"configured":    true,
		"authenticated": a.token != nil && a.token.RefreshToken != "",
	}
	if a.token != nil {
		result["channelName"] = a.token.ChannelName
		result["channelId"] = a.token.ChannelID
	}
	return result
}

// AuthURL returns the Google consent URL. Open this in the user's browser.
// Includes offline access + force-consent so we always get a refresh token.
func (a *Auth) AuthURL() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	a.state = randomState()
	state := a.state
	a.mu.Unlock()

	return a.cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce, // alias for prompt=consent
	)
}

// Exchange trades an authorization code for tokens.
func (a *Auth) Exchange(code, state string) error {
	if a == nil {
		return fmt.Errorf("youtube auth not configured")
	}
	a.mu.Lock()
	expected := a.state
	a.mu.Unlock()
	if expected == "" || state != expected {
		return fmt.Errorf("invalid state parameter")
	}

	tok, err := a.cfg.Exchange(context.Background(), code)
	if err != nil {
		return fmt.Errorf("token exchange failed: %w", err)
	}

	a.mu.Lock()
	a.token = &storedToken{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		TokenType:    tok.TokenType,
		Expiry:       tok.Expiry,
	}
	a.state = ""
	// Invalidate the cached HTTP client. It holds a TokenSource bound to
	// the old token snapshot; using it after re-auth would keep talking
	// to YouTube as the previous identity until the next Logout.
	a.client = nil
	a.mu.Unlock()

	// Fetch channel info with the new token (non-fatal if it fails).
	_ = a.fetchChannelInfo()

	return a.saveToken()
}

// HTTPClient returns an *http.Client with automatic token refresh. The
// oauth2 library refreshes near expiry; persistTokenSource writes the
// refreshed token to disk. The client is cached for the lifetime of the
// stored token so we don't rebuild the OAuth pipeline on every API call.
func (a *Auth) HTTPClient() (*http.Client, error) {
	if a == nil {
		return nil, fmt.Errorf("youtube auth not configured")
	}
	a.mu.Lock()
	stored := a.token
	cached := a.client
	a.mu.Unlock()
	if stored == nil || stored.RefreshToken == "" {
		return nil, fmt.Errorf("not authenticated with YouTube")
	}
	if cached != nil {
		return cached, nil
	}
	src := a.cfg.TokenSource(context.Background(), stored.toOauth2())
	persistSrc := &persistTokenSource{src: src, auth: a}
	client := oauth2.NewClient(context.Background(), persistSrc)
	client.Timeout = 30 * time.Second
	a.mu.Lock()
	a.client = client
	a.mu.Unlock()
	return client, nil
}

// VerifyAuth probes the saved token by fetching channel info. Exercises
// the full OAuth refresh path and returns the channel name on success.
// Use at startup to surface "credentials are wrong" instead of finding out
// when the volunteer clicks Go Live on Sunday morning.
func (a *Auth) VerifyAuth() (string, error) {
	if a == nil {
		return "", fmt.Errorf("not configured")
	}
	if !a.IsAuthenticated() {
		return "", fmt.Errorf("no saved token")
	}
	if err := a.fetchChannelInfo(); err != nil {
		return "", err
	}
	a.mu.Lock()
	name := ""
	if a.token != nil {
		name = a.token.ChannelName
	}
	a.mu.Unlock()
	if name == "" {
		return "", fmt.Errorf("channel info not available")
	}
	return name, nil
}

// Logout clears stored tokens.
func (a *Auth) Logout() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	a.token = nil
	a.client = nil
	a.mu.Unlock()
	_ = os.Remove(a.tokenFile)
	return nil
}

// persistTokenSource wraps an oauth2.TokenSource so that refreshed tokens
// get written to disk and to the Auth's in-memory cache.
type persistTokenSource struct {
	src  oauth2.TokenSource
	auth *Auth
}

func (p *persistTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.src.Token()
	if err != nil {
		return nil, err
	}
	p.auth.mu.Lock()
	prev := p.auth.token
	// Only re-save if the access token actually changed (refresh happened).
	if prev == nil || prev.AccessToken != tok.AccessToken {
		channelName, channelID := "", ""
		if prev != nil {
			channelName, channelID = prev.ChannelName, prev.ChannelID
		}
		// Refresh responses sometimes omit refresh_token; preserve the existing one.
		refresh := tok.RefreshToken
		if refresh == "" && prev != nil {
			refresh = prev.RefreshToken
		}
		p.auth.token = &storedToken{
			AccessToken:  tok.AccessToken,
			RefreshToken: refresh,
			TokenType:    tok.TokenType,
			Expiry:       tok.Expiry,
			ChannelName:  channelName,
			ChannelID:    channelID,
		}
		logger := p.auth.logger
		p.auth.mu.Unlock()
		if err := p.auth.saveToken(); err != nil && logger != nil {
			logger.Printf("youtube: failed to persist refreshed token: %v", err)
		}
	} else {
		p.auth.mu.Unlock()
	}
	return tok, nil
}

func (a *Auth) fetchChannelInfo() error {
	client, err := a.HTTPClient()
	if err != nil {
		return err
	}
	resp, err := client.Get("https://www.googleapis.com/youtube/v3/channels?part=snippet&mine=true")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("channels API returned %d", resp.StatusCode)
	}
	var result struct {
		Items []struct {
			ID      string `json:"id"`
			Snippet struct {
				Title string `json:"title"`
			} `json:"snippet"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if len(result.Items) == 0 {
		return fmt.Errorf("no YouTube channel found")
	}

	a.mu.Lock()
	if a.token != nil {
		a.token.ChannelName = result.Items[0].Snippet.Title
		a.token.ChannelID = result.Items[0].ID
	}
	a.mu.Unlock()
	return a.saveToken()
}

func (a *Auth) loadToken() error {
	data, err := os.ReadFile(a.tokenFile)
	if err != nil {
		return err
	}
	var tok storedToken
	if err := json.Unmarshal(data, &tok); err != nil {
		return err
	}
	a.mu.Lock()
	a.token = &tok
	a.mu.Unlock()
	return nil
}

func (a *Auth) saveToken() error {
	a.mu.Lock()
	tok := a.token
	a.mu.Unlock()
	if tok == nil {
		return nil
	}
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(a.tokenFile, data, 0600)
}

func randomState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
