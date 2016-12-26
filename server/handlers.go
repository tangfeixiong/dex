package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	jose "gopkg.in/square/go-jose.v2"

	"github.com/coreos/dex/connector"
	"github.com/coreos/dex/storage"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	start := s.now()
	err := func() error {
		// Instead of trying to introspect health, just try to use the underlying storage.
		a := storage.AuthRequest{
			ID:       storage.NewID(),
			ClientID: storage.NewID(),

			// Set a short expiry so if the delete fails this will be cleaned up quickly by garbage collection.
			Expiry: s.now().Add(time.Minute),
		}

		if err := s.storage.CreateAuthRequest(a); err != nil {
			return fmt.Errorf("create auth request: %v", err)
		}
		if err := s.storage.DeleteAuthRequest(a.ID); err != nil {
			return fmt.Errorf("delete auth request: %v", err)
		}
		return nil
	}()

	t := s.now().Sub(start)
	if err != nil {
		s.logger.Errorf("Storage health check failed: %v", err)
		s.renderError(w, http.StatusInternalServerError, "Health check failed.")
		return
	}
	fmt.Fprintf(w, "Health check passed in %s", t)
}

func (s *Server) handlePublicKeys(w http.ResponseWriter, r *http.Request) {
	// TODO(ericchiang): Cache this.
	keys, err := s.storage.GetKeys()
	if err != nil {
		s.logger.Errorf("failed to get keys: %v", err)
		s.renderError(w, http.StatusInternalServerError, "Internal server error.")
		return
	}

	if keys.SigningKeyPub == nil {
		s.logger.Errorf("No public keys found.")
		s.renderError(w, http.StatusInternalServerError, "Internal server error.")
		return
	}

	jwks := jose.JSONWebKeySet{
		Keys: make([]jose.JSONWebKey, len(keys.VerificationKeys)+1),
	}
	jwks.Keys[0] = *keys.SigningKeyPub
	for i, verificationKey := range keys.VerificationKeys {
		jwks.Keys[i+1] = *verificationKey.PublicKey
	}

	data, err := json.MarshalIndent(jwks, "", "  ")
	if err != nil {
		s.logger.Errorf("failed to marshal discovery data: %v", err)
		s.renderError(w, http.StatusInternalServerError, "Internal server error.")
		return
	}
	maxAge := keys.NextRotation.Sub(s.now())
	if maxAge < (time.Minute * 2) {
		maxAge = time.Minute * 2
	}

	w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d, must-revalidate", int(maxAge.Seconds())))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

type discovery struct {
	Issuer        string   `json:"issuer"`
	Auth          string   `json:"authorization_endpoint"`
	Token         string   `json:"token_endpoint"`
	Keys          string   `json:"jwks_uri"`
	ResponseTypes []string `json:"response_types_supported"`
	Subjects      []string `json:"subject_types_supported"`
	IDTokenAlgs   []string `json:"id_token_signing_alg_values_supported"`
	Scopes        []string `json:"scopes_supported"`
	AuthMethods   []string `json:"token_endpoint_auth_methods_supported"`
	Claims        []string `json:"claims_supported"`
}

func (s *Server) discoveryHandler() (http.HandlerFunc, error) {
	d := discovery{
		Issuer:      s.issuerURL.String(),
		Auth:        s.absURL("/auth"),
		Token:       s.absURL("/token"),
		Keys:        s.absURL("/keys"),
		Subjects:    []string{"public"},
		IDTokenAlgs: []string{string(jose.RS256)},
		Scopes:      []string{"openid", "email", "groups", "profile", "offline_access"},
		AuthMethods: []string{"client_secret_basic"},
		Claims: []string{
			"aud", "email", "email_verified", "exp",
			"iat", "iss", "locale", "name", "sub",
		},
	}

	for responseType := range s.supportedResponseTypes {
		d.ResponseTypes = append(d.ResponseTypes, responseType)
	}
	sort.Strings(d.ResponseTypes)

	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal discovery data: %v", err)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Write(data)
	}, nil
}

