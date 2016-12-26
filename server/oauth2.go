package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/dex/connector"
	"github.com/coreos/dex/storage"
)

// TODO(ericchiang): clean this file up and figure out more idiomatic error handling.

// authErr is an error response to an authorization request.
// See: https://tools.ietf.org/html/rfc6749#section-4.1.2.1
type authErr struct {
	State       string
	RedirectURI string
	Type        string
	Description string
}

func (err *authErr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v := url.Values{}
	v.Add("state", err.State)
	v.Add("error", err.Type)
	if err.Description != "" {
		v.Add("error_description", err.Description)
	}
	var redirectURI string
	if strings.Contains(err.RedirectURI, "?") {
		redirectURI = err.RedirectURI + "&" + v.Encode()
	} else {
		redirectURI = err.RedirectURI + "?" + v.Encode()
	}
	http.Redirect(w, r, redirectURI, http.StatusSeeOther)
}

func tokenErr(w http.ResponseWriter, typ, description string, statusCode int) error {
	data := struct {
		Error       string `json:"error"`
		Description string `json:"error_description,omitempty"`
	}{typ, description}
	body, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal token error response: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(statusCode)
	w.Write(body)
	return nil
}

const (
	errInvalidRequest          = "invalid_request"
	errUnauthorizedClient      = "unauthorized_client"
	errAccessDenied            = "access_denied"
	errUnsupportedResponseType = "unsupported_response_type"
	errInvalidScope            = "invalid_scope"
	errServerError             = "server_error"
	errTemporarilyUnavailable  = "temporarily_unavailable"
	errUnsupportedGrantType    = "unsupported_grant_type"
	errInvalidGrant            = "invalid_grant"
	errInvalidClient           = "invalid_client"
)

const (
	scopeOfflineAccess     = "offline_access" // Request a refresh token.
	scopeOpenID            = "openid"
	scopeGroups            = "groups"
	scopeEmail             = "email"
	scopeProfile           = "profile"
	scopeCrossClientPrefix = "audience:server:client_id:"
)

const (
	redirectURIOOB = "urn:ietf:wg:oauth:2.0:oob"
)

const (
	grantTypeAuthorizationCode = "authorization_code"
	grantTypeRefreshToken      = "refresh_token"
)

const (
	responseTypeCode    = "code"     // "Regular" flow
	responseTypeToken   = "token"    // Implicit flow for frontend apps.
	responseTypeIDToken = "id_token" // ID Token in url fragment
)

func parseScopes(scopes []string) connector.Scopes {
	var s connector.Scopes
	for _, scope := range scopes {
		switch scope {
		case scopeOfflineAccess:
			s.OfflineAccess = true
		case scopeGroups:
			s.Groups = true
		}
	}
	return s
}

type audience []string

func (a audience) MarshalJSON() ([]byte, error) {
	if len(a) == 1 {
		return json.Marshal(a[0])
	}
	return json.Marshal([]string(a))
}

type idTokenClaims struct {
	Issuer           string   `json:"iss"`
	Subject          string   `json:"sub"`
	Audience         audience `json:"aud"`
	Expiry           int64    `json:"exp"`
	IssuedAt         int64    `json:"iat"`
	AuthorizingParty string   `json:"azp,omitempty"`
	Nonce            string   `json:"nonce,omitempty"`

	Email         string `json:"email,omitempty"`
	EmailVerified *bool  `json:"email_verified,omitempty"`

	Groups []string `json:"groups,omitempty"`

	Name string `json:"name,omitempty"`
}

func (s *Server) newIDToken(clientID string, claims storage.Claims, scopes []string, nonce string) (idToken string, expiry time.Time, err error) {
	issuedAt := s.now()
	expiry = issuedAt.Add(s.idTokensValidFor)

	tok := idTokenClaims{
		Issuer:   s.issuerURL.String(),
		Subject:  claims.UserID,
		Nonce:    nonce,
		Expiry:   expiry.Unix(),
		IssuedAt: issuedAt.Unix(),
	}

	for _, scope := range scopes {
		switch {
		case scope == scopeEmail:
			tok.Email = claims.Email
			tok.EmailVerified = &claims.EmailVerified
		case scope == scopeGroups:
			tok.Groups = claims.Groups
		case scope == scopeProfile:
			tok.Name = claims.Username
		default:
			peerID, ok := parseCrossClientScope(scope)
			if !ok {
				continue
			}
			isTrusted, err := s.validateCrossClientTrust(clientID, peerID)
			if err != nil {
				return "", expiry, err
			}
			if !isTrusted {
				// TODO(ericchiang): propagate this error to the client.
				return "", expiry, fmt.Errorf("peer (%s) does not trust client", peerID)
			}
			tok.Audience = append(tok.Audience, peerID)
		}
	}
	if len(tok.Audience) == 0 {
		tok.Audience = audience{clientID}
	} else {
		tok.AuthorizingParty = clientID
	}

	payload, err := json.Marshal(tok)
	if err != nil {
		return "", expiry, fmt.Errorf("could not serialize claims: %v", err)
	}

	keys, err := s.storage.GetKeys()
	if err != nil {
		s.logger.Errorf("Failed to get keys: %v", err)
		return "", expiry, err
	}
	if idToken, err = keys.Sign(payload); err != nil {
		return "", expiry, fmt.Errorf("failed to sign payload: %v", err)
	}
	return idToken, expiry, nil
}

