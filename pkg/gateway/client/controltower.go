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
	"gorm.io/gorm/clause"
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
	if credential, ok, err := c.revealControlTowerPrincipalCredential(ctx, subject, name, user.ID); err != nil {
		return nil, err
	} else if ok {
		return credential, nil
	}

	reserved, err := c.reserveControlTowerCredential(ctx, name)
	if err != nil {
		return nil, err
	}
	if !reserved {
		return c.waitForControlTowerPrincipalCredential(ctx, subject, name, user.ID)
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

func (c *Client) reserveControlTowerCredential(ctx context.Context, name string) (bool, error) {
	result := c.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "context"}, {Name: "name"}},
		DoNothing: true,
	}).Create(&types.Credential{
		Context:   controlTowerCredentialContext,
		Name:      name,
		Secrets:   map[string]string{},
		CreatedAt: time.Now().UTC(),
	})
	if result.Error != nil {
		return false, fmt.Errorf("failed to reserve control tower credential: %w", result.Error)
	}
	return result.RowsAffected == 1, nil
}

func (c *Client) waitForControlTowerPrincipalCredential(ctx context.Context, subject, name string, userID uint) (*ControlTowerPrincipalCredential, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		if credential, ok, err := c.revealControlTowerPrincipalCredential(ctx, subject, name, userID); err != nil {
			return nil, err
		} else if ok {
			return credential, nil
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for control tower credential: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (c *Client) revealControlTowerPrincipalCredential(ctx context.Context, subject, name string, userID uint) (*ControlTowerPrincipalCredential, bool, error) {
	credential, err := c.RevealCredential(ctx, []string{controlTowerCredentialContext}, name)
	if err != nil {
		if isCredentialNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to reveal control tower credential: %w", err)
	}
	token := credential.Secrets[controlTowerCredentialTokenKey]
	if token == "" {
		return nil, false, nil
	}
	return &ControlTowerPrincipalCredential{
		Subject:    subject,
		Token:      token,
		ObotUserID: fmt.Sprint(userID),
	}, true, nil
}

func isCredentialNotFound(err error) bool {
	_, ok := err.(CredentialNotFoundError)
	return ok
}
