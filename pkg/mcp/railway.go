package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	otypes "github.com/obot-platform/obot/apiclient/types"
	"github.com/obot-platform/obot/pkg/utils"
)

const railwayConfigHashVariable = "OBOT_MCP_CONFIG_HASH"

var railwayServiceNameInvalid = regexp.MustCompile(`[^a-z0-9-]+`)

type railwayBackend struct {
	client            *railwayClient
	projectID         string
	environmentID     string
	servicePrefix     string
	obotServiceName   string
	obotInternalPort  int
	containerImage    string
	remoteShimImage   string
	authEnabled       bool
	auditBatchSize    int
	auditFlushSeconds int
	startupPollPeriod time.Duration
	skipReadiness     bool
}

type railwayServiceSpec struct {
	Name            string
	Image           string
	StartCommand    string
	HealthcheckPath string
	Port            int
	Variables       map[string]string
	UpstreamURL     string
}

type railwayService struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type railwayDeployment struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt"`
}

func newRailwayBackend(authEnabled bool, opts Options) (backend, error) {
	if strings.TrimSpace(opts.MCPRailwayAPIToken) == "" {
		return nil, errors.New("Railway API token is required")
	}
	if strings.TrimSpace(opts.MCPRailwayProjectID) == "" {
		return nil, errors.New("Railway project ID is required")
	}
	if strings.TrimSpace(opts.MCPRailwayEnvironmentID) == "" {
		return nil, errors.New("Railway environment ID is required")
	}
	apiURL := opts.MCPRailwayAPIURL
	if apiURL == "" {
		apiURL = "https://backboard.railway.com/graphql/v2"
	}
	prefix := opts.MCPRailwayServicePrefix
	if prefix == "" {
		prefix = "obot-mcp-"
	}
	serviceName := opts.MCPRailwayObotServiceName
	if serviceName == "" {
		serviceName = "obot"
	}
	port := opts.MCPRailwayObotInternalPort
	if port == 0 {
		port = 8080
	}
	return &railwayBackend{
		client:            newRailwayClient(apiURL, opts.MCPRailwayAPIToken),
		projectID:         opts.MCPRailwayProjectID,
		environmentID:     opts.MCPRailwayEnvironmentID,
		servicePrefix:     prefix,
		obotServiceName:   serviceName,
		obotInternalPort:  port,
		containerImage:    opts.MCPBaseImage,
		remoteShimImage:   opts.MCPRemoteShimBaseImage,
		authEnabled:       authEnabled,
		auditBatchSize:    opts.MCPAuditLogsPersistBatchSize,
		auditFlushSeconds: opts.MCPAuditLogPersistIntervalSeconds,
		startupPollPeriod: time.Second,
	}, nil
}

func (r *railwayBackend) ensureServerDeployment(ctx context.Context, server ServerConfig, webhooks []Webhook) (ServerConfig, error) {
	services, err := r.apply(ctx, server, webhooks, true)
	if err != nil {
		return ServerConfig{}, err
	}
	endpoint := services[len(services)-1]
	result := r.transformedConfig(server, endpoint)
	if !r.skipReadiness {
		readyCtx, cancel := context.WithTimeout(ctx, server.StartupTimeout)
		if server.StartupTimeout <= 0 {
			readyCtx, cancel = context.WithTimeout(ctx, MaxMCPServerStartupTimeout)
		}
		defer cancel()
		if err := ensureServerReady(readyCtx, result.URL, result); err != nil {
			return ServerConfig{}, fmt.Errorf("Railway service readiness check failed: %w", err)
		}
	}
	return result, nil
}

func (r *railwayBackend) deployServer(ctx context.Context, server ServerConfig, webhooks []Webhook) error {
	_, err := r.apply(ctx, server, webhooks, false)
	return err
}

