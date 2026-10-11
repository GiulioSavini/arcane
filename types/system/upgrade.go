package system

import "time"

// UpgradeCheckResult reports whether the system can be upgraded.
type UpgradeCheckResult struct {
	CanUpgrade bool   `json:"canUpgrade"`
	Error      bool   `json:"error"`
	Message    string `json:"message"`
}

// EnvironmentUpdateResult reports an environment's outcome within an update job.
type EnvironmentUpdateResult struct {
	EnvironmentID   string     `json:"environmentId"`
	EnvironmentName string     `json:"environmentName"`
	Status          string     `json:"status"`
	FromVersion     string     `json:"fromVersion,omitempty"`
	FromDigest      string     `json:"fromDigest,omitempty"`
	ToVersion       string     `json:"toVersion,omitempty"`
	Stage           string     `json:"stage,omitempty"`
	StageStartedAt  *time.Time `json:"stageStartedAt,omitempty"`
	Error           string     `json:"error,omitempty"`
}

// EnvironmentUpdateJob reports a persisted fleet-wide update job.
type EnvironmentUpdateJob struct {
	ID                    string                    `json:"id"`
	CreatedAt             time.Time                 `json:"createdAt" sortable:"true"`
	UpdatedAt             *time.Time                `json:"updatedAt,omitempty"`
	Status                string                    `json:"status"`
	UserID                string                    `json:"userId"`
	Username              string                    `json:"username"`
	ManagerVersionAtStart string                    `json:"managerVersionAtStart"`
	ManagerDigestAtStart  string                    `json:"managerDigestAtStart"`
	ManagerTargetVersion  string                    `json:"managerTargetVersion"`
	Results               []EnvironmentUpdateResult `json:"results,omitempty"`
	Error                 *string                   `json:"error,omitempty"`
	CompletedAt           *time.Time                `json:"completedAt,omitempty"`
}