// handleAuthorization handles the OAuth2 auth endpoint.
func (s *Server) handleAuthorization(w http.ResponseWriter, r *http.Request) {
	authReq, err := s.parseAuthorizationRequest(s.supportedResponseTypes, r)
	if err != nil {
		s.logger.Errorf("Failed to parse authorization request: %v", err)
		s.renderError(w, http.StatusInternalServerError, "Failed to connect to the database.")
		return
	}
	authReq.Expiry = s.now().Add(time.Minute * 30)
	if err := s.storage.CreateAuthRequest(authReq); err != nil {
		s.logger.Errorf("Failed to create authorization request: %v", err)
		s.renderError(w, http.StatusInternalServerError, "Failed to connect to the database.")
		return
	}
	if len(s.connectors) == 1 {
		for id := range s.connectors {
			http.Redirect(w, r, s.absPath("/auth", id)+"?req="+authReq.ID, http.StatusFound)
			return
		}
	}

	connectorInfos := make([]connectorInfo, len(s.connectors))
	i := 0
	for id, conn := range s.connectors {
		connectorInfos[i] = connectorInfo{
			ID:   id,
			Name: conn.DisplayName,
			URL:  s.absPath("/auth", id),
		}
		i++
	}

	if err := s.templates.login(w, connectorInfos, authReq.ID); err != nil {
		s.logger.Errorf("Server template error: %v", err)
	}
}

func (s *Server) handleConnectorLogin(w http.ResponseWriter, r *http.Request) {
	connID := mux.Vars(r)["connector"]
	conn, ok := s.connectors[connID]
	if !ok {
		s.logger.Errorf("Failed to create authorization request.")
		s.renderError(w, http.StatusBadRequest, "Requested resource does not exist.")
		return
	}

	authReqID := r.FormValue("req")

	authReq, err := s.storage.GetAuthRequest(authReqID)
	if err != nil {
		s.logger.Errorf("Failed to get auth request: %v", err)
		s.renderError(w, http.StatusInternalServerError, "Database error.")
		return
	}
	scopes := parseScopes(authReq.Scopes)

	switch r.Method {
	case "GET":
		// Set the connector being used for the login.
		updater := func(a storage.AuthRequest) (storage.AuthRequest, error) {
			a.ConnectorID = connID
			return a, nil
		}
		if err := s.storage.UpdateAuthRequest(authReqID, updater); err != nil {
			s.logger.Errorf("Failed to set connector ID on auth request: %v", err)
			s.renderError(w, http.StatusInternalServerError, "Database error.")
			return
		}

		switch conn := conn.Connector.(type) {
		case connector.CallbackConnector:
			// Use the auth request ID as the "state" token.
			//
			// TODO(ericchiang): Is this appropriate or should we also be using a nonce?
			callbackURL, err := conn.LoginURL(scopes, s.absURL("/callback"), authReqID)
			if err != nil {
				s.logger.Errorf("Connector %q returned error when creating callback: %v", connID, err)
				s.renderError(w, http.StatusInternalServerError, "Login error.")
				return
			}
			http.Redirect(w, r, callbackURL, http.StatusFound)
		case connector.PasswordConnector:
			if err := s.templates.password(w, authReqID, r.URL.String(), "", false); err != nil {
				s.logger.Errorf("Server template error: %v", err)
			}
		default:
			s.renderError(w, http.StatusBadRequest, "Requested resource does not exist.")
		}
	case "POST":
		passwordConnector, ok := conn.Connector.(connector.PasswordConnector)
		if !ok {
			s.renderError(w, http.StatusBadRequest, "Requested resource does not exist.")
			return
		}

		username := r.FormValue("login")
		password := r.FormValue("password")

		identity, ok, err := passwordConnector.Login(r.Context(), scopes, username, password)
		if err != nil {
			s.logger.Errorf("Failed to login user: %v", err)
			s.renderError(w, http.StatusInternalServerError, "Login error.")
			return
		}
		if !ok {
			if err := s.templates.password(w, authReqID, r.URL.String(), username, true); err != nil {
				s.logger.Errorf("Server template error: %v", err)
			}
			return
		}
		redirectURL, err := s.finalizeLogin(identity, authReq, conn.Connector)
		if err != nil {
			s.logger.Errorf("Failed to finalize login: %v", err)
			s.renderError(w, http.StatusInternalServerError, "Login error.")
			return
		}

		http.Redirect(w, r, redirectURL, http.StatusSeeOther)
	default:
		s.renderError(w, http.StatusBadRequest, "Unsupported request method.")
	}
}