func (r *railwayBackend) apply(ctx context.Context, server ServerConfig, webhooks []Webhook, wait bool) ([]railwayService, error) {
	specs, err := r.serviceSpecs(server, webhooks)
	if err != nil {
		return nil, err
	}
	existing, err := r.client.listServices(ctx, r.projectID)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]railwayService, len(existing))
	for _, service := range existing {
		byName[service.Name] = service
	}

	result := make([]railwayService, 0, len(specs))
	for _, spec := range specs {
		service, ok := byName[spec.Name]
		if !ok {
			service, err = r.client.createService(ctx, r.projectID, r.environmentID, spec.Name, spec.Image)
			if err != nil {
				return nil, fmt.Errorf("create Railway service %s: %w", spec.Name, err)
			}
		}
		desiredHash := utils.Digest(spec)
		variables, err := r.client.variables(ctx, r.projectID, r.environmentID, service.ID)
		if err != nil && ok {
			return nil, fmt.Errorf("read Railway variables for %s: %w", spec.Name, err)
		}
		if variables[railwayConfigHashVariable] != desiredHash {
			spec.Variables[railwayConfigHashVariable] = desiredHash
			if err := r.client.upsertVariables(ctx, r.projectID, r.environmentID, service.ID, spec.Variables); err != nil {
				return nil, fmt.Errorf("set Railway variables for %s: %w", spec.Name, err)
			}
			if err := r.client.updateServiceInstance(ctx, r.environmentID, service.ID, spec); err != nil {
				return nil, fmt.Errorf("configure Railway service %s: %w", spec.Name, err)
			}
			if err := r.client.deploy(ctx, r.environmentID, service.ID); err != nil {
				return nil, fmt.Errorf("deploy Railway service %s: %w", spec.Name, err)
			}
		}
		if wait {
			if _, err := r.waitForDeployment(ctx, service.ID); err != nil {
				return nil, fmt.Errorf("wait for Railway service %s: %w", spec.Name, err)
			}
		}
		result = append(result, service)
	}
	return result, nil
}

func (r *railwayBackend) serviceSpecs(server ServerConfig, webhooks []Webhook) ([]railwayServiceSpec, error) {
	if len(server.Files) > 0 {
		return nil, &ErrNotSupportedByBackend{Feature: "MCP file mounts", Backend: RuntimeBackendRailway}
	}
	baseName := railwayServiceName(r.servicePrefix + server.MCPServerName)
	if baseName == "" {
		return nil, errors.New("MCP server name is required")
	}
	server = r.rewriteServerEndpoints(server)

	switch server.Runtime {
	case otypes.RuntimeContainerized:
		if server.ContainerImage == "" || server.ContainerPort <= 0 {
			return nil, errors.New("container image and port are required for Railway containerized runtime")
		}
		realName := railwayServiceName(baseName + "-mcp")
		real := railwayServiceSpec{
			Name:         realName,
			Image:        server.ContainerImage,
			Port:         server.ContainerPort,
			StartCommand: commandString(server.Command, server.Args),
			Variables:    envSliceToMap(server.Env),
		}
		if _, ok := real.Variables["PORT"]; !ok {
			real.Variables["PORT"] = strconv.Itoa(server.ContainerPort)
		}
		if !server.NeedsShim() {
			return []railwayServiceSpec{real}, nil
		}
		upstream := fmt.Sprintf("http://%s.railway.internal:%d", realName, server.ContainerPort)
		if server.ContainerPath != "" {
			upstream += "/" + strings.TrimPrefix(server.ContainerPath, "/")
		}
		shimServer := server
		shimServer.Runtime = otypes.RuntimeRemote
		shimServer.URL = upstream
		shim, err := r.nanobotSpec(baseName, shimServer, webhooks)
		if err != nil {
			return nil, err
		}
		shim.UpstreamURL = upstream
		return []railwayServiceSpec{real, shim}, nil
	case otypes.RuntimeUVX, otypes.RuntimeNPX, otypes.RuntimeRemote, otypes.RuntimeComposite:
		return r.singleNanobotSpec(baseName, server, webhooks)
	default:
		return nil, fmt.Errorf("unsupported Railway runtime: %s", server.Runtime)
	}
}

func (r *railwayBackend) singleNanobotSpec(name string, server ServerConfig, webhooks []Webhook) ([]railwayServiceSpec, error) {
	spec, err := r.nanobotSpec(name, server, webhooks)
	if err != nil {
		return nil, err
	}
	return []railwayServiceSpec{spec}, nil
}

