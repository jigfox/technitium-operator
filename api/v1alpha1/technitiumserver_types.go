package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// CredentialsSecretRef points at the Secret (in the namespace of the
// TechnitiumServer) that holds the login of a Technitium account.
//
// With tokenKey the operator uses a non-expiring API token (created in
// Technitium under "My Profile") instead of logging in with a password.
type CredentialsSecretRef struct {
	// name of the Secret.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// username is the account name as a literal. If set, usernameKey is not
	// used.
	// +optional
	Username string `json:"username,omitempty"`

	// usernameKey is the key in the Secret that holds the account name.
	// +kubebuilder:default=username
	// +optional
	UsernameKey string `json:"usernameKey,omitempty"`

	// passwordKey is the key in the Secret that holds the password.
	// +kubebuilder:default=password
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`

	// tokenKey is the key in the Secret that holds an API token. If set, the
	// token is used and username and password are ignored.
	// +optional
	TokenKey string `json:"tokenKey,omitempty"`
}

// PruneUnmanaged lists the domain lists from which the operator deletes every
// entry that no TechnitiumAllowedDomain / TechnitiumBlockedDomain of this
// server covers.
type PruneUnmanaged struct {
	// allowedDomains prunes the allowed zones.
	// +optional
	AllowedDomains bool `json:"allowedDomains,omitempty"`

	// blockedDomains prunes the blocked zones (the manually added ones, not
	// the domains of the block lists).
	// +optional
	BlockedDomains bool `json:"blockedDomains,omitempty"`
}

// ServerSettings is the part of Technitium's settings the operator manages.
// Every field is optional: a field that is not set is left alone. More
// settings can be added here without breaking existing resources.
type ServerSettings struct {
	// blockListUrls are the block list URLs Technitium downloads. They replace
	// the live list; an empty list clears it. Technitium re-downloads the
	// lists whenever they change, so the operator writes only on a difference.
	// +optional
	BlockListURLs *[]string `json:"blockListUrls,omitempty"`
}

// AppConfig is the partial configuration of an installed DNS app.
type AppConfig struct {
	// name of the installed app, e.g. "Query Logs (Sqlite)".
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// config holds the keys to set in the app's configuration. They are merged
	// (top-level keys) over the live configuration; keys that are not listed
	// keep their value.
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	// +required
	Config apiextensionsv1.JSON `json:"config"`
}

// TechnitiumServerSpec defines the desired state of TechnitiumServer.
type TechnitiumServerSpec struct {
	// url is the base URL of the Technitium web service, for example
	// http://technitium-0.dns.svc:5380.
	// +kubebuilder:validation:Pattern=`^https?://[^\s/]+(/\S*)?$`
	// +required
	URL string `json:"url"`

	// credentialsSecretRef selects the login.
	// +required
	CredentialsSecretRef CredentialsSecretRef `json:"credentialsSecretRef"`

	// pruneUnmanaged deletes entries that no domain resource covers. Off by
	// default.
	// +optional
	PruneUnmanaged PruneUnmanaged `json:"pruneUnmanaged,omitzero"`

	// settings to manage. Not set: the settings are not touched.
	// +optional
	Settings *ServerSettings `json:"settings,omitempty"`

	// apps are the configurations of installed DNS apps to manage. App
	// configurations are not synced between the nodes of a Technitium
	// cluster, so list them on the TechnitiumServer of every node.
	// +listType=map
	// +listMapKey=name
	// +optional
	Apps []AppConfig `json:"apps,omitempty"`
}

// TechnitiumServerStatus defines the observed state of TechnitiumServer.
type TechnitiumServerStatus struct {
	// version of the Technitium DNS Server.
	// +optional
	Version string `json:"version,omitempty"`

	// observedGeneration is the generation the conditions were computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions: Ready, and DryRun while the operator runs with --dry-run.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.version`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TechnitiumServer is one Technitium DNS Server node the operator manages.
type TechnitiumServer struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec TechnitiumServerSpec `json:"spec"`

	// +optional
	Status TechnitiumServerStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// TechnitiumServerList contains a list of TechnitiumServer.
type TechnitiumServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []TechnitiumServer `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &TechnitiumServer{}, &TechnitiumServerList{})
		return nil
	})
}