func (s *Server) handleConnectorCallback(w http.ResponseWriter, r *http.Request) {
	// SAML redirect bindings use the "RelayState" URL query field. When we support
	// SAML, we'll have to check that field too and possibly let callback connectors
	// indicate which field is used to determine the state.
	//
	// See:
	//   https://docs.oasis-open.org/security/saml/v2.0/saml-bindings-2.0-os.pdf
	//   Section: "3.4.3 RelayState"
	state := r.URL.Query().Get("state")
	if state == "" {
		s.renderError(w, http.StatusBadRequest, "User session error.")
		return
	}

	authReq, err := s.storage.GetAuthRequest(state)
	if err != nil {
		if err == storage.ErrNotFound {
			s.logger.Errorf("Invalid 'state' parameter provided: %v", err)
			s.renderError(w, http.StatusInternalServerError, "Requested resource does not exist.")
			return
		}
		s.logger.Errorf("Failed to get auth request: %v", err)
		s.renderError(w, http.StatusInternalServerError, "Database error.")
		return
	}

	conn, ok := s.connectors[authReq.ConnectorID]
	if !ok {
		s.renderError(w, http.StatusInternalServerError, "Requested resource does not exist.")
		return
	}
	callbackConnector, ok := conn.Connector.(connector.CallbackConnector)
	if !ok {
		s.renderError(w, http.StatusInternalServerError, "Requested resource does not exist.")
		return
	}

	identity, err := callbackConnector.HandleCallback(parseScopes(authReq.Scopes), r)
	if err != nil {
		s.logger.Errorf("Failed to authenticate: %v", err)
		s.renderError(w, http.StatusInternalServerError, "Failed to return user's identity.")
		return
	}

	redirectURL, err := s.finalizeLogin(identity, authReq, conn.Connector)
	if err != nil {
		s.logger.Errorf("Failed to finalize login: %v", err)
		s.renderError(w, http.StatusInternalServerError, "Login error.")
		return
	}

	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

func (s *Server) finalizeLogin(identity connector.Identity, authReq storage.AuthRequest, conn connector.Connector) (string, error) {
	claims := storage.Claims{
		UserID:        identity.UserID,
		Username:      identity.Username,
		Email:         identity.Email,
		EmailVerified: identity.EmailVerified,
		Groups:        identity.Groups,
	}

	updater := func(a storage.AuthRequest) (storage.AuthRequest, error) {
		a.LoggedIn = true
		a.Claims = claims
		a.ConnectorData = identity.ConnectorData
		return a, nil
	}
	if err := s.storage.UpdateAuthRequest(authReq.ID, updater); err != nil {
		return "", fmt.Errorf("failed to update auth request: %v", err)
	}
	return path.Join(s.issuerURL.Path, "/approval") + "?req=" + authReq.ID, nil
}

func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	authReq, err := s.storage.GetAuthRequest(r.FormValue("req"))
	if err != nil {
		s.logger.Errorf("Failed to get auth request: %v", err)
		s.renderError(w, http.StatusInternalServerError, "Database error.")
		return
	}
	if !authReq.LoggedIn {
		s.logger.Errorf("Auth request does not have an identity for approval")
		s.renderError(w, http.StatusInternalServerError, "Login process not yet finalized.")
		return
	}

	switch r.Method {
	case "GET":
		if s.skipApproval {
			s.sendCodeResponse(w, r, authReq)
			return
		}
		client, err := s.storage.GetClient(authReq.ClientID)
		if err != nil {
			s.logger.Errorf("Failed to get client %q: %v", authReq.ClientID, err)
			s.renderError(w, http.StatusInternalServerError, "Failed to retrieve client.")
			return
		}
		if err := s.templates.approval(w, authReq.ID, authReq.Claims.Username, client.Name, authReq.Scopes); err != nil {
			s.logger.Errorf("Server template error: %v", err)
		}
	case "POST":
		if r.FormValue("approval") != "approve" {
			s.renderError(w, http.StatusInternalServerError, "Approval rejected.")
			return
		}
		s.sendCodeResponse(w, r, authReq)
	}
}