func (r *railwayBackend) nanobotSpec(name string, server ServerConfig, webhooks []Webhook) (railwayServiceSpec, error) {
	env := envSliceToBytes(server.Env)
	headers := envSliceToBytes(server.Headers)
	var data []byte
	var err error
	if server.Runtime == otypes.RuntimeComposite {
		data, err = constructMCPServerNanobotYAMLForComposite(server.Components)
	} else {
		data, err = constructMCPServerNanobotYAML(server.MCPServerDisplayName, server.URL, server.Command, server.Args, server.PassthroughHeaderNames, env, headers, webhooks)
	}
	if err != nil {
		return railwayServiceSpec{}, fmt.Errorf("construct nanobot configuration: %w", err)
	}
	image := r.remoteShimImage
	if server.Runtime == otypes.RuntimeUVX || server.Runtime == otypes.RuntimeNPX {
		image = r.containerImage
	}
	variables := map[string]string{
		"OBOT_NANOBOT_CONFIG_B64":                      base64.StdEncoding.EncodeToString(data),
		"PORT":                                         strconv.Itoa(defaultContainerPort),
		"NANOBOT_RUN_HEALTHZ_PATH":                     "/healthz",
		"NANOBOT_RUN_FORCE_FETCH_TOOL_LIST":            "true",
		"NANOBOT_DISABLE_HEALTH_CHECKER":               "true",
		"NANOBOT_RUN_LISTEN_ADDRESS":                   ":8099",
		"NANOBOT_RUN_MCPSERVER_ID":                     strings.TrimSuffix(server.MCPServerName, "-shim"),
		"NANOBOT_RUN_AUDIT_LOG_TOKEN":                  server.AuditLogToken,
		"NANOBOT_RUN_AUDIT_LOG_SEND_URL":               server.AuditLogEndpoint,
		"NANOBOT_RUN_AUDIT_LOG_METADATA":               server.AuditLogMetadata,
		"NANOBOT_RUN_AUDIT_LOG_BATCH_SIZE":             strconv.Itoa(r.auditBatchSize),
		"NANOBOT_RUN_AUDIT_LOG_FLUSH_INTERVAL_SECONDS": strconv.Itoa(r.auditFlushSeconds),
	}
	if r.authEnabled {
		variables["NANOBOT_RUN_TRUSTED_ISSUER"] = server.Issuer
		variables["NANOBOT_RUN_OAUTH_JWKSURL"] = server.JWKSEndpoint
		variables["NANOBOT_RUN_TRUSTED_AUDIENCES"] = strings.Join(server.Audiences, ",")
		variables["NANOBOT_RUN_OAUTH_CLIENT_ID"] = server.TokenExchangeClientID
		variables["NANOBOT_RUN_OAUTH_CLIENT_SECRET"] = server.TokenExchangeClientSecret
		variables["NANOBOT_RUN_OAUTH_TOKEN_URL"] = server.TokenExchangeEndpoint
		variables["NANOBOT_RUN_OAUTH_AUTHORIZE_URL"] = server.AuthorizeEndpoint
		variables["NANOBOT_RUN_OAUTH_SCOPES"] = "profile"
		variables["NANOBOT_RUN_APIKEY_AUTH_WEBHOOK_URL"] = r.transformObotHostname(server.Issuer + "/api/api-keys/auth")
	}
	return railwayServiceSpec{
		Name:            name,
		Image:           image,
		Port:            defaultContainerPort,
		HealthcheckPath: "/healthz",
		StartCommand:    `sh -lc 'printf %s "$OBOT_NANOBOT_CONFIG_B64" | base64 -d > /tmp/nanobot.yaml && exec nanobot run --disable-ui --listen-address :8099 --exclude-built-in-agents --config /tmp/nanobot.yaml'`,
		Variables:       variables,
	}, nil
}

func (r *railwayBackend) transformConfig(ctx context.Context, server ServerConfig) (*ServerConfig, error) {
	name := railwayServiceName(r.servicePrefix + server.MCPServerName)
	service, err := r.client.serviceByName(ctx, r.projectID, name)
	if err != nil || service.ID == "" {
		return nil, err
	}
	deployment, err := r.client.serviceInstance(ctx, r.environmentID, service.ID)
	if err != nil || !railwayDeploymentSuccessful(deployment.Status) {
		return nil, err
	}
	result := r.transformedConfig(server, service)
	return &result, nil
}

