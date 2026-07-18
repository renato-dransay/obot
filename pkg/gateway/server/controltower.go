package server

import (
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	types2 "github.com/obot-platform/obot/apiclient/types"
	"github.com/obot-platform/obot/pkg/api"
)

const controlTowerSubjectMaxLength = 512

type controlTowerPrincipalRequest struct {
	Subject string `json:"subject"`
}

func (s *Server) provisionControlTowerPrincipal(apiContext api.Context) error {
	if !apiContext.UserIsAdmin() && !apiContext.UserIsOwner() {
		return types2.NewErrHTTP(http.StatusForbidden, "control tower principal provisioning requires admin or owner")
	}

	var req controlTowerPrincipalRequest
	if err := apiContext.Read(&req); err != nil {
		return types2.NewErrHTTP(http.StatusBadRequest, "invalid request body")
	}

	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		return types2.NewErrHTTP(http.StatusBadRequest, "subject is required")
	}
	if !utf8.ValidString(subject) || len(subject) > controlTowerSubjectMaxLength {
		return types2.NewErrHTTP(http.StatusBadRequest, fmt.Sprintf("subject must be valid UTF-8 and at most %d bytes", controlTowerSubjectMaxLength))
	}

	credential, err := apiContext.GatewayClient.ProvisionControlTowerPrincipal(apiContext.Context(), subject)
	if err != nil {
		return types2.NewErrHTTP(http.StatusInternalServerError, "failed to provision control tower principal")
	}

	return apiContext.Write(credential)
}
