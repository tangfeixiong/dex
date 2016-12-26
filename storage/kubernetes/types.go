package kubernetes

import (
	"strings"
	"time"

	jose "gopkg.in/square/go-jose.v2"

	"github.com/coreos/dex/storage"
	"github.com/coreos/dex/storage/kubernetes/k8sapi"
)

var tprMeta = k8sapi.TypeMeta{
	APIVersion: "extensions/v1beta1",
	Kind:       "ThirdPartyResource",
}

// The set of third party resources required by the storage. These are managed by
// the storage so it can migrate itself by creating new resources.
var thirdPartyResources = []k8sapi.ThirdPartyResource{
	{
		ObjectMeta: k8sapi.ObjectMeta{
			Name: "auth-code.oidc.coreos.com",
		},
		TypeMeta:    tprMeta,
		Description: "A code which can be claimed for an access token.",
		Versions:    []k8sapi.APIVersion{{Name: "v1"}},
	},
	{
		ObjectMeta: k8sapi.ObjectMeta{
			Name: "auth-request.oidc.coreos.com",
		},
		TypeMeta:    tprMeta,
		Description: "A request for an end user to authorize a client.",
		Versions:    []k8sapi.APIVersion{{Name: "v1"}},
	},
	{
		ObjectMeta: k8sapi.ObjectMeta{
			Name: "o-auth2-client.oidc.coreos.com",
		},
		TypeMeta:    tprMeta,
		Description: "An OpenID Connect client.",
		Versions:    []k8sapi.APIVersion{{Name: "v1"}},
	},
	{
		ObjectMeta: k8sapi.ObjectMeta{
			Name: "signing-key.oidc.coreos.com",
		},
		TypeMeta:    tprMeta,
		Description: "Keys used to sign and verify OpenID Connect tokens.",
		Versions:    []k8sapi.APIVersion{{Name: "v1"}},
	},
	{
		ObjectMeta: k8sapi.ObjectMeta{
			Name: "refresh-token.oidc.coreos.com",
		},
		TypeMeta:    tprMeta,
		Description: "Refresh tokens for clients to continuously act on behalf of an end user.",
		Versions:    []k8sapi.APIVersion{{Name: "v1"}},
	},
	{
		ObjectMeta: k8sapi.ObjectMeta{
			Name: "password.oidc.coreos.com",
		},
		TypeMeta:    tprMeta,
		Description: "Passwords managed by the OIDC server.",
		Versions:    []k8sapi.APIVersion{{Name: "v1"}},
	},
}

// There will only ever be a single keys resource. Maintain this by setting a
// common name.
const keysName = "openid-connect-keys"

// Client is a mirrored struct from storage with JSON struct tags and
// Kubernetes type metadata.
type Client struct {
	// Name is a hash of the ID.
	k8sapi.TypeMeta   `json:",inline"`
	k8sapi.ObjectMeta `json:"metadata,omitempty"`

	// ID is immutable, since it's a primary key and should not be changed.
	ID string `json:"id,omitempty"`

	Secret       string   `json:"secret,omitempty"`
	RedirectURIs []string `json:"redirectURIs,omitempty"`
	TrustedPeers []string `json:"trustedPeers,omitempty"`

	Public bool `json:"public"`

	Name    string `json:"name,omitempty"`
	LogoURL string `json:"logoURL,omitempty"`
}

// ClientList is a list of Clients.
type ClientList struct {
	k8sapi.TypeMeta `json:",inline"`
	k8sapi.ListMeta `json:"metadata,omitempty"`
	Clients         []Client `json:"items"`
}

func (cli *client) fromStorageClient(c storage.Client) Client {
	return Client{
		TypeMeta: k8sapi.TypeMeta{
			Kind:       kindClient,
			APIVersion: cli.apiVersion,
		},
		ObjectMeta: k8sapi.ObjectMeta{
			Name:      cli.idToName(c.ID),
			Namespace: cli.namespace,
		},
		ID:           c.ID,
		Secret:       c.Secret,
		RedirectURIs: c.RedirectURIs,
		TrustedPeers: c.TrustedPeers,
		Public:       c.Public,
		Name:         c.Name,
		LogoURL:      c.LogoURL,
	}
}

func toStorageClient(c Client) storage.Client {
	return storage.Client{
		ID:           c.ID,
		Secret:       c.Secret,
		RedirectURIs: c.RedirectURIs,
		TrustedPeers: c.TrustedPeers,
		Public:       c.Public,
		Name:         c.Name,
		LogoURL:      c.LogoURL,
	}
}

// Claims is a mirrored struct from storage with JSON struct tags.
type Claims struct {
	UserID        string   `json:"userID"`
	Username      string   `json:"username"`
	Email         string   `json:"email"`
	EmailVerified bool     `json:"emailVerified"`
	Groups        []string `json:"groups,omitempty"`
}