func (r *railwayBackend) transformedConfig(server ServerConfig, service railwayService) ServerConfig {
	healthzPath := server.HealthzPath
	if server.NeedsShim() {
		healthzPath = "/healthz"
	}

	return ServerConfig{
		Runtime:                   otypes.RuntimeRemote,
		URL:                       fmt.Sprintf("http://%s.railway.internal:%d", service.Name, defaultContainerPort),
		ContainerPort:             defaultContainerPort,
		ContainerPath:             server.ContainerPath,
		HealthzPath:               healthzPath,
		MCPServerNamespace:        r.environmentID,
		MCPServerName:             server.MCPServerName,
		MCPServerDisplayName:      server.MCPServerDisplayName,
		Scope:                     service.ID,
		UserID:                    server.UserID,
		OwnerUserID:               server.OwnerUserID,
		Audiences:                 server.Audiences,
		Issuer:                    server.Issuer,
		JWKSEndpoint:              server.JWKSEndpoint,
		TokenExchangeEndpoint:     server.TokenExchangeEndpoint,
		AuthorizeEndpoint:         server.AuthorizeEndpoint,
		TokenExchangeClientID:     server.TokenExchangeClientID,
		TokenExchangeClientSecret: server.TokenExchangeClientSecret,
		AuditLogEndpoint:          server.AuditLogEndpoint,
		AuditLogToken:             server.AuditLogToken,
		AuditLogMetadata:          server.AuditLogMetadata,
		PassthroughHeaderNames:    server.PassthroughHeaderNames,
		PassthroughHeaderValues:   server.PassthroughHeaderValues,
		StartupTimeout:            server.StartupTimeout,
	}
}

func (r *railwayBackend) streamServerLogs(ctx context.Context, id string) (io.ReadCloser, error) {
	deployment, err := r.client.serviceInstance(ctx, r.environmentID, id)
	if err != nil {
		return nil, err
	}
	if deployment.ID == "" {
		return nil, ErrServerNotRunning
	}
	logs, err := r.client.deploymentLogs(ctx, deployment.ID)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(logs)), nil
}

func (r *railwayBackend) getServerDetails(ctx context.Context, id string) (otypes.MCPServerDetails, error) {
	deployment, err := r.client.serviceInstance(ctx, r.environmentID, id)
	if err != nil {
		return otypes.MCPServerDetails{}, err
	}
	if deployment.ID == "" {
		return otypes.MCPServerDetails{}, ErrServerNotRunning
	}
	createdAt, _ := time.Parse(time.RFC3339, deployment.CreatedAt)
	available := railwayDeploymentSuccessful(deployment.Status)
	ready := int32(0)
	if available {
		ready = 1
	}
	return otypes.MCPServerDetails{
		DeploymentName: id,
		Namespace:      "railway",
		LastRestart:    otypes.Time{Time: createdAt},
		ReadyReplicas:  ready,
		Replicas:       1,
		IsAvailable:    available,
		Events: []otypes.MCPServerEvent{{
			Time: otypes.Time{Time: createdAt}, Reason: deployment.Status, Message: "Railway deployment " + deployment.Status,
			EventType: "Deployment", Action: strings.ToLower(deployment.Status), Count: 1, ResourceName: id, ResourceKind: "Service",
		}},
	}, nil
}

func (r *railwayBackend) restartServer(ctx context.Context, server ServerConfig) error {
	service, err := r.client.serviceByName(ctx, r.projectID, railwayServiceName(r.servicePrefix+server.MCPServerName))
	if err != nil || service.ID == "" {
		return err
	}
	deployment, err := r.client.serviceInstance(ctx, r.environmentID, service.ID)
	if err != nil {
		return err
	}
	return r.client.restartDeployment(ctx, deployment.ID)
}

