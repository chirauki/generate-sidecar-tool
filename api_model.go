package main

type TopologyResponse struct {
	Nodes []struct {
		ID             string `json:"id"`
		AggregationKey string `json:"name"`
	} `json:"nodes"`
	Calls []struct {
		ID     string `json:"id"`
		Source string `json:"source"`
		Target string `json:"target"`
	} `json:"calls"`
}

type Service struct {
	FQN         string `json:"fqn"`
	DisplayName string `json:"displayName"`
	Metrics     []struct {
		AggregationKey string `json:"aggregationKey"`
	} `json:"metrics"`
	CanonicalName      string              `json:"canonicalName"`
	SpiffeIds          []string            `json:"spiffeIds"`
	ServiceDeployments []ServiceDeployment `json:"serviceDeployments"`
}

type ServiceDeployment struct {
	FQN    string `json:"fqn"`
	Source string `json:"source"`
}