func fromStorageClaims(i storage.Claims) Claims {
	return Claims{
		UserID:        i.UserID,
		Username:      i.Username,
		Email:         i.Email,
		EmailVerified: i.EmailVerified,
		Groups:        i.Groups,
	}
}

func toStorageClaims(i Claims) storage.Claims {
	return storage.Claims{
		UserID:        i.UserID,
		Username:      i.Username,
		Email:         i.Email,
		EmailVerified: i.EmailVerified,
		Groups:        i.Groups,
	}
}

// AuthRequest is a mirrored struct from storage with JSON struct tags and
// Kubernetes type metadata.
type AuthRequest struct {
	k8sapi.TypeMeta   `json:",inline"`
	k8sapi.ObjectMeta `json:"metadata,omitempty"`

	ClientID      string   `json:"clientID"`
	ResponseTypes []string `json:"responseTypes,omitempty"`
	Scopes        []string `json:"scopes,omitempty"`
	RedirectURI   string   `json:"redirectURI"`

	Nonce string `json:"nonce,omitempty"`
	State string `json:"state,omitempty"`

	// The client has indicated that the end user must be shown an approval prompt
	// on all requests. The server cannot cache their initial action for subsequent
	// attempts.
	ForceApprovalPrompt bool `json:"forceApprovalPrompt,omitempty"`

	LoggedIn bool `json:"loggedIn"`

	// The identity of the end user. Generally nil until the user authenticates
	// with a backend.
	Claims Claims `json:"claims,omitempty"`
	// The connector used to login the user. Set when the user authenticates.
	ConnectorID   string `json:"connectorID,omitempty"`
	ConnectorData []byte `json:"connectorData,omitempty"`

	Expiry time.Time `json:"expiry"`
}

// AuthRequestList is a list of AuthRequests.
type AuthRequestList struct {
	k8sapi.TypeMeta `json:",inline"`
	k8sapi.ListMeta `json:"metadata,omitempty"`
	AuthRequests    []AuthRequest `json:"items"`
}

func toStorageAuthRequest(req AuthRequest) storage.AuthRequest {
	a := storage.AuthRequest{
		ID:                  req.ObjectMeta.Name,
		ClientID:            req.ClientID,
		ResponseTypes:       req.ResponseTypes,
		Scopes:              req.Scopes,
		RedirectURI:         req.RedirectURI,
		Nonce:               req.Nonce,
		State:               req.State,
		ForceApprovalPrompt: req.ForceApprovalPrompt,
		LoggedIn:            req.LoggedIn,
		ConnectorID:         req.ConnectorID,
		ConnectorData:       req.ConnectorData,
		Expiry:              req.Expiry,
		Claims:              toStorageClaims(req.Claims),
	}
	return a
}

func (cli *client) fromStorageAuthRequest(a storage.AuthRequest) AuthRequest {
	req := AuthRequest{
		TypeMeta: k8sapi.TypeMeta{
			Kind:       kindAuthRequest,
			APIVersion: cli.apiVersion,
		},
		ObjectMeta: k8sapi.ObjectMeta{
			Name:      a.ID,
			Namespace: cli.namespace,
		},
		ClientID:            a.ClientID,
		ResponseTypes:       a.ResponseTypes,
		Scopes:              a.Scopes,
		RedirectURI:         a.RedirectURI,
		Nonce:               a.Nonce,
		State:               a.State,
		LoggedIn:            a.LoggedIn,
		ForceApprovalPrompt: a.ForceApprovalPrompt,
		ConnectorID:         a.ConnectorID,
		ConnectorData:       a.ConnectorData,
		Expiry:              a.Expiry,
		Claims:              fromStorageClaims(a.Claims),
	}
	return req
}

// Password is a mirrored struct from the stroage with JSON struct tags and
// Kubernetes type metadata.
type Password struct {
	k8sapi.TypeMeta   `json:",inline"`
	k8sapi.ObjectMeta `json:"metadata,omitempty"`

	// The Kubernetes name is actually an encoded version of this value.
	//
	// This field is IMMUTABLE. Do not change.
	Email string `json:"email,omitempty"`

	Hash     []byte `json:"hash,omitempty"`
	Username string `json:"username,omitempty"`
	UserID   string `json:"userID,omitempty"`
}

// PasswordList is a list of Passwords.
type PasswordList struct {
	k8sapi.TypeMeta `json:",inline"`
	k8sapi.ListMeta `json:"metadata,omitempty"`
	Passwords       []Password `json:"items"`
}

func (cli *client) fromStoragePassword(p storage.Password) Password {
	email := strings.ToLower(p.Email)
	return Password{
		TypeMeta: k8sapi.TypeMeta{
			Kind:       kindPassword,
			APIVersion: cli.apiVersion,
		},
		ObjectMeta: k8sapi.ObjectMeta{
			Name:      cli.idToName(email),
			Namespace: cli.namespace,
		},
		Email:    email,
		Hash:     p.Hash,
		Username: p.Username,
		UserID:   p.UserID,
	}
}

