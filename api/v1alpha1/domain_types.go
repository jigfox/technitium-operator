package v1alpha1

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ServerReference names a TechnitiumServer in the same namespace.
type ServerReference struct {
	// name of the TechnitiumServer.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// DomainSpec is the desired state of an allowed or blocked domain.
type DomainSpec struct {
	// serverRef is the TechnitiumServer the entry belongs to. In a Technitium
	// cluster, point it at the primary node only: the secondaries receive the
	// entry from there.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="serverRef is immutable"
	// +required
	ServerRef ServerReference `json:"serverRef"`

	// domain name. Case and a trailing dot do not matter (the operator
	// lowercases it and strips the dot). Technitium treats an entry as the
	// domain and all its subdomains.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=254
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_]([A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?(\.[A-Za-z0-9_]([A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?)*\.?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="domain is immutable"
	// +required
	Domain string `json:"domain"`

	// reason is free text explaining the entry. It is shown by kubectl get and
	// not sent to Technitium.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Reason string `json:"reason,omitempty"`

	// deletionPolicy decides whether deleting this resource also deletes the
	// entry in Technitium (Delete, the default) or leaves it (Orphan).
	// +kubebuilder:default=Delete
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// DomainStatus is the observed state of an allowed or blocked domain.
type DomainStatus struct {
	// domain is the normalised name of the entry as stored in Technitium.
	// +optional
	Domain string `json:"domain,omitempty"`

	// adopted is true if the entry already existed in Technitium when the
	// operator first reconciled this resource.
	// +optional
	Adopted bool `json:"adopted,omitempty"`

	// observedGeneration is the generation the conditions were computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions: Ready, and DryRun while the operator runs with --dry-run.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NormalizeDomain lowercases a domain name and strips a trailing dot, the form
// Technitium stores.
func NormalizeDomain(d string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
}

// EffectiveDeletionPolicy is the deletion policy, or Delete if unset.
func (s *DomainSpec) EffectiveDeletionPolicy() DeletionPolicy {
	if s.DeletionPolicy == "" {
		return DeletionPolicyDelete
	}
	return s.DeletionPolicy
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Domain",type=string,JSONPath=`.spec.domain`
// +kubebuilder:printcolumn:name="Server",type=string,JSONPath=`.spec.serverRef.name`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.spec.reason`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Adopted",type=boolean,JSONPath=`.status.adopted`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TechnitiumAllowedDomain manages one entry of Technitium's allowed zones.
type TechnitiumAllowedDomain struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec DomainSpec `json:"spec"`

	// +optional
	Status DomainStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// TechnitiumAllowedDomainList contains a list of TechnitiumAllowedDomain.
type TechnitiumAllowedDomainList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []TechnitiumAllowedDomain `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Domain",type=string,JSONPath=`.spec.domain`
// +kubebuilder:printcolumn:name="Server",type=string,JSONPath=`.spec.serverRef.name`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.spec.reason`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Adopted",type=boolean,JSONPath=`.status.adopted`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TechnitiumBlockedDomain manages one entry of Technitium's blocked zones.
type TechnitiumBlockedDomain struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec DomainSpec `json:"spec"`

	// +optional
	Status DomainStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// TechnitiumBlockedDomainList contains a list of TechnitiumBlockedDomain.
type TechnitiumBlockedDomainList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []TechnitiumBlockedDomain `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion,
			&TechnitiumAllowedDomain{}, &TechnitiumAllowedDomainList{},
			&TechnitiumBlockedDomain{}, &TechnitiumBlockedDomainList{})
		return nil
	})
}
