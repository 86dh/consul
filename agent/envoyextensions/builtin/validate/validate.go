package validate

import (
	"fmt"

	envoy_admin_v3 "github.com/envoyproxy/go-control-plane/envoy/admin/v3"
	envoy_cluster_v3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	envoy_core_v3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	envoy_listener_v3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	envoy_route_v3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	envoy_aggregate_cluster_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/clusters/aggregate/v3"
	envoy_resource_v3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/hashicorp/go-multierror"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/hashicorp/consul/agent/envoyextensions/extensioncommon"
)

const builtinValidateExtension = "builtin/proxy/validate"

// Validate contains input information about which proxy resources to validate and output information about resources it
// has validated.
type Validate struct {
	// envoyID is an argument to the Validate plugin and identifies which listener to begin the validation with.
	envoyID string

	// snis is all of the upstream SNIs for this proxy. It is set via ExtensionConfiguration.
	snis map[string]struct{}

	// listener specifies if the service's listener has been seen.
	listener bool

	// usesRDS determines if the listener's outgoing filter uses RDS.
	usesRDS bool

	// listener specifies if the service's route has been seen.
	route bool

	// resources is a mapping from SNI to the expected resources
	// for that SNI. It is populated based on the cluster names on routes
	// (whether they are specified on listener filters or routes).
	resources map[string]*resource
}

type resource struct {
	// required determines if the resource is required for the given upstream.
	required bool
	// cluster specifies if the cluster has been seen.
	cluster bool
	// aggregateCluster determines if the resource is an aggregate cluster.
	aggregateCluster bool
	// aggregateClusterChildren is a list of SNIs to identify the child clusters of this aggregate cluster.
	aggregateClusterChildren []string
	// parentCluster is empty if this is a top level cluster, and has a value if this is a child of an aggregate
	// cluster.
	parentCluster string
	// loadAssignment specifies if the load assignment has been seen.
	loadAssignment bool
	// usesEDS specifies if the cluster has EDS configured.
	usesEDS bool
	// The number of endpoints for the cluster or load assignment.
	endpoints int
}

var _ extensioncommon.BasicExtension = (*Validate)(nil)

// EndpointValidator allows us to inject a different function for tests.
type EndpointValidator func(*resource, string, *envoy_admin_v3.Clusters)

// MakeValidate is a builtinextensiontemplate.PluginConstructor for a builtinextensiontemplate.EnvoyExtension.
func MakeValidate(ext extensioncommon.RuntimeConfig) (extensioncommon.BasicExtension, error) {
	var resultErr error
	var plugin Validate

	if name := ext.EnvoyExtension.Name; name != builtinValidateExtension {
		return nil, fmt.Errorf("expected extension name 'builtin/proxy/validate' but got %q", name)
	}

	envoyID, _ := ext.EnvoyExtension.Arguments["envoyID"]
	mainEnvoyID, _ := envoyID.(string)
	if len(mainEnvoyID) == 0 {
		return nil, fmt.Errorf("envoyID is required")
	}
	plugin.envoyID = mainEnvoyID
	plugin.snis = ext.Upstreams[ext.ServiceName].SNI
	plugin.resources = make(map[string]*resource)

	return &plugin, resultErr
}

// Errors returns the error based only on Validate's state.
func (v *Validate) Errors(validateEndpoints bool, endpointValidator EndpointValidator, clusters *envoy_admin_v3.Clusters) error {
	var resultErr error
	if !v.listener {
		resultErr = multierror.Append(resultErr, fmt.Errorf("no listener"))
	}

	if v.usesRDS && !v.route {
		resultErr = multierror.Append(resultErr, fmt.Errorf("no route"))
	}

	numRequiredResources := 0
	// Resources will be marked as required in PatchFilter or PatchRoute because the listener or route will determine
	// which clusters/endpoints to validate.
	for sni, resource := range v.resources {
		if !resource.required {
			continue
		}
		numRequiredResources += 1

		_, ok := v.snis[sni]
		if !ok || !resource.cluster {
			resultErr = multierror.Append(resultErr, fmt.Errorf("no cluster for sni %s", sni))
			continue
		}

		if validateEndpoints {
			// If resource is a top-level cluster (any cluster that is an aggregate cluster or not a child of an aggregate
			// cluster), it will have an empty parent. If resource is a child cluster, it will have a nonempty parent.
			if resource.parentCluster == "" && resource.aggregateCluster {
				// Aggregate cluster case: do endpoint verification by checking each child cluster. We need at least one
				// child cluster to have healthy endpoints.
				oneClusterHasEndpoints := false
				for _, childCluster := range resource.aggregateClusterChildren {
					endpointValidator(v.resources[childCluster], childCluster, clusters)
					if v.resources[childCluster].endpoints > 0 {
						oneClusterHasEndpoints = true
					}
				}
				if !oneClusterHasEndpoints {
					resultErr = multierror.Append(resultErr, fmt.Errorf("zero healthy endpoints for aggregate cluster %s", sni))
				}
			} else if resource.parentCluster == "" {
				// Top-level non-aggregate cluster case: check for load assignment and healthy endpoints.
				endpointValidator(resource, sni, clusters)
				if resource.usesEDS && !resource.loadAssignment {
					resultErr = multierror.Append(resultErr, fmt.Errorf("no cluster load assignment for cluster %s", sni))
				}
				if resource.endpoints == 0 {
					resultErr = multierror.Append(resultErr, fmt.Errorf("zero healthy endpoints for cluster %s", sni))
				}
			} else {
				// Child cluster case: skip, since it'll be verified by the parent aggregate cluster.
				continue
			}
		}

	}

	if numRequiredResources == 0 {
		resultErr = multierror.Append(resultErr, fmt.Errorf("no clusters found on route or listener"))
	}

	return resultErr
}

