// SPDX-License-Identifier: MIT

package blanktrail

import (
	"context"
	"net/http"
)

// integrationKeyPath is where the service keeps the developer's key.
const integrationKeyPath = "/api/v1/settings/integration-key"

// IntegrationKey is the developer's key the service is currently stamped with,
// and whether it is stamped at all.
func (c *Client) IntegrationKey(ctx context.Context) (string, bool, error) {
	var out struct {
		IntegrationKey string `json:"integration_key"`
		Set            bool   `json:"set"`
	}
	if err := c.doJSON(ctx, http.MethodGet, integrationKeyPath, nil, &out); err != nil {
		return "", false, err
	}
	return out.IntegrationKey, out.Set, nil
}

// SetIntegrationKey stamps the developer's key into the service, or clears it
// when given the empty string.
//
// It is how a program says which developer's work brought the user here: the
// service carries the key on the requests that bind a licence, and the account
// behind it is credited there. Nothing about it travels in a link and nothing
// about it is a secret — it names an integrator, not a person.
//
// The service takes the value as it is given, so a program that stamps the
// wrong thing has said the wrong thing. What it must not do is stamp over
// another integrator's key, which is somebody else's credit; that judgement
// belongs to the caller and not here.
func (c *Client) SetIntegrationKey(ctx context.Context, key string) error {
	body := struct {
		IntegrationKey string `json:"integration_key"`
	}{IntegrationKey: key}
	return c.doJSON(ctx, http.MethodPut, integrationKeyPath, body, nil)
}
