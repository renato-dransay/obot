package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/obot-platform/obot/apiclient/types"
)

func TestNewRailwayBackendValidatesConfiguration(t *testing.T) {
	t.Parallel()

	for name, opts := range map[string]Options{
		"token":       {MCPRailwayProjectID: "project", MCPRailwayEnvironmentID: "environment"},
		"project":     {MCPRailwayAPIToken: "token", MCPRailwayEnvironmentID: "environment"},
		"environment": {MCPRailwayAPIToken: "token", MCPRailwayProjectID: "project"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := newRailwayBackend(false, opts); err == nil {
				t.Fatalf("expected missing %s configuration to fail", name)
			}
		})
	}
}

func TestRailwaySpecsRemoteUsesPrivateServiceAndMaterializesConfig(t *testing.T) {
	t.Parallel()

	b := &railwayBackend{
		projectID:       "project",
		environmentID:   "environment",
		servicePrefix:   "obot-mcp-",
		remoteShimImage: "ghcr.io/obot-platform/nanobot:v0.0.88",
	}
	specs, err := b.serviceSpecs(ServerConfig{
		Runtime:              types.RuntimeRemote,
		URL:                  "https://example.com/mcp",
		MCPServerName:        "server_123",
		MCPServerDisplayName: "Example",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 {
		t.Fatalf("expected one service, got %d", len(specs))
	}
	if specs[0].Name != "obot-mcp-server-123" {
		t.Fatalf("unexpected service name %q", specs[0].Name)
	}
	if specs[0].Variables["OBOT_NANOBOT_CONFIG_B64"] == "" {
		t.Fatal("nanobot configuration was not materialized")
	}
	if specs[0].Port != defaultContainerPort || specs[0].HealthcheckPath != "/healthz" {
		t.Fatalf("unexpected runtime endpoint: port=%d health=%q", specs[0].Port, specs[0].HealthcheckPath)
	}
}

func TestRailwaySpecsContainerizedUsesDirectService(t *testing.T) {
	t.Parallel()

	b := &railwayBackend{servicePrefix: "obot-mcp-", remoteShimImage: "nanobot"}
	specs, err := b.serviceSpecs(ServerConfig{
		Runtime:              types.RuntimeContainerized,
		ContainerImage:       "example/mcp:1",
		ContainerPort:        3000,
		ContainerPath:        "/mcp",
		MCPServerName:        "catalog-server",
		MCPServerDisplayName: "Catalog",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 {
		t.Fatalf("expected one direct service, got %d", len(specs))
	}
	if specs[0].Name != "obot-mcp-catalog-server-mcp" {
		t.Fatalf("unexpected service name: %q", specs[0].Name)
	}
}

func TestRailwayTransformedConfigUsesDirectContainerEndpoint(t *testing.T) {
	t.Parallel()

	b := &railwayBackend{environmentID: "environment"}
	config := b.transformedConfig(ServerConfig{
		Runtime:       types.RuntimeContainerized,
		ContainerPort: 3000,
		ContainerPath: "/mcp",
		MCPServerName: "catalog-server",
	}, railwayService{ID: "service-1", Name: "obot-mcp-catalog-server"})

	if config.URL != "http://obot-mcp-catalog-server.railway.internal:3000/mcp" {
		t.Fatalf("unexpected direct endpoint %q", config.URL)
	}
}

func TestRailwayEnsureDeploymentIsIdempotent(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		switch requests {
		case 1:
			_, _ = w.Write([]byte(`{"data":{"project":{"services":{"edges":[]}}}}`))
		case 2:
			_, _ = w.Write([]byte(`{"data":{"serviceCreate":{"id":"service-1","name":"obot-mcp-demo"}}}`))
		case 3:
			_, _ = w.Write([]byte(`{"data":{"variables":{}}}`))
		case 4:
			_, _ = w.Write([]byte(`{"data":{"variableCollectionUpsert":true}}`))
		case 5:
			_, _ = w.Write([]byte(`{"data":{"serviceInstanceUpdate":true}}`))
		case 6:
			_, _ = w.Write([]byte(`{"data":{"serviceInstanceDeployV2":true}}`))
		case 7:
			_, _ = w.Write([]byte(`{"data":{"serviceInstance":{"latestDeployment":{"id":"deployment-1","status":"SUCCESS","createdAt":"2026-07-13T00:00:00Z"}}}}`))
		default:
			t.Fatalf("unexpected GraphQL request %d", requests)
		}
	}))
	defer server.Close()

	b := &railwayBackend{
		client:            newRailwayClient(server.URL, "token"),
		projectID:         "project",
		environmentID:     "environment",
		servicePrefix:     "obot-mcp-",
		remoteShimImage:   "nanobot",
		startupPollPeriod: 0,
		skipReadiness:     true,
	}
	result, err := b.ensureServerDeployment(context.Background(), ServerConfig{
		Runtime:              types.RuntimeRemote,
		URL:                  "https://example.com/mcp",
		MCPServerName:        "demo",
		MCPServerDisplayName: "Demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "http://obot-mcp-demo.railway.internal:8099" || result.Scope != "service-1" {
		t.Fatalf("unexpected transformed config: %#v", result)
	}
}

func TestRailwayBackendIntegration(t *testing.T) {
	token := os.Getenv("OBOT_TEST_RAILWAY_API_TOKEN")
	projectID := os.Getenv("OBOT_TEST_RAILWAY_PROJECT_ID")
	environmentID := os.Getenv("OBOT_TEST_RAILWAY_ENVIRONMENT_ID")
	if token == "" || projectID == "" || environmentID == "" {
		t.Skip("Railway integration test credentials are not configured")
	}

	backend, err := newRailwayBackend(false, Options{
		MCPRailwayAPIToken:      token,
		MCPRailwayProjectID:     projectID,
		MCPRailwayEnvironmentID: environmentID,
		MCPRailwayServicePrefix: "obot-integration-",
		MCPRemoteShimBaseImage:  "ghcr.io/obot-platform/nanobot:v0.0.88",
	})
	if err != nil {
		t.Fatal(err)
	}
	rb := backend.(*railwayBackend)
	rb.skipReadiness = true
	server := ServerConfig{
		Runtime:              types.RuntimeRemote,
		URL:                  "https://mcp.deepwiki.com/mcp",
		MCPServerName:        "railway-backend-probe",
		MCPServerDisplayName: "Railway backend probe",
		StartupTimeout:       5 * time.Minute,
	}
	t.Cleanup(func() {
		if err := rb.shutdownServer(context.Background(), server.MCPServerName, true); err != nil {
			t.Errorf("cleanup Railway service: %v", err)
		}
	})
	deployed, err := rb.ensureServerDeployment(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	if deployed.Scope == "" || deployed.URL == "" {
		t.Fatalf("incomplete deployed server config: %#v", deployed)
	}
}
