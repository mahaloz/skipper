package kubernetes

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/zalando/skipper/dataclients/kubernetes/definitions"
	"github.com/zalando/skipper/eskip"
)

func setPath(m PathMode, r *eskip.Route, prule definitions.IngressPathRule) {
	path := prule.GetPath()
	// see https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#httpingresspath-v1-networking-k8s-io
	switch prule.GetPathType() {
	case "Exact":
		r.Predicates = append(r.Predicates, &eskip.Predicate{
			Name: "Path",
			Args: []interface{}{path},
		})
	case "Prefix":
		r.Predicates = append(r.Predicates, &eskip.Predicate{
			Name: "PathSubtree",
			Args: []interface{}{path},
		})
	case "ImplementationSpecific":
		if path == "" {
			return
		}
		switch m {
		case PathPrefix:
			r.Predicates = append(r.Predicates, &eskip.Predicate{
				Name: "PathSubtree",
				Args: []interface{}{path},
			})
		case PathRegexp:
			r.PathRegexps = []string{path}
		default:
			if path == "/" {
				r.PathRegexps = []string{"^/"}
			} else {
				r.PathRegexps = []string{"^(" + path + ")"}
			}
		}
	}
}

func convertPathRule(
	state *clusterState,
	metadata *definitions.Metadata,
	host string,
	prule definitions.IngressPathRule,
	pathMode PathMode,
	allowedExternalNames []*regexp.Regexp,
) (*eskip.Route, error) {

	ns := metadata.Namespace
	name := metadata.Name

	if prule.GetBackend() == nil {
		return nil, fmt.Errorf("invalid path rule, missing backend in: %s/%s/%s", ns, name, host)
	}

	var (
		eps []string
		err error
		svc *service
	)

	var hostRegexp []string
	if host != "" {
		hostRegexp = []string{createHostRx(host)}
	}
	svcPort := prule.GetBackend().GetServicePort()
	svcName := prule.GetBackend().GetServiceName()

	svc, err = state.getService(ns, svcName)
	if err != nil {
		log.Errorf("convertPathRule: Failed to get service %s, %s, %s", ns, svcName, svcPort)
		return nil, err
	}

	servicePort, err := svc.getServicePort(svcPort)
	if err != nil {
		// service definition is wrong or no pods
		err = nil
		if len(eps) > 0 {
			// should never happen
			log.Errorf("convertPathRule: Failed to find target port for service %s, but %d endpoints exist. Kubernetes has inconsistent data", svcName, len(eps))
		}
	} else if svc.Spec.Type == "ExternalName" {
		return externalNameRoute(ns, name, host, hostRegexp, svc, servicePort, allowedExternalNames)
	} else {
		protocol := "http"
		if p, ok := metadata.Annotations[skipperBackendProtocolAnnotationKey]; ok {
			protocol = p
		}

		eps = state.getEndpointsByService(ns, svcName, protocol, servicePort)
		log.Debugf("convertPathRule: Found %d endpoints %s for %s", len(eps), servicePort, svcName)
	}
	if len(eps) == 0 {
		// add shunt route https://github.com/zalando/skipper/issues/1525
		log.Debugf("convertPathRule: add shuntroute to return 502 for ingress %s/%s service %s with %d endpoints", ns, name, svcName, len(eps))
		r := &eskip.Route{
			Id:          routeID(ns, name, host, prule.GetPath(), svcName),
			HostRegexps: hostRegexp,
		}

		setPath(pathMode, r, prule)
		setTraffic(r, svcName, prule.GetBackend().GetTraffic())
		shuntRoute(r)
		return r, nil
	}

	log.Debugf("convertPathRule: %d routes for %s/%s/%s", len(eps), ns, svcName, svcPort)
	if len(eps) == 1 {
		r := &eskip.Route{
			Id:          routeID(ns, name, host, prule.GetPath(), svcName),
			Backend:     eps[0],
			HostRegexps: hostRegexp,
		}

		setPath(pathMode, r, prule)
		setTraffic(r, svcName, prule.GetBackend().GetTraffic())
		return r, nil
	}

	r := &eskip.Route{
		Id:          routeID(ns, name, host, prule.GetPath(), svcName),
		BackendType: eskip.LBBackend,
		LBEndpoints: eps,
		LBAlgorithm: getLoadBalancerAlgorithm(metadata),
		HostRegexps: hostRegexp,
	}
	setPath(pathMode, r, prule)
	setTraffic(r, svcName, prule.GetBackend().GetTraffic())
	return r, nil
}

