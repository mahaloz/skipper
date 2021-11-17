package definitions

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
)

type IngressV1List struct {
	Items []*IngressV1Item `json:"items"`
}

type IngressV1Item struct {
	Metadata *Metadata      `json:"metadata"`
	Spec     *IngressV1Spec `json:"spec"`
}

// IngressSpecV1 https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#ingressspec-v1-networking-k8s-io
type IngressV1Spec struct {
	DefaultBackend   *BackendV1 `json:"defaultBackend,omitempty"`
	IngressClassName string     `json:"ingressClassName,omitempty"`
	Rules            []*RuleV1  `json:"rules"`
	// Ingress TLS not supported: https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#ingressspec-v1-networking-k8s-io
}

// BackendV1 https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#ingressbackend-v1-networking-k8s-io
type BackendV1 struct {
	Service Service `json:"service,omitempty"` // can be nil, because of TypedLocalObjectReference
	// Resource TypedLocalObjectReference is not supported https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#typedlocalobjectreference-v1-core

	// Traffic field used for custom traffic weights, but not part of the ingress spec.
	Traffic IngressBackendTraffic
}

func (b *BackendV1) GetServiceName() string {
	return b.Service.Name
}

func (b *BackendV1) GetServicePort() string {
	return b.Service.Port.String()
}

func (b *BackendV1) GetTraffic() *IngressBackendTraffic {
	return &b.Traffic
}

// Service https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#ingressservicebackend-v1-networking-k8s-io
type Service struct {
	Name string        `json:"name"`
	Port BackendPortV1 `json:"port"`
}

type BackendPortV1 struct {
	Name   string `json:"name"`
	Number int    `json:"number"`
}

func (p BackendPortV1) String() string {
	if p.Number != 0 {
		return strconv.Itoa(p.Number)
	}
	return p.Name
}

type RuleV1 struct {
	Host string      `json:"host"`
	Http *HTTPRuleV1 `json:"http"`
}

func (r *RuleV1) GetHost() string {
	return r.Host
}

func (r *RuleV1) GetPathRules() []IngressPathRule {
	if r.Http == nil {
		return nil
	}
	ret := make([]IngressPathRule, len(r.Http.Paths))
	for i, pr := range r.Http.Paths {
		ret[i] = pr
	}
	return ret
}

type HTTPRuleV1 struct {
	Paths []*PathRuleV1 `json:"paths"`
}

// PathRuleV1 https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.22/#httpingresspath-v1-networking-k8s-io
type PathRuleV1 struct {
	Path     string     `json:"path"`
	PathType string     `json:"pathType"`
	Backend  *BackendV1 `json:"backend"`
}

func (r *PathRuleV1) GetPath() string {
	return r.Path
}

func (r *PathRuleV1) GetPathType() string {
	return r.PathType
}

func (r *PathRuleV1) GetBackend() IngressBackend {
	return r.Backend
}

// ParseIngressV1JSON parse JSON into an IngressV1List
func ParseIngressV1JSON(d []byte) (IngressV1List, error) {
	var il IngressV1List
	err := json.Unmarshal(d, &il)
	return il, err
}

// ParseIngressV1YAML parse YAML into an IngressV1List
func ParseIngressV1YAML(d []byte) (IngressV1List, error) {
	var il IngressV1List
	err := yaml.Unmarshal(d, &il)
	return il, err
}

// TODO: implement once IngressItem has a validate method
// ValidateIngressV1 is a no-op
func ValidateIngressV1(_ *IngressV1Item) error {
	return nil
}

// ValidateIngresses is a no-op
func ValidateIngressesV1(ingressList IngressV1List) error {
	var err error
	// discover all errors to avoid the user having to repeatedly validate
	for _, i := range ingressList.Items {
		nerr := ValidateIngressV1(i)
		if nerr != nil {
			name := i.Metadata.Name
			namespace := i.Metadata.Namespace
			nerr = fmt.Errorf("%s/%s: %w", name, namespace, nerr)
			err = errors.Wrap(err, nerr.Error())
		}
	}

	if err != nil {
		return err
	}

	return nil
}
