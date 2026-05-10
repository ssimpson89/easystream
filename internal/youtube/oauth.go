package youtube

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	authEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenEndpoint = "https://oauth2.googleapis.com/token"
	ytScope       = "https://www.googleapis.com/auth/youtube"
)

// Token holds OAuth 2.0 credentials persisted to disk.
type Token struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	TokenType    string    `json:"tokenType"`
	ExpiresAt    time.Time `json:"expiresAt"`
	ChannelName  string    `json:"channelName,omitempty"`
	ChannelID    string    `json:"channelId,omitempty"`
}

func (t *Token) expired() bool {
	return t == nil || time.Now().After(t.ExpiresAt.Add(-30*time.Second))
}

// Auth manages YouTube OAuth tokens.
type Auth struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
	TokenFile    string

	mu    sync.Mutex
	token *Token
	state string // CSRF state for current auth flow
}

// NewAuth creates an Auth that stores tokens at tokenFile.
// Returns nil if clientID or clientSecret are empty (YouTube features disabled).
func NewAuth(clientID, clientSecret, redirectURI, tokenFile string) *Auth {
	if clientID == "" || clientSecret == "" {
		return nil
	}
	a := &Auth{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURI:  redirectURI,
		TokenFile:    tokenFile,
	}
	_ = a.loadToken()
	return a
}

// Configured returns true if YouTube OAuth credentials are set.
func (a *Auth) Configured() bool {
	return a != nil && a.ClientID != ""
}

// IsAuthenticated returns true if we have a valid refresh token.
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

// AuthURL returns the Google consent URL. Opens this in the user's browser.
func (a *Auth) AuthURL() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	a.state = randomState()
	state := a.state
	a.mu.Unlock()

	params := url.Values{
		"client_id":     {a.ClientID},
		"redirect_uri":  {a.RedirectURI},
		"response_type": {"code"},
		"scope":         {ytScope},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	return authEndpoint + "?" + params.Encode()
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

	data := url.Values{
		"code":          {code},
		"client_id":     {a.ClientID},
		"client_secret": {a.ClientSecret},
		"redirect_uri":  {a.RedirectURI},
		"grant_type":    {"authorization_code"},
	}
	tok, err := postToken(data)
	if err != nil {
		return fmt.Errorf("token exchange failed: %w", err)
	}

	a.mu.Lock()
	a.token = tok
	a.state = ""
	a.mu.Unlock()

	// Fetch channel info with the new token.
	if err := a.fetchChannelInfo(); err != nil {
		// Non-fatal; token still valid.
		_ = err
	}

	return a.saveToken()
}

// HTTPClient returns an *http.Client that injects the access token.
// Automatically refreshes expired tokens.
func (a *Auth) HTTPClient() (*http.Client, error) {
	if a == nil {
		return nil, fmt.Errorf("youtube auth not configured")
	}
	if err := a.ensureFreshToken(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	accessToken := a.token.AccessToken
	a.mu.Unlock()

	return &http.Client{
		Transport: &bearerTransport{token: accessToken, base: http.DefaultTransport},
		Timeout:   30 * time.Second,
	}, nil
}

// Logout clears stored tokens.
func (a *Auth) Logout() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	a.token = nil
	a.mu.Unlock()
	_ = os.Remove(a.TokenFile)
	return nil
}

func (a *Auth) ensureFreshToken() error {
	a.mu.Lock()
	tok := a.token
	a.mu.Unlock()

	if tok == nil || tok.RefreshToken == "" {
		return fmt.Errorf("not authenticated with YouTube")
	}
	if !tok.expired() {
		return nil
	}

	data := url.Values{
		"refresh_token": {tok.RefreshToken},
		"client_id":     {a.ClientID},
		"client_secret": {a.ClientSecret},
		"grant_type":    {"refresh_token"},
	}
	newTok, err := postToken(data)
	if err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}
	// Refresh responses don't include a new refresh token; keep the old one.
	if newTok.RefreshToken == "" {
		newTok.RefreshToken = tok.RefreshToken
	}
	newTok.ChannelName = tok.ChannelName
	newTok.ChannelID = tok.ChannelID

	a.mu.Lock()
	a.token = newTok
	a.mu.Unlock()
	return a.saveToken()
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
	a.token.ChannelName = result.Items[0].Snippet.Title
	a.token.ChannelID = result.Items[0].ID
	a.mu.Unlock()
	return a.saveToken()
}

func (a *Auth) loadToken() error {
	data, err := os.ReadFile(a.TokenFile)
	if err != nil {
		return err
	}
	var tok Token
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
	if err := os.MkdirAll(filepath.Dir(a.TokenFile), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.TokenFile, data, 0600)
}

// postToken exchanges credentials at the Google token endpoint.
func postToken(data url.Values) (*Token, error) {
	resp, err := http.PostForm(tokenEndpoint, data)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, body)
	}

	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if raw.Error != "" {
		return nil, fmt.Errorf("%s: %s", raw.Error, raw.ErrorDesc)
	}
	return &Token{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		TokenType:    raw.TokenType,
		ExpiresAt:    time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second),
	}, nil
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}

func randomState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
