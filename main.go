package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	trafficv2 "github.com/tetrateio/api/tsb/traffic/v2"
	typesv2 "github.com/tetrateio/api/tsb/types/v2"
	"github.com/tetrateio/tetrate/pkg/api"
	"github.com/tetrateio/tetrate/pkg/fqn"
	"github.com/tetrateio/tetrate/tctl/pkg/printers"
	"golang.org/x/exp/slices"
	"google.golang.org/protobuf/types/known/anypb"
	"istio.io/api/networking/v1beta1"
	network1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const DATE_FORMAT = "2006-01-02"

type Config struct {
	username string
	password string
	server   string
	org      string
	start    time.Time
	end      time.Time
	insecure bool

	debug   bool
	verbose bool
}

type APIClient interface {
	// Returns the service topology from skywalking, which needs to be normalized to services in
	// TSB via the 'aggregated metrics' names in each TSB Service.
	GetTopology(start, end time.Time) (*TopologyResponse, error)
	// Calls TSB's ListServices endpoint
	GetServices() ([]Service, error)
	// Returns the traffic group that matches the provided service
	LookupTrafficGroup(service *Service) (*TrafficGroup, error) // TODO: multi-error
	// Returns the TrafficSetting for the provided group FQN
	GetTrafficSettings(groupFQN string) (*trafficv2.TrafficSetting, error)
}

type Runtime struct {
	start  time.Time
	end    time.Time
	server string

	debug   bool
	verbose bool
	client  APIClient
}

var debug = func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }

