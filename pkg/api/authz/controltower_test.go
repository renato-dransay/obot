package authz

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/obot-platform/obot/apiclient/types"
	"k8s.io/apiserver/pkg/authentication/user"
)

func TestControlTowerPrincipalProvisioningAuthorization(t *testing.T) {
	authorizer := NewAuthorizer(nil, nil, nil, false, nil, nil, false)
	request := httptest.NewRequest(http.MethodPost, "/api/control-tower/principals", nil)
	tests := []struct {
		name    string
		groups  []string
		allowed bool
	}{
		{name: "owner", groups: types.RoleOwner.Groups(), allowed: true},
		{name: "admin", groups: types.RoleAdmin.Groups(), allowed: true},
		{name: "basic", groups: types.RoleBasic.Groups(), allowed: false},
		{name: "anonymous", groups: []string{UnauthenticatedGroup}, allowed: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			allowed := authorizer.Authorize(request, &user.DefaultInfo{
				Name:   test.name,
				Groups: test.groups,
			})
			if allowed != test.allowed {
				t.Fatalf("expected allowed=%t, got %t", test.allowed, allowed)
			}
		})
	}
}
