// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package watchers

import (
	"fmt"
	"strconv"

	"github.com/cilium/cilium/pkg/envoy"
	"github.com/cilium/cilium/pkg/loadbalancer"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/service"
	"github.com/cilium/cilium/pkg/slices"
)

// EnvoyServiceBackendSyncer syncs the backends of a Service as Endpoints to the Envoy L7 proxy.
type EnvoyServiceBackendSyncer struct {
	envoyXdsServer envoy.XDSServer

	l7lbSvcsMutex lock.RWMutex
	l7lbSvcs      map[loadbalancer.ServiceName]*backendSyncInfo
}

var _ service.BackendSyncer = &EnvoyServiceBackendSyncer{}

func (*EnvoyServiceBackendSyncer) ProxyName() string {
	return "Envoy"
}

func NewEnvoyServiceBackendSyncer(envoyXdsServer envoy.XDSServer) *EnvoyServiceBackendSyncer {
	return &EnvoyServiceBackendSyncer{
		envoyXdsServer: envoyXdsServer,
		l7lbSvcs:       map[loadbalancer.ServiceName]*backendSyncInfo{},
	}
}

func (r *EnvoyServiceBackendSyncer) Sync(svc *loadbalancer.SVC) error {
	r.l7lbSvcsMutex.RLock()
	l7lbInfo, exists := r.l7lbSvcs[svc.Name]
	r.l7lbSvcsMutex.RUnlock()

	if !exists {
		return nil
	}

	// Filter backend based on list of port numbers, then upsert backends
	// as Envoy endpoints
	be := filterServiceBackends(svc, l7lbInfo.GetAllFrontendPorts())

	log.
		WithField("filteredBackends", be).
		WithField(logfields.L7LBFrontendPorts, l7lbInfo.GetAllFrontendPorts()).
		Debug("Upsert envoy endpoints")
	if err := r.envoyXdsServer.UpsertEnvoyEndpoints(svc.Name, be); err != nil {
		return fmt.Errorf("failed to update backends in Envoy: %w", err)
	}

	return nil
}

func (r *EnvoyServiceBackendSyncer) RegisterServiceUsageInCEC(svcName loadbalancer.ServiceName, resourceName service.L7LBResourceName, frontendPorts []string) {
	r.l7lbSvcsMutex.Lock()
	defer r.l7lbSvcsMutex.Unlock()

	l7lbInfo, exists := r.l7lbSvcs[svcName]

	if !exists {
		l7lbInfo = &backendSyncInfo{}
	}

	if l7lbInfo.backendRefs == nil {
		l7lbInfo.backendRefs = make(map[service.L7LBResourceName]backendSyncCECInfo, 1)
	}

	l7lbInfo.backendRefs[resourceName] = backendSyncCECInfo{
		frontendPorts: frontendPorts,
	}

	r.l7lbSvcs[svcName] = l7lbInfo
}

func (r *EnvoyServiceBackendSyncer) DeregisterServiceUsageInCEC(svcName loadbalancer.ServiceName, resourceName service.L7LBResourceName) bool {
	r.l7lbSvcsMutex.Lock()
	defer r.l7lbSvcsMutex.Unlock()

	l7lbInfo, exists := r.l7lbSvcs[svcName]

	if !exists {
		return false
	}

	if l7lbInfo.backendRefs != nil {
		delete(l7lbInfo.backendRefs, resourceName)
	}

	// Cleanup service if it's no longer used by any CEC
	if len(l7lbInfo.backendRefs) == 0 {
		delete(r.l7lbSvcs, svcName)
		return true
	}

	r.l7lbSvcs[svcName] = l7lbInfo

	return false
}

// filterServiceBackends returns the list of backends based on given front end ports.
// The returned map will have key as port name/number, and value as list of respective backends.
func filterServiceBackends(svc *loadbalancer.SVC, onlyPorts []string) map[string][]*loadbalancer.Backend {
	if len(onlyPorts) == 0 {
		return map[string][]*loadbalancer.Backend{
			"*": filterPreferredBackends(svc.Backends),
		}
	}

	res := map[string][]*loadbalancer.Backend{}
	for _, port := range onlyPorts {
		// check for port number
		if port == strconv.Itoa(int(svc.Frontend.Port)) {
			return map[string][]*loadbalancer.Backend{
				port: filterPreferredBackends(svc.Backends),
			}
		}
		// check for either named port
		for _, backend := range filterPreferredBackends(svc.Backends) {
			if port == backend.FEPortName {
				res[port] = append(res[port], backend)
			}
		}
	}

	return res
}

// filterPreferredBackends returns the slice of backends which are preferred for the given service.
// If there is no preferred backend, it returns the slice of all backends.
func filterPreferredBackends(backends []*loadbalancer.Backend) []*loadbalancer.Backend {
	res := []*loadbalancer.Backend{}
	for _, backend := range backends {
		if backend.Preferred == loadbalancer.Preferred(true) {
			res = append(res, backend)
		}
	}
	if len(res) > 0 {
		return res
	}

	return backends
}

type backendSyncInfo struct {
	// Names of the L7 LB resources (e.g. CEC) that need this service's backends to be
	// synced to to an L7 Loadbalancer.
	backendRefs map[service.L7LBResourceName]backendSyncCECInfo
}

func (r *backendSyncInfo) GetAllFrontendPorts() []string {
	allPorts := []string{}

	for _, info := range r.backendRefs {
		allPorts = append(allPorts, info.frontendPorts...)
	}

	return slices.SortedUnique(allPorts)
}

type backendSyncCECInfo struct {
	// List of front-end ports of upstream service/cluster, which will be used for
	// filtering applicable endpoints.
	//
	// If nil, all the available backends will be used.
	frontendPorts []string
}