func (ing *ingress) addEndpointsRule(ic ingressContext, host string, prule definitions.IngressPathRule) error {
	meta := ic.metadata
	endpointsRoute, err := convertPathRule(
		ic.state,
		meta,
		host,
		prule,
		ic.pathMode,
		ing.allowedExternalNames,
	)
	if err != nil {
		// if the service is not found the route should be removed
		if err == errServiceNotFound || err == errResourceNotFound {
			return nil
		}

		// TODO: this error checking should not really be used, and the error handling of the ingress
		// problems should be refactored such that a single ingress's error doesn't block the
		// processing of the independent ingresses.
		if errors.Is(err, errNotAllowedExternalName) {
			log.Infof("Not allowed external name: %v", err)
			return nil
		}

		// Ingress status field does not support errors
		return fmt.Errorf("error while getting service: %v", err)
	}

	// safe prepend, see: https://play.golang.org/p/zg5aGKJpRyK
	filters := make([]*eskip.Filter, len(endpointsRoute.Filters)+len(ic.annotationFilters))
	copy(filters, ic.annotationFilters)
	copy(filters[len(ic.annotationFilters):], endpointsRoute.Filters)
	endpointsRoute.Filters = filters

	// add pre-configured default filters
	df, err := ic.defaultFilters.getNamed(meta.Namespace, prule.GetBackend().GetServiceName())
	if err != nil {
		ic.logger.Errorf("Failed to retrieve default filters: %v.", err)
	} else {
		// it's safe to prepend, because type defaultFilters copies the slice during get()
		endpointsRoute.Filters = append(df, endpointsRoute.Filters...)
	}

	err = applyAnnotationPredicates(ic.pathMode, endpointsRoute, ic.annotationPredicate)
	if err != nil {
		ic.logger.Errorf("failed to apply annotation predicates: %v", err)
	}
	ic.addHostRoute(host, endpointsRoute)

	redirect := ic.redirect
	ewRangeMatch := false
	for _, s := range ing.eastWestRangeDomains {
		if strings.HasSuffix(host, s) {
			ewRangeMatch = true
			break
		}
	}
	if !(ewRangeMatch || strings.HasSuffix(host, ing.kubernetesEastWestDomain) && ing.kubernetesEastWestDomain != "") {
		switch {
		case redirect.ignore:
			// no redirect
		case redirect.enable:
			ic.addHostRoute(host, createIngressEnableHTTPSRedirect(endpointsRoute, redirect.code))
			redirect.setHost(host)
		case redirect.disable:
			ic.addHostRoute(host, createIngressDisableHTTPSRedirect(endpointsRoute))
			redirect.setHostDisabled(host)
		case redirect.defaultEnabled:
			ic.addHostRoute(host, createIngressEnableHTTPSRedirect(endpointsRoute, redirect.code))
			redirect.setHost(host)
		}
	}

	if ing.kubernetesEnableEastWest {
		ewRoute := createEastWestRouteIng(ing.kubernetesEastWestDomain, meta.Name, meta.Namespace, endpointsRoute)
		ewHost := fmt.Sprintf("%s.%s.%s", meta.Name, meta.Namespace, ing.kubernetesEastWestDomain)
		ic.addHostRoute(ewHost, ewRoute)
	}
	return nil
}