func toStoragePassword(p Password) storage.Password {
	return storage.Password{
		Email:    p.Email,
		Hash:     p.Hash,
		Username: p.Username,
		UserID:   p.UserID,
	}
}

// AuthCode is a mirrored struct from storage with JSON struct tags and
// Kubernetes type metadata.
type AuthCode struct {
	k8sapi.TypeMeta   `json:",inline"`
	k8sapi.ObjectMeta `json:"metadata,omitempty"`

	ClientID    string   `json:"clientID"`
	Scopes      []string `json:"scopes,omitempty"`
	RedirectURI string   `json:"redirectURI"`

	Nonce string `json:"nonce,omitempty"`
	State string `json:"state,omitempty"`

	Claims Claims `json:"claims,omitempty"`

	ConnectorID   string `json:"connectorID,omitempty"`
	ConnectorData []byte `json:"connectorData,omitempty"`

	Expiry time.Time `json:"expiry"`
}

// AuthCodeList is a list of AuthCodes.
type AuthCodeList struct {
	k8sapi.TypeMeta `json:",inline"`
	k8sapi.ListMeta `json:"metadata,omitempty"`
	AuthCodes       []AuthCode `json:"items"`
}

func (cli *client) fromStorageAuthCode(a storage.AuthCode) AuthCode {
	return AuthCode{
		TypeMeta: k8sapi.TypeMeta{
			Kind:       kindAuthCode,
			APIVersion: cli.apiVersion,
		},
		ObjectMeta: k8sapi.ObjectMeta{
			Name:      a.ID,
			Namespace: cli.namespace,
		},
		ClientID:      a.ClientID,
		RedirectURI:   a.RedirectURI,
		ConnectorID:   a.ConnectorID,
		ConnectorData: a.ConnectorData,
		Nonce:         a.Nonce,
		Scopes:        a.Scopes,
		Claims:        fromStorageClaims(a.Claims),
		Expiry:        a.Expiry,
	}
}

func toStorageAuthCode(a AuthCode) storage.AuthCode {
	return storage.AuthCode{
		ID:            a.ObjectMeta.Name,
		ClientID:      a.ClientID,
		RedirectURI:   a.RedirectURI,
		ConnectorID:   a.ConnectorID,
		ConnectorData: a.ConnectorData,
		Nonce:         a.Nonce,
		Scopes:        a.Scopes,
		Claims:        toStorageClaims(a.Claims),
		Expiry:        a.Expiry,
	}
}

// RefreshToken is a mirrored struct from storage with JSON struct tags and
// Kubernetes type metadata.
type RefreshToken struct {
	k8sapi.TypeMeta   `json:",inline"`
	k8sapi.ObjectMeta `json:"metadata,omitempty"`

	ClientID string   `json:"clientID"`
	Scopes   []string `json:"scopes,omitempty"`

	Nonce string `json:"nonce,omitempty"`

	Claims      Claims `json:"claims,omitempty"`
	ConnectorID string `json:"connectorID,omitempty"`
}

// RefreshList is a list of refresh tokens.
type RefreshList struct {
	k8sapi.TypeMeta `json:",inline"`
	k8sapi.ListMeta `json:"metadata,omitempty"`
	RefreshTokens   []RefreshToken `json:"items"`
}

// Keys is a mirrored struct from storage with JSON struct tags and Kubernetes
// type metadata.
type Keys struct {
	k8sapi.TypeMeta   `json:",inline"`
	k8sapi.ObjectMeta `json:"metadata,omitempty"`

	// Key for creating and verifying signatures. These may be nil.
	SigningKey    *jose.JSONWebKey `json:"signingKey,omitempty"`
	SigningKeyPub *jose.JSONWebKey `json:"signingKeyPub,omitempty"`
	// Old signing keys which have been rotated but can still be used to validate
	// existing signatures.
	VerificationKeys []storage.VerificationKey `json:"verificationKeys,omitempty"`

	// The next time the signing key will rotate.
	//
	// For caching purposes, implementations MUST NOT update keys before this time.
	NextRotation time.Time `json:"nextRotation"`
}

func (cli *client) fromStorageKeys(keys storage.Keys) Keys {
	return Keys{
		TypeMeta: k8sapi.TypeMeta{
			Kind:       kindKeys,
			APIVersion: cli.apiVersion,
		},
		ObjectMeta: k8sapi.ObjectMeta{
			Name:      keysName,
			Namespace: cli.namespace,
		},
		SigningKey:       keys.SigningKey,
		SigningKeyPub:    keys.SigningKeyPub,
		VerificationKeys: keys.VerificationKeys,
		NextRotation:     keys.NextRotation,
	}
}

func toStorageKeys(keys Keys) storage.Keys {
	return storage.Keys{
		SigningKey:       keys.SigningKey,
		SigningKeyPub:    keys.SigningKeyPub,
		VerificationKeys: keys.VerificationKeys,
		NextRotation:     keys.NextRotation,
	}
}