func (r *railwayBackend) shutdownServer(ctx context.Context, id string, hardShutdown bool) error {
	names := []string{railwayServiceName(r.servicePrefix + id), railwayServiceName(r.servicePrefix + id + "-mcp")}
	for _, name := range names {
		service, err := r.client.serviceByName(ctx, r.projectID, name)
		if err != nil {
			return err
		}
		if service.ID == "" {
			continue
		}
		err = r.client.deleteService(ctx, service.ID)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *railwayBackend) transformObotHostname(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || r.obotServiceName == "" {
		return rawURL
	}
	parsed.Scheme = "http"
	parsed.Host = fmt.Sprintf("%s.railway.internal:%d", r.obotServiceName, r.obotInternalPort)
	parsed.User = nil
	return parsed.String()
}

func (r *railwayBackend) rewriteServerEndpoints(server ServerConfig) ServerConfig {
	server.TokenExchangeEndpoint = r.transformObotHostname(server.TokenExchangeEndpoint)
	server.AuthorizeEndpoint = r.transformObotHostname(server.AuthorizeEndpoint)
	server.AuditLogEndpoint = r.transformObotHostname(server.AuditLogEndpoint)
	server.JWKSEndpoint = r.transformObotHostname(server.JWKSEndpoint)
	for i := range server.Components {
		server.Components[i].URL = r.transformObotHostname(server.Components[i].URL)
	}
	return server
}

func (r *railwayBackend) waitForDeployment(ctx context.Context, serviceID string) (railwayDeployment, error) {
	period := r.startupPollPeriod
	if period <= 0 {
		period = time.Millisecond
	}
	for {
		deployment, err := r.client.serviceInstance(ctx, r.environmentID, serviceID)
		if err != nil {
			return railwayDeployment{}, err
		}
		if railwayDeploymentSuccessful(deployment.Status) {
			return deployment, nil
		}
		if railwayDeploymentFailed(deployment.Status) {
			return railwayDeployment{}, fmt.Errorf("deployment %s ended with status %s", deployment.ID, deployment.Status)
		}
		select {
		case <-ctx.Done():
			return railwayDeployment{}, ctx.Err()
		case <-time.After(period):
		}
	}
}

func railwayDeploymentSuccessful(status string) bool { return status == "SUCCESS" }
func railwayDeploymentFailed(status string) bool {
	switch status {
	case "FAILED", "CRASHED", "REMOVED", "CANCELED", "SKIPPED":
		return true
	default:
		return false
	}
}

func railwayServiceName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = railwayServiceNameInvalid.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if len(value) > 63 {
		value = strings.TrimRight(value[:63], "-")
	}
	return value
}

func envSliceToMap(values []string) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		if key, val, ok := strings.Cut(value, "="); ok {
			result[key] = val
		}
	}
	return result
}

func envSliceToBytes(values []string) map[string][]byte {
	result := make(map[string][]byte, len(values))
	for key, value := range envSliceToMap(values) {
		result[key] = []byte(value)
	}
	return result
}