// parse the initial request from the OAuth2 client.
//
// For correctness the logic is largely copied from https://github.com/RangelReale/osin.
func (s *Server) parseAuthorizationRequest(supportedResponseTypes map[string]bool, r *http.Request) (req storage.AuthRequest, oauth2Err *authErr) {
	if err := r.ParseForm(); err != nil {
		return req, &authErr{"", "", errInvalidRequest, "Failed to parse request."}
	}

	redirectURI, err := url.QueryUnescape(r.Form.Get("redirect_uri"))
	if err != nil {
		return req, &authErr{"", "", errInvalidRequest, "No redirect_uri provided."}
	}
	state := r.FormValue("state")

	clientID := r.Form.Get("client_id")

	client, err := s.storage.GetClient(clientID)
	if err != nil {
		if err == storage.ErrNotFound {
			description := fmt.Sprintf("Invalid client_id (%q).", clientID)
			return req, &authErr{"", "", errUnauthorizedClient, description}
		}
		s.logger.Errorf("Failed to get client: %v", err)
		return req, &authErr{"", "", errServerError, ""}
	}

	if !validateRedirectURI(client, redirectURI) {
		description := fmt.Sprintf("Unregistered redirect_uri (%q).", redirectURI)
		return req, &authErr{"", "", errInvalidRequest, description}
	}

	newErr := func(typ, format string, a ...interface{}) *authErr {
		return &authErr{state, redirectURI, typ, fmt.Sprintf(format, a...)}
	}

	scopes := strings.Fields(r.Form.Get("scope"))

	var (
		unrecognized  []string
		invalidScopes []string
	)
	hasOpenIDScope := false
	for _, scope := range scopes {
		switch scope {
		case scopeOpenID:
			hasOpenIDScope = true
		case scopeOfflineAccess, scopeEmail, scopeProfile, scopeGroups:
		default:
			peerID, ok := parseCrossClientScope(scope)
			if !ok {
				unrecognized = append(unrecognized, scope)
				continue
			}

			isTrusted, err := s.validateCrossClientTrust(clientID, peerID)
			if err != nil {
				return req, newErr(errServerError, "")
			}
			if !isTrusted {
				invalidScopes = append(invalidScopes, scope)
			}
		}
	}
	if !hasOpenIDScope {
		return req, newErr("invalid_scope", `Missing required scope(s) ["openid"].`)
	}
	if len(unrecognized) > 0 {
		return req, newErr("invalid_scope", "Unrecognized scope(s) %q", unrecognized)
	}
	if len(invalidScopes) > 0 {
		return req, newErr("invalid_scope", "Client can't request scope(s) %q", invalidScopes)
	}

	nonce := r.Form.Get("nonce")
	responseTypes := strings.Split(r.Form.Get("response_type"), " ")
	for _, responseType := range responseTypes {
		if !supportedResponseTypes[responseType] {
			return req, newErr("invalid_request", "Invalid response type %q", responseType)
		}

		switch responseType {
		case responseTypeCode:
		case responseTypeToken:
			// Implicit flow requires a nonce value.
			// https://openid.net/specs/openid-connect-core-1_0.html#ImplicitAuthRequest
			if nonce == "" {
				return req, newErr("invalid_request", "Response type 'token' requires a 'nonce' value.")
			}

			if redirectURI == redirectURIOOB {
				err := fmt.Sprintf("Cannot use response type 'token' with redirect_uri '%s'.", redirectURIOOB)
				return req, newErr("invalid_request", err)
			}
		default:
			return req, newErr("invalid_request", "Invalid response type %q", responseType)
		}
	}

	return storage.AuthRequest{
		ID:                  storage.NewID(),
		ClientID:            client.ID,
		State:               r.Form.Get("state"),
		Nonce:               nonce,
		ForceApprovalPrompt: r.Form.Get("approval_prompt") == "force",
		Scopes:              scopes,
		RedirectURI:         redirectURI,
		ResponseTypes:       responseTypes,
	}, nil
}

func parseCrossClientScope(scope string) (peerID string, ok bool) {
	if ok = strings.HasPrefix(scope, scopeCrossClientPrefix); ok {
		peerID = scope[len(scopeCrossClientPrefix):]
	}
	return
}

func (s *Server) validateCrossClientTrust(clientID, peerID string) (trusted bool, err error) {
	if peerID == clientID {
		return true, nil
	}
	peer, err := s.storage.GetClient(peerID)
	if err != nil {
		if err != storage.ErrNotFound {
			s.logger.Errorf("Failed to get client: %v", err)
			return false, err
		}
		return false, nil
	}
	for _, id := range peer.TrustedPeers {
		if id == clientID {
			return true, nil
		}
	}
	return false, nil
}

func validateRedirectURI(client storage.Client, redirectURI string) bool {
	if !client.Public {
		for _, uri := range client.RedirectURIs {
			if redirectURI == uri {
				return true
			}
		}
		return false
	}

	if redirectURI == redirectURIOOB {
		return true
	}
	if !strings.HasPrefix(redirectURI, "http://localhost:") {
		return false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(redirectURI, "https://localhost:"))
	return err == nil && n <= 0
}
