package metadata

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/3scale-labs/authorino/pkg/common"
)

type providerJSON struct {
	Issuer                  string `json:"issuer"`
	TokenURL                string `json:"token_endpoint"`
	ResourceRegistrationURL string `json:"resource_registration_endpoint"`
}

type Provider struct {
	issuer                  string
	tokenURL                string
	resourceRegistrationURL string

	// Raw claims returned by the server.
	rawClaims []byte
}

func (provider *Provider) GetTokenURL() string {
	return provider.tokenURL
}

func (provider *Provider) GetResourcesByURI(uri string, pat PAT, ctx context.Context) ([]interface{}, error) {
	if err := common.CheckContext(ctx); err != nil {
		return nil, err
	}

	resourceIDs, err := provider.queryResourcesByURI(uri, pat, ctx)
	if err != nil {
		return nil, err
	}
	return provider.getResourcesByIDs(resourceIDs, pat, ctx)
}

func (provider *Provider) queryResourcesByURI(uri string, pat PAT, ctx context.Context) ([]string, error) {
	if err := common.CheckContext(ctx); err != nil {
		return nil, err
	}

	queryResourcesURL, _ := url.Parse(provider.resourceRegistrationURL)
	queryResourcesURL.RawQuery = "uri=" + uri
	var resourceIDs []string
	if err := sendRequestWithPAT(queryResourcesURL.String(), pat, ctx, &resourceIDs); err != nil {
		return nil, err
	}
	return resourceIDs, nil
}

func (provider *Provider) getResourcesByIDs(resourceIDs []string, pat PAT, ctx context.Context) ([]interface{}, error) {
	if err := common.CheckContext(ctx); err != nil {
		return nil, err
	}

	waitGroup := new(sync.WaitGroup)
	size := len(resourceIDs)
	buf := make(chan interface{}, size)

	waitGroup.Add(size)
	for _, resourceID := range resourceIDs {
		go func(id string) {
			defer waitGroup.Done()

			if data, err := provider.getResourceByID(id, pat, ctx); err == nil {
				buf <- data
			}
		}(resourceID)
	}

	waitGroup.Wait()
	close(buf)

	resourceData := make([]interface{}, 0)
	for resource := range buf {
		resourceData = append(resourceData, resource)
	}
	return resourceData, nil
}

func (provider *Provider) getResourceByID(resourceID string, pat PAT, ctx context.Context) (interface{}, error) {
	if err := common.CheckContext(ctx); err != nil {
		return nil, err
	}

	resourceURL, _ := url.Parse(provider.resourceRegistrationURL)
	resourceURL.Path += "/" + resourceID
	var data interface{}
	if err := sendRequestWithPAT(resourceURL.String(), pat, ctx, &data); err != nil {
		return nil, err
	}
	return data, nil
}

type PAT struct {
	AccessToken string `json:"access_token"`
}

func (pat *PAT) String() string {
	return pat.AccessToken
}

type UMA struct {
	Endpoint     string `yaml:"endpoint,omitempty"`
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
}

// NewProvider discovers the uma config and returns a Provider struct with its claims
func (uma *UMA) NewProvider(ctx context.Context) (*Provider, error) {
	if err := common.CheckContext(ctx); err != nil {
		return nil, err
	}

	// discover uma config
	wellKnownURL := strings.TrimSuffix(uma.Endpoint, "/") + "/.well-known/uma2-configuration"
	req, err := http.NewRequestWithContext(ctx, "GET", wellKnownURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var p providerJSON
	var rawClaims []byte
	err = common.UnmashalJSONResponse(resp, &p, &rawClaims)
	if err != nil {
		return nil, fmt.Errorf("uma: failed to decode provider discovery object: %v", err)
	}

	// verify same issuer
	if p.Issuer != uma.Endpoint {
		return nil, fmt.Errorf("uma: issuer did not match the issuer returned by provider, expected %q got %q", uma.Endpoint, p.Issuer)
	}

	return &Provider{
		issuer:                  p.Issuer,
		tokenURL:                p.TokenURL,
		resourceRegistrationURL: p.ResourceRegistrationURL,
		rawClaims:               rawClaims,
	}, nil
}

func (uma *UMA) Call(authContext common.AuthContext, ctx context.Context) (interface{}, error) {
	// discover uma config
	// TODO: Move to a 'prepare' step and cache it (like in pkg/config/authorization/opa.go)
	provider, err := uma.NewProvider(ctx)
	if err != nil {
		return nil, err
	}

	// get the protection API token (PAT)
	var pat PAT
	if err := uma.requestPAT(ctx, provider, &pat); err != nil {
		return nil, err
	}

	// get resource data
	uri := authContext.GetRequest().Attributes.Request.Http.GetPath()
	resourceData, err := provider.GetResourcesByURI(uri, pat, ctx)
	if err != nil {
		return nil, err
	}

	return resourceData, nil
}

func (uma *UMA) clientAuthenticatedURL(rawurl string) (*url.URL, error) {
	parsedURL, err := url.Parse(rawurl)
	if err != nil {
		return nil, err
	}
	parsedURL.User = url.UserPassword(uma.ClientID, uma.ClientSecret)
	return parsedURL, nil
}

func (uma *UMA) requestPAT(ctx context.Context, provider *Provider, pat *PAT) error {
	if err := common.CheckContext(ctx); err != nil {
		return err
	}

	// build the request
	tokenURL, _ := uma.clientAuthenticatedURL(provider.GetTokenURL())
	data := url.Values{"grant_type": {"client_credentials"}}
	encodedData := bytes.NewBufferString(data.Encode())
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL.String(), encodedData)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// get the response
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	// parse the pat
	err = common.UnmashalJSONResponse(resp, pat, nil)
	if err != nil {
		return fmt.Errorf("uma: failed to decode PAT: %v", err)
	}

	return nil
}

func sendRequestWithPAT(rawurl string, pat PAT, ctx context.Context, v interface{}) error {
	if err := common.CheckContext(ctx); err != nil {
		return err
	}

	// build the request
	req, err := http.NewRequestWithContext(ctx, "GET", rawurl, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+pat.String())

	// get the response
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return common.UnmashalJSONResponse(resp, &v, nil)
}