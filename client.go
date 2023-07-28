package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	trafficv2 "github.com/tetrateio/api/tsb/traffic/v2"
	typesv2 "github.com/tetrateio/api/tsb/types/v2"
)

type TSBHttpClient struct {
	server   string
	org      string
	username string
	password string
	client   *http.Client
}

// compile-time assert we satisfy the interface we intend to
var _ APIClient = &TSBHttpClient{}

func NewTSBHttpClient(cfg *Config) *TSBHttpClient {
	client := http.DefaultClient
	if cfg.insecure {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		client = &http.Client{Transport: tr}
	}
	return &TSBHttpClient{
		server:   cfg.server,
		org:      cfg.org,
		username: cfg.username,
		password: cfg.password,
		client:   client}
}

// Returns the service topology from skywalking, which needs to be normalized to services in
// TSB via the 'aggregated metrics' names in each TSB Service.
func (c *TSBHttpClient) GetTopology(start, end time.Time) (*TopologyResponse, error) {
	s := start.Format(DATE_FORMAT)
	e := end.Format(DATE_FORMAT)
	query := fmt.Sprintf(`{
    "query":"query ListNodesAndEdges($duration: Duration!) {topo: getGlobalTopology(duration: $duration) { nodes {id ,name, type, isReal } calls { id, source, sourceComponents, target, targetComponents, detectPoints } } }",
    "variables":{"duration":{"start":"%s","end":"%s","step":"DAY"}}
}`, s, e)

	debug("issuing query:\n%s", query)

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("https://%s/graphql", c.server), strings.NewReader(query))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	body, err := c.callTSB(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get topology: %w", err)
	}

	type respData struct {
		Data struct {
			Response TopologyResponse `json:"topo"`
		} `json:"data"`
	}

	out := &respData{}
	err = json.Unmarshal(body, out)
	return &out.Data.Response, err
}

// Calls TSB's ListServices endpoint
func (c *TSBHttpClient) GetServices() ([]Service, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://%s/v2/organizations/%s/services", c.server, c.org), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	body, err := c.callTSB(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get services: %w", err)
	}

	type respData struct {
		Services []Service `json:"services"`
	}
	out := &respData{}
	err = json.Unmarshal(body, &out)
	return out.Services, err
}

// Service FQN -> Group FQN
type TrafficGroupResponse struct {
	TrafficGroups []TrafficGroup `json:"trafficGroups"`
}

type TrafficGroup struct {
	ConfigMode string             `json:"configMode"`
	FQN        string             `json:"fqn"`
	Metadata   typesv2.ObjectMeta `json:"metadata"`
}

// Returns the traffic group that matches the provided service
func (c *TSBHttpClient) LookupTrafficGroup(svc *Service) (*TrafficGroup, error) { // TODO: multi-error
	// use the Lookup API to get the groups for each service
	url := fmt.Sprintf("https://%s/v2/%s/groups", c.server, svc.FQN)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		debug("failed to create request for service groups for %q", svc.FQN)
		return nil, err
	}

	body, err := c.callTSB(req)
	if err != nil {
		debug("failed to get service groups for %q: %v", svc.FQN, err)
		return nil, err
	}

	resp := &TrafficGroupResponse{}
	if err = json.Unmarshal(body, &resp); err != nil {
		debug("failed to unmarshal %q: %v", svc.FQN, err)
		return nil, err
	}
	if len(resp.TrafficGroups) == 0 {
		return nil, nil
	}
	return &resp.TrafficGroups[0], nil
}

// Returns the TrafficSetting for the provided group FQN
func (c *TSBHttpClient) GetTrafficSettings(groupFQN string) (*trafficv2.TrafficSetting, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://%s/%s/settings", c.server, groupFQN), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	body, err := c.callTSB(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get traffic settings: %w", err)
	}

	var out []trafficv2.TrafficSetting
	_ = json.Unmarshal(body, &out)
	if len(out) > 0 {
		return &out[0], nil
	}
	return nil, nil
}

func (c *TSBHttpClient) callTSB(req *http.Request) ([]byte, error) {
	debug("sending %v to %q", req.Method, req.URL.String())
	req.Header.Set("content-type", "application/json")
	req.SetBasicAuth(c.username, c.password)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to issue request: %w", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	sample := string(body)
	if len(body) > 80 {
		sample = fmt.Sprintf("%s...", body[0:80])
	}
	debug("got body: %s", sample)
	return body, nil
}
