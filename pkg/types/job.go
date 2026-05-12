package types

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Job represents a short-lived workload that runs to completion.
type Job struct {
	// Unique identifier for the job
	ID string `json:"id" yaml:"id"`

	// Human-readable name for the job
	Name string `json:"name" yaml:"name"`

	// Namespace the job belongs to
	Namespace string `json:"namespace" yaml:"namespace"`

	// Schedule in cron format (if this is a scheduled job)
	Schedule string `json:"schedule,omitempty" yaml:"schedule,omitempty"`

	// Template defining the job execution environment
	Template JobTemplate `json:"template" yaml:"template"`

	// Optional array job configuration
	Array *JobArray `json:"array,omitempty" yaml:"array,omitempty"`

	// Retry policy for failed job runs
	RetryPolicy *RetryPolicy `json:"retryPolicy,omitempty" yaml:"retryPolicy,omitempty"`

	// How to handle concurrent runs (for scheduled jobs)
	ConcurrencyPolicy string `json:"concurrencyPolicy,omitempty" yaml:"concurrencyPolicy,omitempty"`

	// Number of successful run records to keep
	SuccessfulRunsHistoryLimit int `json:"successfulRunsHistoryLimit,omitempty" yaml:"successfulRunsHistoryLimit,omitempty"`

	// Number of failed run records to keep
	FailedRunsHistoryLimit int `json:"failedRunsHistoryLimit,omitempty" yaml:"failedRunsHistoryLimit,omitempty"`

	// Creation timestamp
	CreatedAt time.Time `json:"createdAt" yaml:"createdAt"`

	// Last update timestamp
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`
}

// JobTemplate defines the execution environment for a job.
type JobTemplate struct {
	// Container image for the job
	Image string `json:"image" yaml:"image"`

	// Command to run in the container (overrides image CMD)
	Command string `json:"command,omitempty" yaml:"command,omitempty"`

	// Arguments to the command
	Args []string `json:"args,omitempty" yaml:"args,omitempty"`

	// Environment variables for the job
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`

	// Resource requirements for the job
	Resources Resources `json:"resources,omitempty" yaml:"resources,omitempty"`

	// Volumes to mount in the job container
	Volumes []VolumeMount `json:"volumes,omitempty" yaml:"volumes,omitempty"`

	// Secret mounts
	SecretMounts []SecretMount `json:"secretMounts,omitempty" yaml:"secretMounts,omitempty"`

	// Config mounts
	ConfigmapMounts []ConfigmapMount `json:"configmapMounts,omitempty" yaml:"configmapMounts,omitempty"`
}

// JobArray defines parameters for array jobs (multiple parallel runs).
type JobArray struct {
	// Number of runs to create in the array
	Count int `json:"count" yaml:"count"`

	// Optional parallelism limit (max concurrent runs)
	Parallelism int `json:"parallelism,omitempty" yaml:"parallelism,omitempty"`
}

// RetryPolicy defines when and how to retry failed jobs.
type RetryPolicy struct {
	// Number of retry attempts
	Count int `json:"count" yaml:"count"`

	// Backoff type (fixed or exponential)
	Backoff string `json:"backoff" yaml:"backoff"`

	// Initial backoff duration in seconds (for fixed or exponential)
	BackoffSeconds int `json:"backoffSeconds,omitempty" yaml:"backoffSeconds,omitempty"`
}

// SecretMount defines a secret to be mounted in a container.
type SecretMount struct {
	// Name of the mount (for identification)
	Name string `json:"name" yaml:"name"`

	// Path where the secret should be mounted
	MountPath string `json:"mountPath" yaml:"mountPath"`

	// Name of the secret to mount
	SecretName string `json:"secretName" yaml:"secretName"`

	// Optional: specific keys to project from the secret
	Items []KeyToPath `json:"items,omitempty" yaml:"items,omitempty"`
}

// ConfigmapMount defines a config to be mounted in a container.
type ConfigmapMount struct {
	// Name of the mount (for identification)
	Name string `json:"name" yaml:"name"`

	// Path where the config should be mounted
	MountPath string `json:"mountPath" yaml:"mountPath"`

	// Name of the config to mount
	ConfigmapName string `json:"configmapName" yaml:"configmapName"`

	// Optional: specific keys to project from the config
	Items []KeyToPath `json:"items,omitempty" yaml:"items,omitempty"`
}

// KeyToPath defines mapping from a key in a Secret/Config to a path in the mount.
type KeyToPath struct {
	// Key in the Secret or Config
	Key string `json:"key" yaml:"key"`

	// Path where the key should be mounted (relative to mount point)
	Path string `json:"path" yaml:"path"`
}

