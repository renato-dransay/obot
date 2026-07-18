package client

import (
	"context"
	"testing"

	types2 "github.com/obot-platform/obot/apiclient/types"
	"github.com/obot-platform/obot/pkg/gateway/types"
	v1 "github.com/obot-platform/obot/pkg/storage/apis/obot.obot.ai/v1"
	storagescheme "github.com/obot-platform/obot/pkg/storage/scheme"
	"github.com/obot-platform/obot/pkg/system"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newControlTowerTestClient(t *testing.T) *Client {
	t.Helper()

	c := newTestClient(t)
	c.storageClient = fake.NewClientBuilder().
		WithScheme(storagescheme.Scheme).
		WithObjects(&v1.UserDefaultRoleSetting{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: system.DefaultNamespace,
				Name:      system.DefaultRoleSettingName,
			},
			Spec: v1.UserDefaultRoleSettingSpec{Role: types2.RoleBasic},
		}).
		Build()
	return c
}

func TestProvisionControlTowerPrincipalCreatesDistinctBasicUsersAndTokens(t *testing.T) {
	ctx := context.Background()
	c := newControlTowerTestClient(t)

	first, err := c.ProvisionControlTowerPrincipal(ctx, "ct:user:alice")
	if err != nil {
		t.Fatalf("provision first principal: %v", err)
	}
	second, err := c.ProvisionControlTowerPrincipal(ctx, "ct:user:bob")
	if err != nil {
		t.Fatalf("provision second principal: %v", err)
	}

	if first.ObotUserID == second.ObotUserID {
		t.Fatalf("expected distinct users, got %q", first.ObotUserID)
	}
	if first.Token == "" || second.Token == "" || first.Token == second.Token {
		t.Fatalf("expected distinct non-empty tokens")
	}

	var users []types.User
	if err := c.db.WithContext(ctx).Find(&users).Error; err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}
	for i := range users {
		if err := c.decryptUser(ctx, &users[i]); err != nil {
			t.Fatalf("decrypt user: %v", err)
		}
		if users[i].Role != types2.RoleBasic {
			t.Fatalf("expected provisioned user role Basic, got %d", users[i].Role)
		}
	}
}

func TestProvisionControlTowerPrincipalIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := newControlTowerTestClient(t)

	first, err := c.ProvisionControlTowerPrincipal(ctx, "ct:user:alice")
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}
	second, err := c.ProvisionControlTowerPrincipal(ctx, " ct:user:alice ")
	if err != nil {
		t.Fatalf("second provision: %v", err)
	}

	if first.Token != second.Token {
		t.Fatalf("expected retry to return same token")
	}
	if first.ObotUserID != second.ObotUserID {
		t.Fatalf("expected retry to return same user")
	}

	var keyCount int64
	if err := c.db.WithContext(ctx).Model(&types.APIKey{}).Count(&keyCount).Error; err != nil {
		t.Fatalf("count API keys: %v", err)
	}
	if keyCount != 1 {
		t.Fatalf("expected one stored API key, got %d", keyCount)
	}
}

func TestProvisionControlTowerPrincipalRejectsBlankSubject(t *testing.T) {
	c := newControlTowerTestClient(t)
	if _, err := c.ProvisionControlTowerPrincipal(context.Background(), " \t "); err == nil {
		t.Fatal("expected blank subject to fail")
	}
}
