package risk

import "strings"

// clusterScoped lists built-in kinds that have no namespace, keyed by group/Kind.
var clusterScoped = map[string]bool{
	"Namespace":        true,
	"Node":             true,
	"PersistentVolume": true,
	"apiextensions.k8s.io/CustomResourceDefinition":                 true,
	"rbac.authorization.k8s.io/ClusterRole":                         true,
	"rbac.authorization.k8s.io/ClusterRoleBinding":                  true,
	"storage.k8s.io/StorageClass":                                   true,
	"storage.k8s.io/CSIDriver":                                      true,
	"scheduling.k8s.io/PriorityClass":                               true,
	"networking.k8s.io/IngressClass":                                true,
	"gateway.networking.k8s.io/GatewayClass":                        true,
	"admissionregistration.k8s.io/ValidatingWebhookConfiguration":   true,
	"admissionregistration.k8s.io/MutatingWebhookConfiguration":     true,
	"admissionregistration.k8s.io/ValidatingAdmissionPolicy":        true,
	"admissionregistration.k8s.io/ValidatingAdmissionPolicyBinding": true,
	"admissionregistration.k8s.io/MutatingAdmissionPolicy":          true,
	"admissionregistration.k8s.io/MutatingAdmissionPolicyBinding":   true,
	"kubeamber.dev/ChangeRequest":                                   true,
}

// IsClusterScoped reports whether a built-in kind is cluster-scoped. It is used
// when evaluating manifests offline, without API discovery.
func IsClusterScoped(group, kind string) bool {
	gk := kind
	if group != "" {
		gk = group + "/" + kind
	}
	return clusterScoped[gk]
}

// parentKinds maps resources that expose a scale subresource to their kind.
var parentKinds = map[string][2]string{
	"deployments":     {"apps", "Deployment"},
	"statefulsets":    {"apps", "StatefulSet"},
	"replicasets":     {"apps", "ReplicaSet"},
	"pods":            {"", "Pod"},
	"serviceaccounts": {"", "ServiceAccount"},
}

// ParentKind returns the group and kind of the resource a subresource request
// targets. kind/group are the request's own values, returned unchanged when
// there is no subresource or the resource is unknown.
func ParentKind(resource, subResource, group, kind string) (string, string) {
	if subResource == "" {
		return group, kind
	}
	if gk, ok := parentKinds[strings.ToLower(resource)]; ok {
		return gk[0], gk[1]
	}
	return group, kind
}
