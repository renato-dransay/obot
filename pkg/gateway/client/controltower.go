package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	types2 "github.com/obot-platform/obot/apiclient/types"
	"github.com/obot-platform/obot/pkg/gateway/types"
)

const (
	controlTowerAuthProviderNamespace = "control-tower"
	controlTowerAuthProviderName      = "control-tower"
	controlTowerCredentialContext     = "control-tower/principals"
	controlTowerCredentialTokenKey    = "token"
)

type ControlTowerPrincipalCredential struct {
	Subject    string `json:"subject"`
	Token      string `json:"token"`
	ObotUserID string `json:"obotUserId"`
}

func controlTowerPrincipalID(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return "ct-" + hex.EncodeToString(sum[:])[:32]
}

func controlTowerCredentialName(subject string) string {
	return controlTowerPrincipalID(subject)
}

func (c *Client) ProvisionControlTowerPrincipal(ctx context.Context, subject string) (*ControlTowerPrincipalCredential, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return nil, fmt.Errorf("subject is required")
	}

	providerID := controlTowerPrincipalID(subject)
	user, err := c.EnsureIdentityWithRole(ctx, &types.Identity{
		AuthProviderNamespace: controlTowerAuthProviderNamespace,
		AuthProviderName:      controlTowerAuthProviderName,
		ProviderUserID:        providerID,
		ProviderUsername:      providerID,
		Email:                 providerID + "@control-tower.internal",
	}, "UTC", types2.RoleBasic)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure control tower identity: %w", err)
	}

	name := controlTowerCredentialName(subject)
	credential, err := c.RevealCredential(ctx, []string{controlTowerCredentialContext}, name)
	if err == nil {
		token := credential.Secrets[controlTowerCredentialTokenKey]
		if token != "" {
			return &ControlTowerPrincipalCredential{
				Subject:    subject,
				Token:      token,
				ObotUserID: fmt.Sprint(user.ID),
			}, nil
		}
	} else if !isCredentialNotFound(err) {
		return nil, fmt.Errorf("failed to reveal control tower credential: %w", err)
	}

	created, err := c.CreateAPIKey(ctx, user.ID, "Control Tower MCP runtime", "Provisioned by Control Tower for MCP runtime access", nil, types.APIKeyScopes{
		MCPServerIDs: []string{"*"},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create control tower API key: %w", err)
	}

	if err := c.UpsertCredential(ctx, types.Credential{
		Context: controlTowerCredentialContext,
		Name:    name,
		Secrets: map[string]string{
			controlTowerCredentialTokenKey: created.Key,
		},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		return nil, fmt.Errorf("failed to store control tower credential: %w", err)
	}

	return &ControlTowerPrincipalCredential{
		Subject:    subject,
		Token:      created.Key,
		ObotUserID: fmt.Sprint(user.ID),
	}, nil
}

func isCredentialNotFound(err error) bool {
	_, ok := err.(CredentialNotFoundError)
	return ok
}