func (s *Server) sendCodeResponse(w http.ResponseWriter, r *http.Request, authReq storage.AuthRequest) {
	if s.now().After(authReq.Expiry) {
		s.renderError(w, http.StatusBadRequest, "User session has expired.")
		return
	}

	if err := s.storage.DeleteAuthRequest(authReq.ID); err != nil {
		if err != storage.ErrNotFound {
			s.logger.Errorf("Failed to delete authorization request: %v", err)
			s.renderError(w, http.StatusInternalServerError, "Internal server error.")
		} else {
			s.renderError(w, http.StatusBadRequest, "User session error.")
		}
		return
	}
	u, err := url.Parse(authReq.RedirectURI)
	if err != nil {
		s.renderError(w, http.StatusInternalServerError, "Invalid redirect URI.")
		return
	}
	q := u.Query()

	for _, responseType := range authReq.ResponseTypes {
		switch responseType {
		case responseTypeCode:
			code := storage.AuthCode{
				ID:            storage.NewID(),
				ClientID:      authReq.ClientID,
				ConnectorID:   authReq.ConnectorID,
				Nonce:         authReq.Nonce,
				Scopes:        authReq.Scopes,
				Claims:        authReq.Claims,
				Expiry:        s.now().Add(time.Minute * 30),
				RedirectURI:   authReq.RedirectURI,
				ConnectorData: authReq.ConnectorData,
			}
			if err := s.storage.CreateAuthCode(code); err != nil {
				s.logger.Errorf("Failed to create auth code: %v", err)
				s.renderError(w, http.StatusInternalServerError, "Internal server error.")
				return
			}

			if authReq.RedirectURI == redirectURIOOB {
				if err := s.templates.oob(w, code.ID); err != nil {
					s.logger.Errorf("Server template error: %v", err)
				}
				return
			}
			q.Set("code", code.ID)
		case responseTypeToken:
			idToken, expiry, err := s.newIDToken(authReq.ClientID, authReq.Claims, authReq.Scopes, authReq.Nonce)
			if err != nil {
				s.logger.Errorf("failed to create ID token: %v", err)
				s.tokenErrHelper(w, errServerError, "", http.StatusInternalServerError)
				return
			}
			v := url.Values{}
			v.Set("access_token", storage.NewID())
			v.Set("token_type", "bearer")
			v.Set("id_token", idToken)
			v.Set("state", authReq.State)
			v.Set("expires_in", strconv.Itoa(int(expiry.Sub(s.now()).Seconds())))
			u.Fragment = v.Encode()
		}
	}

	q.Set("state", authReq.State)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	clientID, clientSecret, ok := r.BasicAuth()
	if ok {
		var err error
		if clientID, err = url.QueryUnescape(clientID); err != nil {
			s.tokenErrHelper(w, errInvalidRequest, "client_id improperly encoded", http.StatusBadRequest)
			return
		}
		if clientSecret, err = url.QueryUnescape(clientSecret); err != nil {
			s.tokenErrHelper(w, errInvalidRequest, "client_secret improperly encoded", http.StatusBadRequest)
			return
		}
	} else {
		clientID = r.PostFormValue("client_id")
		clientSecret = r.PostFormValue("client_secret")
	}

	client, err := s.storage.GetClient(clientID)
	if err != nil {
		if err != storage.ErrNotFound {
			s.logger.Errorf("failed to get client: %v", err)
			s.tokenErrHelper(w, errServerError, "", http.StatusInternalServerError)
		} else {
			s.tokenErrHelper(w, errInvalidClient, "Invalid client credentials.", http.StatusUnauthorized)
		}
		return
	}
	if client.Secret != clientSecret {
		s.tokenErrHelper(w, errInvalidClient, "Invalid client credentials.", http.StatusUnauthorized)
		return
	}

	grantType := r.PostFormValue("grant_type")
	switch grantType {
	case grantTypeAuthorizationCode:
		s.handleAuthCode(w, r, client)
	case grantTypeRefreshToken:
		s.handleRefreshToken(w, r, client)
	default:
		s.tokenErrHelper(w, errInvalidGrant, "", http.StatusBadRequest)
	}
}

