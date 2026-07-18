package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	types2 "github.com/obot-platform/obot/apiclient/types"
	"github.com/obot-platform/obot/pkg/api"
	"github.com/obot-platform/obot/pkg/gateway/client"
	gatewaydb "github.com/obot-platform/obot/pkg/gateway/db"
	v1 "github.com/obot-platform/obot/pkg/storage/apis/obot.obot.ai/v1"
	storagescheme "github.com/obot-platform/obot/pkg/storage/scheme"
	sservices "github.com/obot-platform/obot/pkg/storage/services"
	"github.com/obot-platform/obot/pkg/system"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/user"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newControlTowerServerTestClient(t *testing.T) *client.Client {
	t.Helper()

	services, err := sservices.New(sservices.Config{DSN: "sqlite://:memory:"})
	if err != nil {
		t.Fatalf("create storage services: %v", err)
	}
	db, err := gatewaydb.New(services.DB.DB, services.DB.SQLDB, true)
	if err != nil {
		t.Fatalf("create gateway db: %v", err)
	}
	if err := db.AutoMigrate(); err != nil {
		t.Fatalf("auto-migrate gateway db: %v", err)
	}
	storageClient := fake.NewClientBuilder().
		WithScheme(storagescheme.Scheme).
		WithObjects(&v1.UserDefaultRoleSetting{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: system.DefaultNamespace,
				Name:      system.DefaultRoleSettingName,
			},
			Spec: v1.UserDefaultRoleSettingSpec{Role: types2.RoleBasic},
		}).
		Build()
	return client.New(context.Background(), db, storageClient, nil, nil, nil, 0, 1, 0, 0, false)
}

func callProvisionControlTowerPrincipal(t *testing.T, groups []string, body string) (int, string, error) {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/api/control-tower/principals", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	ctx := api.Context{
		ResponseWriter: rec,
		Request:        req,
		GatewayClient:  newControlTowerServerTestClient(t),
		User: &user.DefaultInfo{
			UID:    "1",
			Name:   "admin",
			Groups: groups,
		},
	}
	err := (&Server{}).provisionControlTowerPrincipal(ctx)
	return rec.Code, rec.Body.String(), err
}

func TestProvisionControlTowerPrincipalRouteRequiresAdminOrOwner(t *testing.T) {
	_, _, err := callProvisionControlTowerPrincipal(t, []string{types2.GroupBasic, types2.GroupAuthenticated}, `{"subject":"ct:user:alice"}`)
	if err == nil || !strings.Contains(err.Error(), "requires admin or owner") {
		t.Fatalf("expected forbidden error, got %v", err)
	}
}

func TestProvisionControlTowerPrincipalRouteRejectsInvalidSubject(t *testing.T) {
	_, _, err := callProvisionControlTowerPrincipal(t, []string{types2.GroupAdmin, types2.GroupAuthenticated}, `{"subject":" "}`)
	if err == nil || !strings.Contains(err.Error(), "subject is required") {
		t.Fatalf("expected subject validation error, got %v", err)
	}
}

func TestProvisionControlTowerPrincipalRouteReturnsCredential(t *testing.T) {
	status, body, err := callProvisionControlTowerPrincipal(t, []string{types2.GroupAdmin, types2.GroupAuthenticated}, `{"subject":"ct:user:alice"}`)
	if err != nil {
		t.Fatalf("provision route: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", status, body)
	}
	if !strings.Contains(body, `"subject":"ct:user:alice"`) || !strings.Contains(body, `"token":"ok1-`) || !strings.Contains(body, `"obotUserId":"`) {
		t.Fatalf("unexpected response body: %s", body)
	}
}