// JobRun represents a single execution of a Job.
type JobRun struct {
	// Unique identifier for the job run
	ID string `json:"id" yaml:"id"`

	// Human-readable name for the job run
	Name string `json:"name" yaml:"name"`

	// ID of the job this run belongs to
	JobID string `json:"jobId" yaml:"jobId"`

	// Namespace the job run belongs to
	Namespace string `json:"namespace" yaml:"namespace"`

	// Node this job run is/was scheduled on
	NodeID string `json:"nodeId,omitempty" yaml:"nodeId,omitempty"`

	// For array jobs, the index of this run in the array
	ArrayIndex int `json:"arrayIndex,omitempty" yaml:"arrayIndex,omitempty"`

	// Status of the job run
	Status JobRunStatus `json:"status" yaml:"status"`

	// Detailed status message
	StatusMessage string `json:"statusMessage,omitempty" yaml:"statusMessage,omitempty"`

	// Exit code if the job has completed
	ExitCode int `json:"exitCode,omitempty" yaml:"exitCode,omitempty"`

	// Number of restart attempts so far
	RestartCount int `json:"restartCount,omitempty" yaml:"restartCount,omitempty"`

	// Start time
	StartTime *time.Time `json:"startTime,omitempty" yaml:"startTime,omitempty"`

	// Completion time
	CompletionTime *time.Time `json:"completionTime,omitempty" yaml:"completionTime,omitempty"`

	// Creation timestamp
	CreatedAt time.Time `json:"createdAt" yaml:"createdAt"`

	// Last update timestamp
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`
}

// JobRunStatus represents the current status of a job run.
type JobRunStatus string

const (
	// JobRunStatusPending indicates the job run is waiting to be scheduled
	JobRunStatusPending JobRunStatus = "Pending"

	// JobRunStatusRunning indicates the job run is currently executing
	JobRunStatusRunning JobRunStatus = "Running"

	// JobRunStatusSucceeded indicates the job run completed successfully
	JobRunStatusSucceeded JobRunStatus = "Succeeded"

	// JobRunStatusFailed indicates the job run failed
	JobRunStatusFailed JobRunStatus = "Failed"
)

// JobSpec represents the YAML specification for a job.
type JobSpec struct {
	// Human-readable name for the job (required)
	Name string `json:"name" yaml:"name"`

	// Namespace the job belongs to (optional, defaults to "default")
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`

	// Schedule in cron format (if this is a scheduled job)
	Schedule string `json:"schedule,omitempty" yaml:"schedule,omitempty"`

	// Template defining the job execution environment
	Template JobTemplate `json:"template" yaml:"template"`

	// Optional array job configuration
	Array *JobArray `json:"array,omitempty" yaml:"array,omitempty"`

	// Retry policy for failed job runs
	RetryPolicy *RetryPolicy `json:"retryPolicy,omitempty" yaml:"retryPolicy,omitempty"`

	// How to handle concurrent runs (for scheduled jobs)
	ConcurrencyPolicy string `json:"concurrencyPolicy,omitempty" yaml:"concurrencyPolicy,omitempty"`

	// Number of successful run records to keep
	SuccessfulRunsHistoryLimit int `json:"successfulRunsHistoryLimit,omitempty" yaml:"successfulRunsHistoryLimit,omitempty"`

	// Number of failed run records to keep
	FailedRunsHistoryLimit int `json:"failedRunsHistoryLimit,omitempty" yaml:"failedRunsHistoryLimit,omitempty"`
}

// JobFile represents a file containing job definitions.
type JobFile struct {
	// Job definition
	Job *JobSpec `json:"job,omitempty" yaml:"job,omitempty"`

	// Multiple job definitions
	Jobs []JobSpec `json:"jobs,omitempty" yaml:"jobs,omitempty"`
}

// Validate checks if a job specification is valid.
func (j *JobSpec) Validate() error {
	if j.Name == "" {
		return NewValidationError("job name is required")
	}

	if j.Template.Image == "" {
		return NewValidationError("job template image is required")
	}

	if j.Array != nil && j.Array.Count <= 0 {
		return NewValidationError("array job count must be positive")
	}

	// TODO: validate cron expression
	/*if j.Schedule != "" {
		// In a real implementation, validate the cron expression
		// This is a placeholder
	}*/

	return nil
}

// ToJob converts a JobSpec to a Job.
func (j *JobSpec) ToJob() (*Job, error) {
	// Validate
	if err := j.Validate(); err != nil {
		return nil, err
	}

	// Set default namespace if not specified
	namespace := j.Namespace
	if namespace == "" {
		namespace = "default"
	}

	now := time.Now()

	return &Job{
		ID:                         uuid.New().String(),
		Name:                       j.Name,
		Namespace:                  namespace,
		Schedule:                   j.Schedule,
		Template:                   j.Template,
		Array:                      j.Array,
		RetryPolicy:                j.RetryPolicy,
		ConcurrencyPolicy:          j.ConcurrencyPolicy,
		SuccessfulRunsHistoryLimit: j.SuccessfulRunsHistoryLimit,
		FailedRunsHistoryLimit:     j.FailedRunsHistoryLimit,
		CreatedAt:                  now,
		UpdatedAt:                  now,
	}, nil
}

// CreateJobRun creates a new JobRun for a Job.
func (j *Job) CreateJobRun(arrayIndex int) *JobRun {
	now := time.Now()

	name := j.Name
	if j.Array != nil && j.Array.Count > 1 {
		name = fmt.Sprintf("%s-%d", j.Name, arrayIndex)
	}

	return &JobRun{
		ID:         uuid.New().String(),
		Name:       name,
		JobID:      j.ID,
		Namespace:  j.Namespace,
		ArrayIndex: arrayIndex,
		Status:     JobRunStatusPending,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}