// handle an access token request https://tools.ietf.org/html/rfc6749#section-4.1.3
func (s *Server) handleAuthCode(w http.ResponseWriter, r *http.Request, client storage.Client) {
	code := r.PostFormValue("code")
	redirectURI := r.PostFormValue("redirect_uri")

	authCode, err := s.storage.GetAuthCode(code)
	if err != nil || s.now().After(authCode.Expiry) || authCode.ClientID != client.ID {
		if err != storage.ErrNotFound {
			s.logger.Errorf("failed to get auth code: %v", err)
			s.tokenErrHelper(w, errServerError, "", http.StatusInternalServerError)
		} else {
			s.tokenErrHelper(w, errInvalidRequest, "Invalid or expired code parameter.", http.StatusBadRequest)
		}
		return
	}

	if authCode.RedirectURI != redirectURI {
		s.tokenErrHelper(w, errInvalidRequest, "redirect_uri did not match URI from initial request.", http.StatusBadRequest)
		return
	}

	idToken, expiry, err := s.newIDToken(client.ID, authCode.Claims, authCode.Scopes, authCode.Nonce)
	if err != nil {
		s.logger.Errorf("failed to create ID token: %v", err)
		s.tokenErrHelper(w, errServerError, "", http.StatusInternalServerError)
		return
	}

	if err := s.storage.DeleteAuthCode(code); err != nil {
		s.logger.Errorf("failed to delete auth code: %v", err)
		s.tokenErrHelper(w, errServerError, "", http.StatusInternalServerError)
		return
	}

	reqRefresh := func() bool {
		for _, scope := range authCode.Scopes {
			if scope == scopeOfflineAccess {
				return true
			}
		}
		return false
	}()
	var refreshToken string
	if reqRefresh {
		refresh := storage.RefreshToken{
			RefreshToken:  storage.NewID(),
			ClientID:      authCode.ClientID,
			ConnectorID:   authCode.ConnectorID,
			Scopes:        authCode.Scopes,
			Claims:        authCode.Claims,
			Nonce:         authCode.Nonce,
			ConnectorData: authCode.ConnectorData,
		}
		if err := s.storage.CreateRefresh(refresh); err != nil {
			s.logger.Errorf("failed to create refresh token: %v", err)
			s.tokenErrHelper(w, errServerError, "", http.StatusInternalServerError)
			return
		}
		refreshToken = refresh.RefreshToken
	}
	s.writeAccessToken(w, idToken, refreshToken, expiry)
}

