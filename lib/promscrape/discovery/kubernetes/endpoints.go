package kubernetes

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promscrape/discoveryutils"
)

// EndpointsList implements k8s endpoints list.
//
// See https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.17/#endpointslist-v1-core
type EndpointsList struct {
	Items    []Endpoints
	Metadata listMetadata `json:"metadata"`
}

// Endpoints implements k8s endpoints.
//
// See https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.17/#endpoints-v1-core
type Endpoints struct {
	Metadata ObjectMeta
	Subsets  []EndpointSubset
}

func (eps *Endpoints) key() string {
	return eps.Metadata.Namespace + "/" + eps.Metadata.Name
}

// EndpointSubset implements k8s endpoint subset.
//
// See https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.17/#endpointsubset-v1-core
type EndpointSubset struct {
	Addresses         []EndpointAddress
	NotReadyAddresses []EndpointAddress
	Ports             []EndpointPort
}

// EndpointAddress implements k8s endpoint address.
//
// See https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.17/#endpointaddress-v1-core
type EndpointAddress struct {
	Hostname  string
	IP        string
	NodeName  string
	TargetRef ObjectReference
}

// ObjectReference implements k8s object reference.
//
// See https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.17/#objectreference-v1-core
type ObjectReference struct {
	Kind      string
	Name      string
	Namespace string
}

func (or ObjectReference) key() string {
	return or.Namespace + "/" + or.Name
}

// EndpointPort implements k8s endpoint port.
//
// See https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.17/#endpointport-v1beta1-discovery-k8s-io
type EndpointPort struct {
	AppProtocol string
	Name        string
	Port        int
	Protocol    string
}

// parseEndpointsList parses EndpointsList from data.
func parseEndpointsList(data []byte) (*EndpointsList, error) {
	var esl EndpointsList
	if err := json.Unmarshal(data, &esl); err != nil {
		return nil, fmt.Errorf("cannot unmarshal EndpointsList from %q: %w", data, err)
	}
	return &esl, nil
}

// appendTargetLabels appends labels for each endpoint in eps to ms and returns the result.
//
// See https://prometheus.io/docs/prometheus/latest/configuration/configuration/#endpoints
func (eps *Endpoints) appendTargetLabels(ms []map[string]string, podsCache, servicesCache *sync.Map) []map[string]string {
	var svc *Service
	if svco, ok := servicesCache.Load(eps.key()); ok {
		svc = svco.(*Service)
	}
	podPortsSeen := make(map[*Pod][]int)
	for _, ess := range eps.Subsets {
		for _, epp := range ess.Ports {
			ms = appendEndpointLabelsForAddresses(ms, podPortsSeen, eps, ess.Addresses, epp, podsCache, svc, "true")
			ms = appendEndpointLabelsForAddresses(ms, podPortsSeen, eps, ess.NotReadyAddresses, epp, podsCache, svc, "false")
		}
	}

	// Append labels for skipped ports on seen pods.
	portSeen := func(port int, ports []int) bool {
		for _, p := range ports {
			if p == port {
				return true
			}
		}
		return false
	}
	for p, ports := range podPortsSeen {
		for _, c := range p.Spec.Containers {
			for _, cp := range c.Ports {
				if portSeen(cp.ContainerPort, ports) {
					continue
				}
				addr := discoveryutils.JoinHostPort(p.Status.PodIP, cp.ContainerPort)
				m := map[string]string{
					"__address__": addr,
				}
				p.appendCommonLabels(m)
				p.appendContainerLabels(m, c, &cp)
				if svc != nil {
					svc.appendCommonLabels(m)
				}
				ms = append(ms, m)
			}
		}
	}
	return ms
}

func appendEndpointLabelsForAddresses(ms []map[string]string, podPortsSeen map[*Pod][]int, eps *Endpoints, eas []EndpointAddress, epp EndpointPort,
	podsCache *sync.Map, svc *Service, ready string) []map[string]string {
	for _, ea := range eas {
		var p *Pod
		if po, ok := podsCache.Load(ea.TargetRef.key()); ok {
			p = po.(*Pod)
		}
		//p := getPod(pods, ea.TargetRef.Namespace, ea.TargetRef.Name)
		m := getEndpointLabelsForAddressAndPort(podPortsSeen, eps, ea, epp, p, svc, ready)
		ms = append(ms, m)
	}
	return ms
}

func getEndpointLabelsForAddressAndPort(podPortsSeen map[*Pod][]int, eps *Endpoints, ea EndpointAddress, epp EndpointPort, p *Pod, svc *Service, ready string) map[string]string {
	m := getEndpointLabels(eps.Metadata, ea, epp, ready)
	if svc != nil {
		svc.appendCommonLabels(m)
	}
	eps.Metadata.registerLabelsAndAnnotations("__meta_kubernetes_endpoints", m)
	if ea.TargetRef.Kind != "Pod" || p == nil {
		return m
	}
	p.appendCommonLabels(m)
	for _, c := range p.Spec.Containers {
		for _, cp := range c.Ports {
			if cp.ContainerPort == epp.Port {
				p.appendContainerLabels(m, c, &cp)
				podPortsSeen[p] = append(podPortsSeen[p], cp.ContainerPort)
				break
			}
		}
	}
	return m
}

func getEndpointLabels(om ObjectMeta, ea EndpointAddress, epp EndpointPort, ready string) map[string]string {
	addr := discoveryutils.JoinHostPort(ea.IP, epp.Port)
	m := map[string]string{
		"__address__":                      addr,
		"__meta_kubernetes_namespace":      om.Namespace,
		"__meta_kubernetes_endpoints_name": om.Name,

		"__meta_kubernetes_endpoint_ready":         ready,
		"__meta_kubernetes_endpoint_port_name":     epp.Name,
		"__meta_kubernetes_endpoint_port_protocol": epp.Protocol,
	}
	if ea.TargetRef.Kind != "" {
		m["__meta_kubernetes_endpoint_address_target_kind"] = ea.TargetRef.Kind
		m["__meta_kubernetes_endpoint_address_target_name"] = ea.TargetRef.Name
	}
	if ea.NodeName != "" {
		m["__meta_kubernetes_endpoint_node_name"] = ea.NodeName
	}
	if ea.Hostname != "" {
		m["__meta_kubernetes_endpoint_hostname"] = ea.Hostname
	}
	return m
}

func processEndpoints(cfg *apiConfig, sc *SharedKubernetesCache, p *Endpoints, action string) {
	key := buildSyncKey("endpoints", cfg.setName, p.key())
	switch action {
	case "ADDED", "MODIFIED":
		lbs := p.appendTargetLabels(nil, sc.Pods, sc.Services)
		cfg.targetChan <- SyncEvent{
			Labels:           lbs,
			Key:              key,
			ConfigSectionSet: cfg.setName,
		}
	case "DELETED":
		cfg.targetChan <- SyncEvent{
			Key:              key,
			ConfigSectionSet: cfg.setName,
		}
	case "ERROR":
	default:
		logger.Warnf("unexpected action: %s", action)
	}
}