// computeBackendWeights computes and sets the backend traffic weights on the
// rule backends.
// The traffic is calculated based on the following rules:
//
// * if no weight is defined for a backend it will get weight 0.
// * if no weights are specified for all backends of a path, then traffic will
//   be distributed equally.
//
// Each traffic weight is relative to the number of backends per path. If there
// are multiple backends per path the weight will be relative to the number of
// remaining backends for the path e.g. if the weight is specified as
//
//      backend-1: 0.2
//      backend-2: 0.6
//      backend-3: 0.2
//
// then the weight will be calculated to:
//
//      backend-1: 0.2
//      backend-2: 0.75
//      backend-3: 1.0
//
// where for a weight of 1.0 no Traffic predicate will be generated.
func computeBackendWeights(backendWeights map[string]float64, paths []definitions.IngressPathRule) {
	type pathInfo struct {
		sum          float64
		lastActive   definitions.IngressBackend
		count        int
		weightsCount int
	}

	// get backend weight sum and count of backends for all paths
	pathInfos := make(map[string]*pathInfo)
	for _, path := range paths {
		sc, ok := pathInfos[path.GetPath()]
		if !ok {
			sc = &pathInfo{}
			pathInfos[path.GetPath()] = sc
		}

		if weight, ok := backendWeights[path.GetBackend().GetServiceName()]; ok {
			sc.sum += weight
			if weight > 0 {
				sc.lastActive = path.GetBackend()
				sc.weightsCount++
			}
		} else {
			sc.count++
		}
	}

	// calculate traffic weight for each backend
	for _, path := range paths {
		if sc, ok := pathInfos[path.GetPath()]; ok {
			if weight, ok := backendWeights[path.GetBackend().GetServiceName()]; ok {
				// force a weight of 1.0 for the last backend with a non-zero weight to avoid rounding issues
				if sc.lastActive == path.GetBackend() {
					path.GetBackend().GetTraffic().Weight = 1.0
					continue
				}

				path.GetBackend().GetTraffic().Weight = weight / sc.sum
				// subtract weight from the sum in order to
				// give subsequent backends a higher relative
				// weight.
				sc.sum -= weight

				// noops are required to make sure that routes are in order selected by
				// routing tree
				if sc.weightsCount > 2 {
					path.GetBackend().GetTraffic().NoopCount = sc.weightsCount - 2
				}
				sc.weightsCount--
			} else if sc.sum == 0 && sc.count > 0 {
				path.GetBackend().GetTraffic().Weight = 1.0 / float64(sc.count)
			}
			// reduce count by one in order to give subsequent
			// backends for the path a higher relative weight.
			sc.count--
		}
	}
}

