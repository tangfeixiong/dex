// Package oidc implements logging in through OpenID Connect providers.
package oidc

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/Sirupsen/logrus"
	"github.com/coreos/go-oidc"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"

	"github.com/coreos/dex/connector"
)

// Config holds configuration options for OpenID Connect logins.
type Config struct {
	Issuer       string `json:"issuer"`
	ClientID     string `json:"clientID"`
	ClientSecret string `json:"clientSecret"`
	RedirectURI  string `json:"redirectURI"`

	Scopes []string `json:"scopes"` // defaults to "profile" and "email"
}

// Open returns a connector which can be used to login users through an upstream
// OpenID Connect provider.
func (c *Config) Open(logger logrus.FieldLogger) (conn connector.Connector, err error) {
	ctx, cancel := context.WithCancel(context.Background())

	provider, err := oidc.NewProvider(ctx, c.Issuer)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to get provider: %v", err)
	}

	scopes := []string{oidc.ScopeOpenID}
	if len(c.Scopes) > 0 {
		scopes = append(scopes, c.Scopes...)
	} else {
		scopes = append(scopes, "profile", "email")
	}

	clientID := c.ClientID
	return &oidcConnector{
		redirectURI: c.RedirectURI,
		oauth2Config: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: c.ClientSecret,
			Endpoint:     provider.Endpoint(),
			Scopes:       scopes,
			RedirectURL:  c.RedirectURI,
		},
		verifier: provider.Verifier(
			oidc.VerifyExpiry(),
			oidc.VerifyAudience(clientID),
		),
		logger: logger,
	}, nil
}

var (
	_ connector.CallbackConnector = (*oidcConnector)(nil)
)

type oidcConnector struct {
	redirectURI  string
	oauth2Config *oauth2.Config
	verifier     *oidc.IDTokenVerifier
	ctx          context.Context
	cancel       context.CancelFunc
	logger       logrus.FieldLogger
}

func (c *oidcConnector) Close() error {
	c.cancel()
	return nil
}

func (c *oidcConnector) LoginURL(s connector.Scopes, callbackURL, state string) (string, error) {
	if c.redirectURI != callbackURL {
		return "", fmt.Errorf("expected callback URL did not match the URL in the config")
	}
	return c.oauth2Config.AuthCodeURL(state), nil
}

type oauth2Error struct {
	error            string
	errorDescription string
}

func (e *oauth2Error) Error() string {
	if e.errorDescription == "" {
		return e.error
	}
	return e.error + ": " + e.errorDescription
}

func (c *oidcConnector) HandleCallback(s connector.Scopes, r *http.Request) (identity connector.Identity, err error) {
	q := r.URL.Query()
	if errType := q.Get("error"); errType != "" {
		return identity, &oauth2Error{errType, q.Get("error_description")}
	}
	token, err := c.oauth2Config.Exchange(r.Context(), q.Get("code"))
	if err != nil {
		return identity, fmt.Errorf("oidc: failed to get token: %v", err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		return identity, errors.New("oidc: no id_token in token response")
	}
	idToken, err := c.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		return identity, fmt.Errorf("oidc: failed to verify ID Token: %v", err)
	}

	var claims struct {
		Username      string `json:"name"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return identity, fmt.Errorf("oidc: failed to decode claims: %v", err)
	}

	identity = connector.Identity{
		UserID:        idToken.Subject,
		Username:      claims.Username,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
	}
	return identity, nil
}