// handle a refresh token request https://tools.ietf.org/html/rfc6749#section-6
func (s *Server) handleRefreshToken(w http.ResponseWriter, r *http.Request, client storage.Client) {
	code := r.PostFormValue("refresh_token")
	scope := r.PostFormValue("scope")
	if code == "" {
		s.tokenErrHelper(w, errInvalidRequest, "No refresh token in request.", http.StatusBadRequest)
		return
	}

	refresh, err := s.storage.GetRefresh(code)
	if err != nil || refresh.ClientID != client.ID {
		if err != storage.ErrNotFound {
			s.logger.Errorf("failed to get auth code: %v", err)
			s.tokenErrHelper(w, errServerError, "", http.StatusInternalServerError)
		} else {
			s.tokenErrHelper(w, errInvalidRequest, "Refresh token is invalid or has already been claimed by another client.", http.StatusBadRequest)
		}
		return
	}

	// Per the OAuth2 spec, if the client has omitted the scopes, default to the original
	// authorized scopes.
	//
	// https://tools.ietf.org/html/rfc6749#section-6
	scopes := refresh.Scopes
	if scope != "" {
		requestedScopes := strings.Fields(scope)
		var unauthorizedScopes []string

		for _, s := range requestedScopes {
			contains := func() bool {
				for _, scope := range refresh.Scopes {
					if s == scope {
						return true
					}
				}
				return false
			}()
			if !contains {
				unauthorizedScopes = append(unauthorizedScopes, s)
			}
		}

		if len(unauthorizedScopes) > 0 {
			msg := fmt.Sprintf("Requested scopes contain unauthorized scope(s): %q.", unauthorizedScopes)
			s.tokenErrHelper(w, errInvalidRequest, msg, http.StatusBadRequest)
			return
		}
		scopes = requestedScopes
	}

	conn, ok := s.connectors[refresh.ConnectorID]
	if !ok {
		s.logger.Errorf("connector ID not found: %q", refresh.ConnectorID)
		s.tokenErrHelper(w, errServerError, "", http.StatusInternalServerError)
		return
	}

	// Can the connector refresh the identity? If so, attempt to refresh the data
	// in the connector.
	//
	// TODO(ericchiang): We may want a strict mode where connectors that don't implement
	// this interface can't perform refreshing.
	if refreshConn, ok := conn.Connector.(connector.RefreshConnector); ok {
		ident := connector.Identity{
			UserID:        refresh.Claims.UserID,
			Username:      refresh.Claims.Username,
			Email:         refresh.Claims.Email,
			EmailVerified: refresh.Claims.EmailVerified,
			Groups:        refresh.Claims.Groups,
			ConnectorData: refresh.ConnectorData,
		}
		ident, err := refreshConn.Refresh(r.Context(), parseScopes(scopes), ident)
		if err != nil {
			s.logger.Errorf("failed to refresh identity: %v", err)
			s.tokenErrHelper(w, errServerError, "", http.StatusInternalServerError)
			return
		}

		// Update the claims of the refresh token.
		//
		// UserID intentionally ignored for now.
		refresh.Claims.Username = ident.Username
		refresh.Claims.Email = ident.Email
		refresh.Claims.EmailVerified = ident.EmailVerified
		refresh.Claims.Groups = ident.Groups
		refresh.ConnectorData = ident.ConnectorData
	}

	idToken, expiry, err := s.newIDToken(client.ID, refresh.Claims, scopes, refresh.Nonce)
	if err != nil {
		s.logger.Errorf("failed to create ID token: %v", err)
		s.tokenErrHelper(w, errServerError, "", http.StatusInternalServerError)
		return
	}

	// Refresh tokens are claimed exactly once. Delete the current token and
	// create a new one.
	if err := s.storage.DeleteRefresh(code); err != nil {
		s.logger.Errorf("failed to delete auth code: %v", err)
		s.tokenErrHelper(w, errServerError, "", http.StatusInternalServerError)
		return
	}
	refresh.RefreshToken = storage.NewID()
	if err := s.storage.CreateRefresh(refresh); err != nil {
		s.logger.Errorf("failed to create refresh token: %v", err)
		s.tokenErrHelper(w, errServerError, "", http.StatusInternalServerError)
		return
	}
	s.writeAccessToken(w, idToken, refresh.RefreshToken, expiry)
}

func (s *Server) writeAccessToken(w http.ResponseWriter, idToken, refreshToken string, expiry time.Time) {
	// TODO(ericchiang): figure out an access token story and support the user info
	// endpoint. For now use a random value so no one depends on the access_token
	// holding a specific structure.
	resp := struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		RefreshToken string `json:"refresh_token,omitempty"`
		IDToken      string `json:"id_token"`
	}{
		storage.NewID(),
		"bearer",
		int(expiry.Sub(s.now()).Seconds()),
		refreshToken,
		idToken,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		s.logger.Errorf("failed to marshal access token response: %v", err)
		s.tokenErrHelper(w, errServerError, "", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

func (s *Server) renderError(w http.ResponseWriter, status int, description string) {
	if err := s.templates.err(w, http.StatusText(status), description); err != nil {
		s.logger.Errorf("Server template error: %v", err)
	}
}

func (s *Server) tokenErrHelper(w http.ResponseWriter, typ string, description string, statusCode int) {
	if err := tokenErr(w, typ, description, statusCode); err != nil {
		s.logger.Errorf("token error repsonse: %v", err)
	}
}