// TODO: default filters not applied to 'extra' routes from the custom route annotations. Is it on purpose?
// https://github.com/zalando/skipper/issues/1287
func (ing *ingress) addSpecRule(ic ingressContext, ru definitions.IngressHTTPHostRule) error {
	host, pathRules := ru.GetHost(), ru.GetPathRules()
	if pathRules == nil {
		ic.logger.Warn("invalid ingress item: rule missing http definitions")
		return nil
	}
	// update Traffic field for each backend
	computeBackendWeights(ic.backendWeights, pathRules)
	for _, prule := range pathRules {
		addExtraRoutes(ic, host, prule, ing.kubernetesEastWestDomain, ing.kubernetesEnableEastWest)
		if prule.GetBackend().GetTraffic().Weight > 0 {
			err := ing.addEndpointsRule(ic, host, prule)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// converts the default backend if any
func (ing *ingress) convertDefaultBackend(
	state *clusterState,
	backend definitions.IngressBackend,
	metadata *definitions.Metadata,
) (*eskip.Route, bool, error) {
	var (
		eps     []string
		err     error
		ns      = metadata.Namespace
		name    = metadata.Name
		svcName = backend.GetServiceName()
		svcPort = backend.GetServicePort()
	)

	svc, err := state.getService(ns, svcName)
	if err != nil {
		log.Errorf("convertDefaultBackend: Failed to get service %s, %s, %s", ns, svcName, svcPort)
		return nil, false, err
	}

	servicePort, err := svc.getServicePort(svcPort)
	if err != nil {
		log.Errorf("convertDefaultBackend: Failed to find target port %v, %s, for ingress %s/%s and service %s add shuntroute: %v", svc.Spec.Ports, svcPort, ns, name, svcName, err)
		err = nil
	} else if svc.Spec.Type == "ExternalName" {
		r, err := externalNameRoute(ns, name, "default", nil, svc, servicePort, ing.allowedExternalNames)
		return r, err == nil, err
	} else {
		log.Debugf("convertDefaultBackend: Found target port %v, for service %s", servicePort.TargetPort, svcName)
		protocol := "http"
		if p, ok := metadata.Annotations[skipperBackendProtocolAnnotationKey]; ok {
			protocol = p
		}

		eps = state.getEndpointsByService(
			ns,
			svcName,
			protocol,
			servicePort,
		)
		log.Debugf("convertDefaultBackend: Found %d endpoints for %s: %v", len(eps), svcName, err)
	}

	if len(eps) == 0 {
		// add shunt route https://github.com/zalando/skipper/issues/1525
		log.Debugf("convertDefaultBackend: add shuntroute to return 502 for ingress %s/%s service %s with %d endpoints", ns, name, svcName, len(eps))
		r := &eskip.Route{
			Id: routeID(ns, name, "", "", ""),
		}
		shuntRoute(r)
		return r, true, nil
	} else if len(eps) == 1 {
		return &eskip.Route{
			Id:      routeID(ns, name, "", "", ""),
			Backend: eps[0],
		}, true, nil
	}

	return &eskip.Route{
		Id:          routeID(ns, name, "", "", ""),
		BackendType: eskip.LBBackend,
		LBEndpoints: eps,
		LBAlgorithm: getLoadBalancerAlgorithm(metadata),
	}, true, nil
}

func (ing *ingress) ingressRoute(
	i *definitions.IngressItem,
	redirect *redirectInfo,
	state *clusterState,
	hostRoutes map[string][]*eskip.Route,
	df defaultFilters,
) (*eskip.Route, error) {
	if i.Metadata == nil || i.Metadata.Namespace == "" || i.Metadata.Name == "" || i.Spec == nil {
		log.Error("invalid ingress item: missing Metadata or Spec")
		return nil, nil
	}
	logger := log.WithFields(log.Fields{
		"ingress": fmt.Sprintf("%s/%s", i.Metadata.Namespace, i.Metadata.Name),
	})
	redirect.initCurrent(i.Metadata)
	ic := ingressContext{
		state:               state,
		metadata:            i.Metadata,
		logger:              logger,
		annotationFilters:   annotationFilter(i.Metadata, logger),
		annotationPredicate: annotationPredicate(i.Metadata),
		extraRoutes:         extraRoutes(i.Metadata, logger),
		backendWeights:      backendWeights(i.Metadata, logger),
		pathMode:            pathMode(i.Metadata, ing.pathMode),
		redirect:            redirect,
		hostRoutes:          hostRoutes,
		defaultFilters:      df,
	}

	var route *eskip.Route

	// the usage of the default backend depends on what we want
	// we can generate a hostname out of it based on shared rules
	// and instructions in annotations, if there are no rules defined
	// this is a flaw in the ingress API design, because it is not on the hosts' level, but the spec
	// tells to match if no rule matches. This means that there is no matching rule on this ingress
	// and if there are multiple ingress items, then there is a race between them.
	if i.Spec.DefaultBackend != nil {
		if r, ok, err := ing.convertDefaultBackend(state, i.Spec.DefaultBackend, i.Metadata); ok {
			route = r
		} else if err != nil {
			ic.logger.Errorf("error while converting default backend: %v", err)
		}
	}
	for _, rule := range i.Spec.Rules {
		err := ing.addSpecRule(ic, rule)
		if err != nil {
			return nil, err
		}
	}
	return route, nil
}
