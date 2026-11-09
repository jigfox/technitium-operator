package v1alpha1

// DeletionPolicy decides what happens to the entry in Technitium when its
// custom resource is deleted.
// +kubebuilder:validation:Enum=Delete;Orphan
type DeletionPolicy string

const (
	// DeletionPolicyDelete removes the entry from Technitium.
	DeletionPolicyDelete DeletionPolicy = "Delete"
	// DeletionPolicyOrphan leaves the entry in Technitium.
	DeletionPolicyOrphan DeletionPolicy = "Orphan"
)

// Condition types shared by all kinds.
const (
	// ConditionReady is True when Technitium matches the spec.
	ConditionReady = "Ready"
	// ConditionDryRun is True while the operator runs with --dry-run.
	ConditionDryRun = "DryRun"
)

// Condition reasons shared by all kinds.
const (
	ReasonReconciled     = "Reconciled"
	ReasonServerNotFound = "ServerNotFound"
	ReasonCredentials    = "CredentialsError"
	ReasonTechnitium     = "TechnitiumError"
	ReasonAppError       = "AppError"
	ReasonInvalid        = "Invalid"
	ReasonDryRun         = "DryRun"
	ReasonDryRunPending  = "DryRunPending"
	ReasonDeletionFailed = "DeletionFailed"
)
