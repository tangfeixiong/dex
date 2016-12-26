package kubernetes

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/context"

	"github.com/Sirupsen/logrus"
	"github.com/coreos/dex/storage"
	"github.com/coreos/dex/storage/kubernetes/k8sapi"
)

const (
	kindAuthCode     = "AuthCode"
	kindAuthRequest  = "AuthRequest"
	kindClient       = "OAuth2Client"
	kindRefreshToken = "RefreshToken"
	kindKeys         = "SigningKey"
	kindPassword     = "Password"
)

const (
	resourceAuthCode     = "authcodes"
	resourceAuthRequest  = "authrequests"
	resourceClient       = "oauth2clients"
	resourceRefreshToken = "refreshtokens"
	resourceKeys         = "signingkeies" // Kubernetes attempts to pluralize.
	resourcePassword     = "passwords"
)

// Config values for the Kubernetes storage type.
type Config struct {
	InCluster      bool   `json:"inCluster"`
	KubeConfigFile string `json:"kubeConfigFile"`
}

// Open returns a storage using Kubernetes third party resource.
func (c *Config) Open(logger logrus.FieldLogger) (storage.Storage, error) {
	cli, err := c.open(logger)
	if err != nil {
		return nil, err
	}
	return cli, nil
}

// open returns a client with no garbage collection.
func (c *Config) open(logger logrus.FieldLogger) (*client, error) {
	if c.InCluster && (c.KubeConfigFile != "") {
		return nil, errors.New("cannot specify both 'inCluster' and 'kubeConfigFile'")
	}
	if !c.InCluster && (c.KubeConfigFile == "") {
		return nil, errors.New("must specify either 'inCluster' or 'kubeConfigFile'")
	}

	var (
		cluster   k8sapi.Cluster
		user      k8sapi.AuthInfo
		namespace string
		err       error
	)
	if c.InCluster {
		cluster, user, namespace, err = inClusterConfig()
	} else {
		cluster, user, namespace, err = loadKubeConfig(c.KubeConfigFile)
	}
	if err != nil {
		return nil, err
	}

	cli, err := newClient(cluster, user, namespace, logger)
	if err != nil {
		return nil, fmt.Errorf("create client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Try to synchronously create the third party resources once. This doesn't mean
	// they'll immediately be available, but ensures that the client will actually try
	// once.
	if err := cli.createThirdPartyResources(); err != nil {
		logger.Errorf("failed creating third party resources: %v", err)
		go func() {
			for {
				if err := cli.createThirdPartyResources(); err != nil {
					logger.Errorf("failed creating third party resources: %v", err)
				} else {
					return
				}

				select {
				case <-ctx.Done():
					return
				case <-time.After(30 * time.Second):
				}
			}
		}()
	}

	// If the client is closed, stop trying to create third party resources.
	cli.cancel = cancel
	return cli, nil
}

// createThirdPartyResources attempts to create the third party resources dex
// requires or identifies that they're already enabled.
//
// Creating a third party resource does not mean that they'll be immediately available.
//
// TODO(ericchiang): Provide an option to wait for the third party resources
// to actually be available.
func (cli *client) createThirdPartyResources() error {
	for _, r := range thirdPartyResources {
		err := cli.postResource("extensions/v1beta1", "", "thirdpartyresources", r)
		if err != nil {
			if e, ok := err.(httpError); ok {
				if e.StatusCode() == http.StatusConflict {
					cli.logger.Errorf("third party resource already created %q", r.ObjectMeta.Name)
					continue
				}
			}
			return err
		}
		cli.logger.Errorf("create third party resource %q", r.ObjectMeta.Name)
	}
	return nil
}

func (cli *client) Close() error {
	if cli.cancel != nil {
		cli.cancel()
	}
	return nil
}

func (cli *client) CreateAuthRequest(a storage.AuthRequest) error {
	return cli.post(resourceAuthRequest, cli.fromStorageAuthRequest(a))
}

func (cli *client) CreateClient(c storage.Client) error {
	return cli.post(resourceClient, cli.fromStorageClient(c))
}

func (cli *client) CreateAuthCode(c storage.AuthCode) error {
	return cli.post(resourceAuthCode, cli.fromStorageAuthCode(c))
}

func (cli *client) CreatePassword(p storage.Password) error {
	return cli.post(resourcePassword, cli.fromStoragePassword(p))
}

func (cli *client) CreateRefresh(r storage.RefreshToken) error {
	refresh := RefreshToken{
		TypeMeta: k8sapi.TypeMeta{
			Kind:       kindRefreshToken,
			APIVersion: cli.apiVersion,
		},
		ObjectMeta: k8sapi.ObjectMeta{
			Name:      r.RefreshToken,
			Namespace: cli.namespace,
		},
		ClientID:    r.ClientID,
		ConnectorID: r.ConnectorID,
		Scopes:      r.Scopes,
		Nonce:       r.Nonce,
		Claims:      fromStorageClaims(r.Claims),
	}
	return cli.post(resourceRefreshToken, refresh)
}

func (cli *client) GetAuthRequest(id string) (storage.AuthRequest, error) {
	var req AuthRequest
	if err := cli.get(resourceAuthRequest, id, &req); err != nil {
		return storage.AuthRequest{}, err
	}
	return toStorageAuthRequest(req), nil
}

func (cli *client) GetAuthCode(id string) (storage.AuthCode, error) {
	var code AuthCode
	if err := cli.get(resourceAuthCode, id, &code); err != nil {
		return storage.AuthCode{}, err
	}
	return toStorageAuthCode(code), nil
}

func (cli *client) GetClient(id string) (storage.Client, error) {
	c, err := cli.getClient(id)
	if err != nil {
		return storage.Client{}, err
	}
	return toStorageClient(c), nil
}

func (cli *client) getClient(id string) (Client, error) {
	var c Client
	name := cli.idToName(id)
	if err := cli.get(resourceClient, name, &c); err != nil {
		return Client{}, err
	}
	if c.ID != id {
		return Client{}, fmt.Errorf("get client: ID %q mapped to client with ID %q", id, c.ID)
	}
	return c, nil
}

func (cli *client) GetPassword(email string) (storage.Password, error) {
	p, err := cli.getPassword(email)
	if err != nil {
		return storage.Password{}, err
	}
	return toStoragePassword(p), nil
}

func (cli *client) getPassword(email string) (Password, error) {
	// TODO(ericchiang): Figure out whose job it is to lowercase emails.
	email = strings.ToLower(email)
	var p Password
	name := cli.idToName(email)
	if err := cli.get(resourcePassword, name, &p); err != nil {
		return Password{}, err
	}
	if email != p.Email {
		return Password{}, fmt.Errorf("get email: email %q mapped to password with email %q", email, p.Email)
	}
	return p, nil
}

func (cli *client) GetKeys() (storage.Keys, error) {
	var keys Keys
	if err := cli.get(resourceKeys, keysName, &keys); err != nil {
		return storage.Keys{}, err
	}
	return toStorageKeys(keys), nil
}

func (cli *client) GetRefresh(id string) (storage.RefreshToken, error) {
	var r RefreshToken
	if err := cli.get(resourceRefreshToken, id, &r); err != nil {
		return storage.RefreshToken{}, err
	}
	return storage.RefreshToken{
		RefreshToken: r.ObjectMeta.Name,
		ClientID:     r.ClientID,
		ConnectorID:  r.ConnectorID,
		Scopes:       r.Scopes,
		Nonce:        r.Nonce,
		Claims:       toStorageClaims(r.Claims),
	}, nil
}

func (cli *client) ListClients() ([]storage.Client, error) {
	return nil, errors.New("not implemented")
}

func (cli *client) ListRefreshTokens() ([]storage.RefreshToken, error) {
	return nil, errors.New("not implemented")
}

func (cli *client) ListPasswords() (passwords []storage.Password, err error) {
	var passwordList PasswordList
	if err = cli.list(resourcePassword, &passwordList); err != nil {
		return passwords, fmt.Errorf("failed to list passwords: %v", err)
	}

	for _, password := range passwordList.Passwords {
		p := storage.Password{
			Email:    password.Email,
			Hash:     password.Hash,
			Username: password.Username,
			UserID:   password.UserID,
		}
		passwords = append(passwords, p)
	}

	return
}

func (cli *client) DeleteAuthRequest(id string) error {
	return cli.delete(resourceAuthRequest, id)
}

func (cli *client) DeleteAuthCode(code string) error {
	return cli.delete(resourceAuthCode, code)
}

func (cli *client) DeleteClient(id string) error {
	// Check for hash collition.
	c, err := cli.getClient(id)
	if err != nil {
		return err
	}
	return cli.delete(resourceClient, c.ObjectMeta.Name)
}

func (cli *client) DeleteRefresh(id string) error {
	return cli.delete(resourceRefreshToken, id)
}

func (cli *client) DeletePassword(email string) error {
	// Check for hash collition.
	p, err := cli.getPassword(email)
	if err != nil {
		return err
	}
	return cli.delete(resourcePassword, p.ObjectMeta.Name)
}

func (cli *client) UpdateClient(id string, updater func(old storage.Client) (storage.Client, error)) error {
	c, err := cli.getClient(id)
	if err != nil {
		return err
	}

	updated, err := updater(toStorageClient(c))
	if err != nil {
		return err
	}
	updated.ID = c.ID

	newClient := cli.fromStorageClient(updated)
	newClient.ObjectMeta = c.ObjectMeta
	return cli.put(resourceClient, c.ObjectMeta.Name, newClient)
}

func (cli *client) UpdatePassword(email string, updater func(old storage.Password) (storage.Password, error)) error {
	p, err := cli.getPassword(email)
	if err != nil {
		return err
	}

	updated, err := updater(toStoragePassword(p))
	if err != nil {
		return err
	}
	updated.Email = p.Email

	newPassword := cli.fromStoragePassword(updated)
	newPassword.ObjectMeta = p.ObjectMeta
	return cli.put(resourcePassword, p.ObjectMeta.Name, newPassword)
}

func (cli *client) UpdateKeys(updater func(old storage.Keys) (storage.Keys, error)) error {
	firstUpdate := false
	var keys Keys
	if err := cli.get(resourceKeys, keysName, &keys); err != nil {
		if err != storage.ErrNotFound {
			return err
		}
		firstUpdate = true
	}
	var oldKeys storage.Keys
	if !firstUpdate {
		oldKeys = toStorageKeys(keys)
	}

	updated, err := updater(oldKeys)
	if err != nil {
		return err
	}
	newKeys := cli.fromStorageKeys(updated)
	if firstUpdate {
		return cli.post(resourceKeys, newKeys)
	}
	newKeys.ObjectMeta = keys.ObjectMeta
	return cli.put(resourceKeys, keysName, newKeys)
}

func (cli *client) UpdateAuthRequest(id string, updater func(a storage.AuthRequest) (storage.AuthRequest, error)) error {
	var req AuthRequest
	err := cli.get(resourceAuthRequest, id, &req)
	if err != nil {
		return err
	}

	updated, err := updater(toStorageAuthRequest(req))
	if err != nil {
		return err
	}

	newReq := cli.fromStorageAuthRequest(updated)
	newReq.ObjectMeta = req.ObjectMeta
	return cli.put(resourceAuthRequest, id, newReq)
}

func (cli *client) GarbageCollect(now time.Time) (result storage.GCResult, err error) {
	var authRequests AuthRequestList
	if err := cli.list(resourceAuthRequest, &authRequests); err != nil {
		return result, fmt.Errorf("failed to list auth requests: %v", err)
	}

	var delErr error
	for _, authRequest := range authRequests.AuthRequests {
		if now.After(authRequest.Expiry) {
			if err := cli.delete(resourceAuthRequest, authRequest.ObjectMeta.Name); err != nil {
				cli.logger.Errorf("failed to delete auth request: %v", err)
				delErr = fmt.Errorf("failed to delete auth request: %v", err)
			}
			result.AuthRequests++
		}
	}
	if delErr != nil {
		return result, delErr
	}

	var authCodes AuthCodeList
	if err := cli.list(resourceAuthCode, &authCodes); err != nil {
		return result, fmt.Errorf("failed to list auth codes: %v", err)
	}

	for _, authCode := range authCodes.AuthCodes {
		if now.After(authCode.Expiry) {
			if err := cli.delete(resourceAuthCode, authCode.ObjectMeta.Name); err != nil {
				cli.logger.Errorf("failed to delete auth code %v", err)
				delErr = fmt.Errorf("failed to delete auth code: %v", err)
			}
			result.AuthCodes++
		}
	}
	return result, delErr
}