func commandString(command string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	if command != "" {
		parts = append(parts, shellQuote(command))
	}
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'" }

type railwayClient struct {
	url        string
	token      string
	httpClient *http.Client
}

func newRailwayClient(apiURL, token string) *railwayClient {
	return &railwayClient{url: apiURL, token: token, httpClient: &http.Client{Timeout: 30 * time.Second}}
}

func (c *railwayClient) request(ctx context.Context, query string, variables any, target any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("Railway API returned %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		messages := make([]string, len(envelope.Errors))
		for i, apiErr := range envelope.Errors {
			messages[i] = apiErr.Message
		}
		return errors.New(strings.Join(messages, "; "))
	}
	if target == nil {
		return nil
	}
	return json.Unmarshal(envelope.Data, target)
}

func (c *railwayClient) listServices(ctx context.Context, projectID string) ([]railwayService, error) {
	var data struct {
		Project struct {
			Services struct {
				Edges []struct {
					Node railwayService `json:"node"`
				} `json:"edges"`
			} `json:"services"`
		} `json:"project"`
	}
	err := c.request(ctx, `query($id:String!){project(id:$id){services{edges{node{id name}}}}}`, map[string]any{"id": projectID}, &data)
	services := make([]railwayService, 0, len(data.Project.Services.Edges))
	for _, edge := range data.Project.Services.Edges {
		services = append(services, edge.Node)
	}
	return services, err
}

func (c *railwayClient) serviceByName(ctx context.Context, projectID, name string) (railwayService, error) {
	services, err := c.listServices(ctx, projectID)
	if err != nil {
		return railwayService{}, err
	}
	for _, service := range services {
		if service.Name == name {
			return service, nil
		}
	}
	return railwayService{}, nil
}

func (c *railwayClient) createService(ctx context.Context, projectID, environmentID, name, image string) (railwayService, error) {
	var data struct {
		Service railwayService `json:"serviceCreate"`
	}
	input := map[string]any{"projectId": projectID, "environmentId": environmentID, "name": name, "source": map[string]any{"image": image}}
	err := c.request(ctx, `mutation($input:ServiceCreateInput!){serviceCreate(input:$input){id name}}`, map[string]any{"input": input}, &data)
	return data.Service, err
}

func (c *railwayClient) variables(ctx context.Context, projectID, environmentID, serviceID string) (map[string]string, error) {
	var data struct {
		Variables map[string]string `json:"variables"`
	}
	err := c.request(ctx, `query($projectId:String!,$environmentId:String!,$serviceId:String){variables(projectId:$projectId,environmentId:$environmentId,serviceId:$serviceId)}`, map[string]any{"projectId": projectID, "environmentId": environmentID, "serviceId": serviceID}, &data)
	return data.Variables, err
}

func (c *railwayClient) upsertVariables(ctx context.Context, projectID, environmentID, serviceID string, variables map[string]string) error {
	input := map[string]any{"projectId": projectID, "environmentId": environmentID, "serviceId": serviceID, "variables": variables, "replace": true}
	return c.request(ctx, `mutation($input:VariableCollectionUpsertInput!){variableCollectionUpsert(input:$input)}`, map[string]any{"input": input}, nil)
}

func (c *railwayClient) updateServiceInstance(ctx context.Context, environmentID, serviceID string, spec railwayServiceSpec) error {
	input := map[string]any{"source": map[string]any{"image": spec.Image}, "numReplicas": 1, "restartPolicyType": "ON_FAILURE", "restartPolicyMaxRetries": 10}
	if spec.StartCommand != "" {
		input["startCommand"] = spec.StartCommand
	}
	if spec.HealthcheckPath != "" {
		input["healthcheckPath"] = spec.HealthcheckPath
		input["healthcheckTimeout"] = 300
	}
	return c.request(ctx, `mutation($serviceId:String!,$environmentId:String!,$input:ServiceInstanceUpdateInput!){serviceInstanceUpdate(serviceId:$serviceId,environmentId:$environmentId,input:$input)}`, map[string]any{"serviceId": serviceID, "environmentId": environmentID, "input": input}, nil)
}

func (c *railwayClient) deploy(ctx context.Context, environmentID, serviceID string) error {
	return c.request(ctx, `mutation($serviceId:String!,$environmentId:String!){serviceInstanceDeployV2(serviceId:$serviceId,environmentId:$environmentId)}`, map[string]any{"serviceId": serviceID, "environmentId": environmentID}, nil)
}

func (c *railwayClient) serviceInstance(ctx context.Context, environmentID, serviceID string) (railwayDeployment, error) {
	var data struct {
		Instance struct {
			Latest railwayDeployment `json:"latestDeployment"`
		} `json:"serviceInstance"`
	}
	err := c.request(ctx, `query($serviceId:String!,$environmentId:String!){serviceInstance(serviceId:$serviceId,environmentId:$environmentId){latestDeployment{id status createdAt}}}`, map[string]any{"serviceId": serviceID, "environmentId": environmentID}, &data)
	return data.Instance.Latest, err
}

func (c *railwayClient) restartDeployment(ctx context.Context, deploymentID string) error {
	if deploymentID == "" {
		return ErrServerNotRunning
	}
	return c.request(ctx, `mutation($id:String!){deploymentRestart(id:$id)}`, map[string]any{"id": deploymentID}, nil)
}

func (c *railwayClient) deleteService(ctx context.Context, serviceID string) error {
	return c.request(ctx, `mutation($id:String!){serviceDelete(id:$id)}`, map[string]any{"id": serviceID}, nil)
}

func (c *railwayClient) deploymentLogs(ctx context.Context, deploymentID string) (string, error) {
	var data struct {
		Logs []struct{ Timestamp, Message, Severity string } `json:"deploymentLogs"`
	}
	err := c.request(ctx, `query($deploymentId:String!,$limit:Int){deploymentLogs(deploymentId:$deploymentId,limit:$limit){timestamp message severity}}`, map[string]any{"deploymentId": deploymentID, "limit": 500}, &data)
	if err != nil {
		return "", err
	}
	sort.Slice(data.Logs, func(i, j int) bool { return data.Logs[i].Timestamp < data.Logs[j].Timestamp })
	var result strings.Builder
	for _, entry := range data.Logs {
		fmt.Fprintf(&result, "%s %s %s\n", entry.Timestamp, entry.Severity, entry.Message)
	}
	return result.String(), nil
}