func main() {

	// flags
	var (
		startFlag string
		endFlag   string
		noverbose bool
	)

	// static & runtime configs
	var (
		cfg     = &Config{}
		runtime = &Runtime{}
	)
	cmd := &cobra.Command{
		Use:   "generate-sidecar-tool",
		Short: "generate-sidecar-tool: a simple tool for creating Istio Sidecar or TSB TrafficSetting reachability based on the service topology",
		PreRunE: func(cmd *cobra.Command, args []string) error {
			// Set up the app based on config+flags
			if !cfg.debug {
				debug = func(fmt string, args ...any) {}
			}
			if noverbose {
				cfg.verbose = false
			}

			if cfg.server == "" {
				return fmt.Errorf("server address (-s or --server) can't be empty, need an address like 'tsb.yourcorp.com' or an IP like '127.0.1.10'")
			} else {
				// normalize the name; in the client code we prefix every call with `https`, so
				// strip any prefix on input so that both address with protocol and without work
				cfg.server = strings.TrimPrefix(cfg.server, "https://")
				cfg.server = strings.TrimPrefix(cfg.server, "http://")

				debug("got TSB string %q", cfg.server)
			}

			if start, err := time.Parse(DATE_FORMAT, startFlag); err != nil {
				return fmt.Errorf("failed to parse start time %q: %w", startFlag, err)
			} else {
				cfg.start = start
			}
			if end, err := time.Parse(DATE_FORMAT, endFlag); err != nil {
				return fmt.Errorf("failed to parse start time %q: %w", endFlag, err)
			} else {
				cfg.end = end
			}

			runtime = &Runtime{
				start:   cfg.start,
				end:     cfg.end,
				server:  cfg.server,
				debug:   cfg.debug,
				verbose: cfg.verbose,
				client:  NewTSBHttpClient(cfg),
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			debugLogJSON := func(data interface{}) { debugLogJSON(runtime, data) }
			// Do the work: get the topology and services
			top, err := runtime.client.GetTopology(runtime.start, runtime.end)
			if err != nil {
				return fmt.Errorf("failed to get server topology: %w", err)
			}
			debugLogJSON(top)

			services, err := runtime.client.GetServices()
			if err != nil {
				return fmt.Errorf("failed to get service list: %w", err)
			}
			debugLogJSON(services)

			// take the data and build the graph of namespaces; we get back a map of
			// source namespace to list of destination namespaces
			callers := buildGraph(runtime, top, services)

			results, err := generateSettings(runtime.client, callers)
			if err != nil {
				return err
			}
			var resp []api.Response
			for _, r := range results {
				resp = append(resp, api.ProtoToResponses(r)...)
			}

			printers.OutputResponse(resp, api.OutputType(api.OutputYAML), cmd.OutOrStdout(), printers.DefaultFormatter{}, "")
			return nil
		},
	}

	cmd.Flags().StringVarP(&cfg.server, "server", "s", "", "Address of the TSB API server, e.g. some.tsb.address.example.com. REQUIRED")
	cmd.Flags().StringVarP(&cfg.username, "http-auth-user", "u", "", "Username to call TSB with via HTTP Basic Auth. REQUIRED")
	cmd.Flags().StringVarP(&cfg.password, "http-auth-password", "p", "", "Password to call TSB with via HTTP Basic Auth. REQUIRED")
	cmd.Flags().StringVar(&cfg.org, "org", "tetrate", "TSB org to query against")
	cmd.Flags().StringVar(&startFlag, "start", fmt.Sprint(time.Now().Add(-5*24*time.Hour).Format(DATE_FORMAT)),
		"Start of the time range to query the topology in YYYY-MM-DD format")
	cmd.Flags().StringVar(&endFlag, "end", fmt.Sprint(time.Now().Format(DATE_FORMAT)),
		"End of the time range to query the topology in YYYY-MM-DD format")
	cmd.Flags().BoolVarP(&cfg.insecure, "insecure", "k", false, "Skip certificate verification when calling TSB")
	cmd.Flags().BoolVar(&cfg.debug, "debug", false, "Enable debug logging")
	cmd.Flags().BoolVar(&cfg.verbose, "verbose", true, "Enable verbose output, explaining why policy was generated; otherwise only the policy documents are printed.")
	cmd.Flags().BoolVar(&noverbose, "noverbose", false, "Disable verbose output; overrides --verbose (equivalent to --verbose=false)")

	if err := cmd.Execute(); err != nil {
		os.Exit(-1)
	}
}

func generateDirectModeSidecars(call *Call, seenNs map[string][]string, sidecars map[string]*network1beta1.Sidecar, annotations map[string]string) {
	for _, ns := range call.SourceNamespaces {
		if _, ok := seenNs[ns]; !ok {
			seenNs[ns] = make([]string, 0)
		}

		debug("source namespace: %s", ns)
		if _, ok := sidecars[ns]; !ok {
			sidecars[ns] = &network1beta1.Sidecar{
				ObjectMeta: v1.ObjectMeta{
					Name:        "reachability-sidecar",
					Namespace:   ns,
					Annotations: annotations,
				},
				Spec: v1beta1.Sidecar{
					Egress: []*v1beta1.IstioEgressListener{
						{
							Hosts: []string{"istio-system/*", "xcp-multicluster/*"},
						},
					},
				},
			}
			debug("new sidecar for namespace: %s: %+v", ns, sidecars[ns])
		}

		for _, destNs := range call.TargetNamespaces {
			if slices.Contains(seenNs[ns], destNs) {
				debug("dest %q already exists for ns %q", destNs, ns)
				continue
			}
			seenNs[ns] = append(seenNs[ns], destNs)
			debug("fist time found ns %q for src %q", destNs, ns)
			sidecars[ns].Spec.Egress[0].Hosts = append(sidecars[ns].Spec.Egress[0].Hosts, destNs+"/*")
		}
	}
}

func generateBridgedModeTrafficSettings(client APIClient, call *Call, seenNs map[string][]string, trafficSettings map[string]*trafficv2.TrafficSetting, meta *typesv2.ObjectMeta) error {
	for _, ns := range call.SourceNamespaces {
		if _, ok := seenNs[ns]; !ok {
			seenNs[ns] = make([]string, 0)
		}

		debug("source namespace: %s", ns)
		if _, ok := trafficSettings[call.SourceTrafficGroup.FQN]; !ok {
			settings, err := client.GetTrafficSettings(call.SourceTrafficGroup.FQN)
			if err != nil {
				return err
			}
			if settings == nil {
				// No traffic setting for the traffic group
				settings = &trafficv2.TrafficSetting{
					Reachability: &trafficv2.ReachabilitySettings{
						Hosts: []string{"istio-system/*", "xcp-multicluster/*"},
					},
					Fqn: fqn.Tctl{}.FromMeta(api.TrafficAPI, api.TrafficSettingKind, meta),
				}
			}
			trafficSettings[call.SourceTrafficGroup.FQN] = settings
			debug("got settings for namespace %q: %+v", ns, settings)
		}

		for _, destNs := range call.TargetNamespaces {
			if slices.Contains(seenNs[ns], destNs) {
				debug("dest %q already exists for ns %q", destNs, ns)
				continue
			}
			seenNs[ns] = append(seenNs[ns], destNs)
			debug("fist time found ns %q for src %q", destNs, ns)
			reach := trafficSettings[call.SourceTrafficGroup.FQN].GetReachability()
			if reach != nil {
				reachMode := reach.GetMode()
				if reachMode != trafficv2.ReachabilitySettings_CUSTOM {
					debug("can't create sidecar setting for traffic group %q, as its settings have rachability mode different than CUSTOM")
				}
			}
			if !slices.Contains(trafficSettings[call.SourceTrafficGroup.FQN].GetReachability().GetHosts(), destNs+"/*") {
				trafficSettings[call.SourceTrafficGroup.FQN].Reachability.Hosts = append(trafficSettings[call.SourceTrafficGroup.FQN].GetReachability().GetHosts(), destNs+"/*")
			}
		}
	}

	return nil
}

func generateSettings(client APIClient, graph *Graph) ([]*typesv2.Object, error) {
	sidecars := make(map[string]*network1beta1.Sidecar)
	// map[group FQN]*trafficv2.TrafficSetting
	trafficSettings := make(map[string]*trafficv2.TrafficSetting)
	// map[group FQN]*typesv2.ObjectMeta
	trafficMeta := make(map[string]*typesv2.ObjectMeta)
	debug("generating sidecars")

	// sourceNS => list of seen dest namespaces
	seenNs := make(map[string][]string)

	for _, call := range graph.Calls {
		debug("processing call: %+v", call)

		if call.SourceTrafficGroup == nil {
			continue
		}
		switch call.SourceTrafficGroup.ConfigMode {
		case "DIRECT":
			annotations := directModeAnnotations(call.SourceTrafficGroup.FQN)
			generateDirectModeSidecars(call, seenNs, sidecars, annotations)
		default:
			meta := bridgedModeMeta(call.SourceTrafficGroup.FQN)
			trafficMeta[call.SourceTrafficGroup.FQN] = meta
			if err := generateBridgedModeTrafficSettings(client, call, seenNs, trafficSettings, meta); err != nil {
				return nil, err
			}
		}

	}

	results := make([]*typesv2.Object, 0, len(sidecars)+len(trafficSettings))
	for _, s := range sidecars {
		debug("process sidecar: %+v", s)

		any, err := anypb.New(&s.Spec)
		if err != nil {
			return nil, fmt.Errorf("creating anypb: %w", err)
		}
		newSidecar := &typesv2.Object{
			Metadata: &typesv2.ObjectMeta{
				Annotations: s.GetAnnotations(),
				Labels:      s.GetLabels(),
				Namespace:   s.GetNamespace(),
				Name:        s.GetName(),
			},
			ApiVersion: api.IstioNetworkingBeta1API,
			Kind:       api.IstioSidecarKind,
			Spec:       any,
		}

		results = append(results, newSidecar)
	}
	for _, t := range trafficSettings {
		debug("process trafficsettings: %+v", t)
		any, err := anypb.New(t)
		if err != nil {
			return nil, fmt.Errorf("creating anypb: %w", err)
		}
		newSidecar := &typesv2.Object{
			Metadata:   trafficMeta[t.GetFqn()],
			ApiVersion: api.TrafficAPI,
			Kind:       api.TrafficSettingKind,
			Spec:       any,
		}

		results = append(results, newSidecar)
	}

	debug("total results: %d", len(results))
	return results, nil
}

func bridgedModeMeta(fqn string) *typesv2.ObjectMeta {
	meta := &typesv2.ObjectMeta{}

	fqnParts := strings.Split(fqn, "/")
	for i := 0; i < len(fqnParts); i += 2 {
		key := fqnParts[i]
		switch key {
		case "organizations":
			meta.Organization = fqnParts[i+1]
		case "tenants":
			meta.Tenant = fqnParts[i+1]
		case "workspaces":
			meta.Workspace = fqnParts[i+1]
		case "trafficgroups":
			meta.Group = fqnParts[i+1]
		}
	}
	debug("metadata for service %q: %+v", fqn, meta)
	return meta
}

func directModeAnnotations(fqn string) map[string]string {
	annotations := make(map[string]string)

	fqnParts := strings.Split(fqn, "/")
	for i := 0; i < len(fqnParts); i += 2 {
		key := fqnParts[i]
		switch key {
		case "organizations":
			annotations["tsb.tetrate.io/organization"] = fqnParts[i+1]
		case "tenants":
			annotations["tsb.tetrate.io/tenant"] = fqnParts[i+1]
		case "workspaces":
			annotations["tsb.tetrate.io/workspace"] = fqnParts[i+1]
		case "trafficgroups":
			annotations["tsb.tetrate.io/trafficGroup"] = fqnParts[i+1]
		}
	}
	debug("annotations for service %q: %+v", fqn, annotations)
	return annotations
}

type Graph struct {
	Calls []*Call
}

type Call struct {
	SourceService      *Service
	SourceNamespaces   []string
	SourceTrafficGroup *TrafficGroup

	TargetService    *Service
	TargetNamespaces []string
}

// Normalizes the topology response and service list into a Graph of source namespace to set of target namespace
func buildGraph(runtime *Runtime, top *TopologyResponse, services []Service) *Graph {
	graph := &Graph{
		Calls: make([]*Call, 0),
	}

	servicesByTopKey := make(map[string]*Service)
	for _, svc := range services {
		local := svc
		for _, metric := range svc.Metrics {
			debug("service %q has FQN %q", metric.AggregationKey, local.FQN)
			servicesByTopKey[metric.AggregationKey] = &local
		}
	}

	idToTopKey := make(map[string]string)
	for _, node := range top.Nodes {
		debug("node ID %q belongs to %q", node.ID, node.AggregationKey)
		idToTopKey[node.ID] = node.AggregationKey
	}

	servicesByID := make(map[string]*Service)
	for id, key := range idToTopKey {
		if svc, ok := servicesByTopKey[key]; ok {
			servicesByID[id] = svc
			debug("id %q maps to service %q", id, svc.FQN)
		} else {
			debug("no service for key %q", key)
		}
	}

	for _, traffic := range top.Calls {
		debug("processing call %s", traffic.ID)

		source, ok := servicesByID[traffic.Source]
		if !ok {
			debug("no service for key %s", traffic.Source)
			continue
		}
		target, ok := servicesByID[traffic.Target]
		if !ok {
			debug("no service for key %s", traffic.Target)
			continue
		}
		debug("computed source => target: %s => %s", source.FQN, target.FQN)

		call := &Call{
			SourceService: source,
			TargetService: target,
		}

		srcNamespaces := parseNamespace(source)
		call.SourceNamespaces = srcNamespaces
		targetNamespaces := parseNamespace(target)
		call.TargetNamespaces = targetNamespaces

		tg, err := runtime.client.LookupTrafficGroup(source)
		if err != nil {
			debug("error getting traffic group for %s: %w", source.FQN, err)
			return nil
		}
		if tg == nil {
			fmt.Fprintf(os.Stderr, "no trafficgroup found for source service %q, skipping...\n", source.FQN)
		}
		call.SourceTrafficGroup = tg

		graph.Calls = append(graph.Calls, call)
	}
	debug("graph built")
	return graph
}

func parseNamespace(service *Service) []string {
	var results []string

	for _, dep := range service.ServiceDeployments {
		fqnParts := strings.Split(dep.FQN, "/")
		for i := 0; i < len(fqnParts); i += 2 {
			key := fqnParts[i]
			if key == "namespaces" {
				results = append(results, fqnParts[i+1])
			}
		}
	}

	return results
}