// DoEndpointValidation implements the EndpointVerifier function type.
func DoEndpointValidation(r *resource, sni string, clusters *envoy_admin_v3.Clusters) {
	clusterStatuses := clusters.GetClusterStatuses()
	if clusterStatuses == nil {
		return
	}
	status := &envoy_admin_v3.ClusterStatus{}
	r.loadAssignment = false
	for _, s := range clusterStatuses {
		if s.Name == sni {
			status = s
			r.loadAssignment = true
			break
		}
	}

	healthyEndpoints := 0
	hostStatuses := status.GetHostStatuses()

	if r.loadAssignment && hostStatuses != nil {
		for _, h := range hostStatuses {
			health := h.GetHealthStatus()
			if health != nil {
				if health.EdsHealthStatus == envoy_core_v3.HealthStatus_HEALTHY && health.FailedOutlierCheck == false {
					healthyEndpoints += 1
				}
			}
		}
	}
	r.endpoints = healthyEndpoints
}

// CanApply determines if the extension can apply to the given extension configuration.
func (p *Validate) CanApply(config *extensioncommon.RuntimeConfig) bool {
	return true
}

func (p *Validate) PatchRoute(config *extensioncommon.RuntimeConfig, route *envoy_route_v3.RouteConfiguration) (*envoy_route_v3.RouteConfiguration, bool, error) {
	// Route name on connect proxies will be the envoy ID. We are only validating routes for the specific upstream with
	// the envoyID configured.
	if route.Name != p.envoyID {
		return route, false, nil
	}
	p.route = true
	for sni := range extensioncommon.RouteClusterNames(route) {
		if _, ok := p.resources[sni]; ok {
			continue
		}
		p.resources[sni] = &resource{required: true}
	}
	return route, false, nil
}

func (p *Validate) PatchCluster(config *extensioncommon.RuntimeConfig, c *envoy_cluster_v3.Cluster) (*envoy_cluster_v3.Cluster, bool, error) {
	v, ok := p.resources[c.Name]
	if !ok {
		v = &resource{}
		p.resources[c.Name] = v
	}
	v.cluster = true

	// If it's an aggregate cluster, add the child clusters to p.resources if they are not already there.
	aggregateCluster, ok := isAggregateCluster(c)
	if ok {
		// Mark this as an aggregate cluster, so we know we do not need to validate its endpoints directly.
		v.aggregateCluster = true
		for _, clusterName := range aggregateCluster.Clusters {
			r, ok := p.resources[clusterName]
			if !ok {
				r = &resource{}
				p.resources[clusterName] = r
			}
			if v.aggregateClusterChildren == nil {
				v.aggregateClusterChildren = []string{}
			}
			// On the parent cluster, add the children.
			v.aggregateClusterChildren = append(v.aggregateClusterChildren, clusterName)
			// On the child cluster, set the parent.
			r.parentCluster = c.Name
			// The child clusters of an aggregate cluster will be required if the parent cluster is.
			r.required = v.required
		}
		return c, false, nil
	}

	if c.EdsClusterConfig != nil {
		v.usesEDS = true
	} else {
		la := c.LoadAssignment
		if la == nil {
			return c, false, nil
		}
		v.endpoints = len(la.Endpoints) + len(la.NamedEndpoints)
	}
	return c, false, nil
}

func (p *Validate) PatchFilter(config *extensioncommon.RuntimeConfig, filter *envoy_listener_v3.Filter) (*envoy_listener_v3.Filter, bool, error) {
	// If a single filter exists for a listener we say it exists.
	p.listener = true

	if config := envoy_resource_v3.GetHTTPConnectionManager(filter); config != nil {
		// If the http filter uses RDS, then the clusters we need to validate exist in the route, and there's nothing
		// else we need to do with the filter.
		if config.GetRds() != nil {
			p.usesRDS = true
			return filter, true, nil
		}
	}

	// FilterClusterNames handles the filter being an http or tcp filter.
	for sni := range extensioncommon.FilterClusterNames(filter) {
		// Mark any clusters we see as required resources.
		if r, ok := p.resources[sni]; ok {
			r.required = true
		} else {
			p.resources[sni] = &resource{required: true}
		}
	}

	return filter, true, nil
}

func isAggregateCluster(c *envoy_cluster_v3.Cluster) (*envoy_aggregate_cluster_v3.ClusterConfig, bool) {
	aggregateCluster := &envoy_aggregate_cluster_v3.ClusterConfig{}
	cdt, ok := c.ClusterDiscoveryType.(*envoy_cluster_v3.Cluster_ClusterType)
	if ok {
		cct := cdt.ClusterType.TypedConfig
		if cct != nil {
			err := anypb.UnmarshalTo(cct, aggregateCluster, proto.UnmarshalOptions{})
			if err == nil {
				return aggregateCluster, true
			}
		}
	}
	return nil, false
}
