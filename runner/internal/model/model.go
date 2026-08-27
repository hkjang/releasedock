package model

import (
	"encoding/json"
	"time"
)

// JobStatus is both the externally visible release state and the pipeline step
// name. QUEUED is the only non-executing state.
type JobStatus string

type JobOperation string

const (
	StatusQueued       JobStatus = "QUEUED"
	StatusValidating   JobStatus = "VALIDATING"
	StatusPreCheck     JobStatus = "PRE_CHECK"
	StatusExtracting   JobStatus = "EXTRACTING"
	StatusImageInspect JobStatus = "IMAGE_INSPECT"
	StatusImageLoad    JobStatus = "IMAGE_LOAD"
	StatusImageTag     JobStatus = "IMAGE_TAG"
	StatusImagePush    JobStatus = "IMAGE_PUSH"
	StatusDeploying    JobStatus = "DEPLOYING"
	StatusVerifying    JobStatus = "VERIFYING"
	StatusRollback     JobStatus = "ROLLBACK"
	StatusRolledBack   JobStatus = "ROLLED_BACK"
	StatusSuccess      JobStatus = "SUCCESS"
	StatusFailed       JobStatus = "FAILED"
)

const (
	OperationDeploy   JobOperation = "DEPLOY"
	OperationRollback JobOperation = "ROLLBACK"
)

type Settings struct {
	PollInterval      time.Duration
	LockRetry         time.Duration
	SettingsRefresh   time.Duration
	HeartbeatInterval time.Duration
	StaleJobAfter     time.Duration
	WorkspaceRoot     string
	ArtifactRoot      string
	CommandPath       string
	LogChunkBytes     int
}

type ExtractionPolicy struct {
	MaxArchiveBytes   int64
	MaxExtractedBytes int64
	MaxFiles          int
	MaxImages         int
	AllowSymlinks     bool
}

type RuntimeConfig struct {
	Kind                 string
	BinaryPath           string
	Namespace            string
	RegistryURL          string
	RegistryHost         string
	RegistryProject      string
	RegistryInsecure     bool
	RegistryCAPEM        string
	CredentialAAD        string
	CredentialCiphertext string
}

type HealthCheck struct {
	Type               string            `json:"type"`
	Address            string            `json:"address"`
	Method             string            `json:"method,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
	ExpectedStatusMin  int               `json:"expectedStatusMin,omitempty"`
	ExpectedStatusMax  int               `json:"expectedStatusMax,omitempty"`
	ExpectedBody       string            `json:"expectedBody,omitempty"`
	TimeoutSeconds     int               `json:"timeoutSeconds,omitempty"`
	Attempts           int               `json:"attempts,omitempty"`
	IntervalSeconds    int               `json:"intervalSeconds,omitempty"`
	InsecureSkipVerify bool              `json:"insecureSkipVerify,omitempty"`
	CAPEM              string            `json:"caPem,omitempty"`
}

type Script struct {
	ID              string
	Name            string
	Version         int
	Phase           string
	InterpreterPath string
	SHA256          string
	Content         []byte
	Args            []string
	Timeout         time.Duration
	ApprovedAt      time.Time
}

type Profile struct {
	ID                  string
	Name                string
	Extraction          ExtractionPolicy
	Runtime             RuntimeConfig
	HealthChecks        []HealthCheck
	Scripts             []Script
	CommandTimeout      time.Duration
	MaxLogBytes         int64
	AutoRollback        bool
	CleanupWorkspace    bool
	KeepFailedWorkspace bool
}

type TargetCredential struct {
	ID         string
	Type       string
	Version    int
	Ciphertext string
	AAD        string
}

type Job struct {
	ID                      string
	ReleaseID               string
	RollbackSourceReleaseID string
	RollbackSourceJobID     string
	Application             string
	Version                 string
	Environment             string
	LockKey                 string
	ArtifactPath            string
	ExpectedSHA256          string
	ManifestPath            string
	Operation               JobOperation
	Attempt                 int
	Profile                 Profile
	TargetCredential        TargetCredential
	RollbackImages          []ImageRecord
}

type Step struct {
	ID     int64
	JobID  string
	Name   JobStatus
	Number int
}

type ImageRecord struct {
	FilePath       string
	SourceRef      string
	DestinationRef string
	Repository     string
	Tag            string
	Digest         string
}

type StepResult struct {
	ExitCode int
	Message  string
	Metadata json.RawMessage
}
