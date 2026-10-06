package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	schedulertypes "github.com/getarcaneapp/arcane/types/v2/scheduler"

	"github.com/getarcaneapp/arcane/backend/v2/internal/system"
	"github.com/getarcaneapp/arcane/backend/v2/internal/volume"
)

// BackupRepositoryPruneJobName identifies the daily prune of the local backup
// repositories.
const BackupRepositoryPruneJobName = "backup-repository-prune"

// BackupRepositoryPruneJob frees the disk space deleted backups leave behind.
// Deleting a backup only marks its data for deletion, and rustic removes
// marked packs on a later prune once its keep-delete window has passed.
// Internal job: no job_metadata entry, invisible in the Jobs UI.
type BackupRepositoryPruneJob struct {
	systemService *system.SystemService
	volumeService *volume.VolumeService
}

// NewBackupRepositoryPruneJob builds the prune job for the scheduler.
func NewBackupRepositoryPruneJob(systemService *system.SystemService, volumeService *volume.VolumeService) *BackupRepositoryPruneJob {
	return &BackupRepositoryPruneJob{systemService: systemService, volumeService: volumeService}
}

func (j *BackupRepositoryPruneJob) Name() string {
	return BackupRepositoryPruneJobName
}

func (j *BackupRepositoryPruneJob) Schedule(_ context.Context) string {
	return "0 15 4 * * *"
}

func (j *BackupRepositoryPruneJob) Run(ctx context.Context) (schedulertypes.Outcome, error) {
	var pruneErr error
	if j.systemService != nil {
		if err := j.systemService.PruneLocalBackupRepository(ctx); err != nil {
			pruneErr = errors.Join(pruneErr, fmt.Errorf("prune system backup repository: %w", err))
		}
	}
	if j.volumeService != nil {
		if err := j.volumeService.PruneLocalBackupRepository(ctx); err != nil {
			pruneErr = errors.Join(pruneErr, fmt.Errorf("prune volume backup repository: %w", err))
		}
	}
	if pruneErr != nil {
		slog.ErrorContext(ctx, "Failed to prune local backup repositories", "jobName", BackupRepositoryPruneJobName, "error", pruneErr)
		return schedulertypes.Outcome{}, pruneErr
	}
	return schedulertypes.Outcome{Status: schedulertypes.Succeeded}, nil
}

func (j *BackupRepositoryPruneJob) Reschedule(_ context.Context) error {
	return nil
}
