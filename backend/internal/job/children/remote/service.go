// Package remote owns manager-side delivery, history and review of job runs
// executed on remote agents.
package remote

import (
	"context"
	"encoding/json/v2"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/getarcaneapp/arcane/types/v2/jobschedule"
	"github.com/getarcaneapp/arcane/types/v2/meta"
	"github.com/getarcaneapp/arcane/types/v2/scheduler"
	"github.com/getarcaneapp/arcane/types/v2/user"
	"go.getarcane.app/kit/pkg"

	"github.com/getarcaneapp/arcane/backend/v2/internal/environment"
	"github.com/getarcaneapp/arcane/backend/v2/internal/kv"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/remenv"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/scheduler/runs"
)

// Service talks to agents through the environment proxy and persists delivery
// state in the shared run coordinator.
type Service struct {
	runs         *runs.Coordinator
	store        *kv.KVService
	environments *environment.EnvironmentService
}

func New(coordinator *runs.Coordinator, store *kv.KVService, environments *environment.EnvironmentService) *Service {
	return &Service{
		runs:         coordinator,
		store:        store,
		environments: environments,
	}
}

// Deliver drives one manager-owned run through the remote delivery protocol.
func (s *Service) Deliver(ctx context.Context, run scheduler.Run) (scheduler.Outcome, error) {
	env, err := s.environments.GetEnvironmentByID(ctx, run.EnvironmentID)
	if err != nil {
		return scheduler.Outcome{Status: scheduler.Failed}, err
	}
	if !env.Enabled {
		return scheduler.Outcome{Status: scheduler.Failed, Message: "Environment is disabled"}, nil
	}
	basePath := "/api/environments/0/jobs/" + url.PathEscape(run.JobID)
	runPath := basePath + "/runs/" + url.PathEscape(run.ID)
	// A previously confirmed terminal result needs only acknowledgement, never execution.
	if !run.RemoteRetryRequested && run.RemoteAccepted && kit.FromPtr(run.RemoteOutcome).Status.Terminal() {
		return s.acknowledgeRemote(ctx, run, runPath, *run.RemoteOutcome)
	}
	var catalog jobschedule.JobListResponse
	if proxyJSONRequestErr := s.environments.ProxyJSONRequest(ctx, run.EnvironmentID, http.MethodGet, "/api/environments/0/jobs", nil, &catalog); proxyJSONRequestErr != nil {
		return remoteFailure(proxyJSONRequestErr)
	}
	if !catalog.DurableRuns {
		return scheduler.Outcome{Status: scheduler.Failed, Message: "Upgrade required: agent does not support durable job runs"}, nil
	}
	var remoteRun scheduler.Run
	if err = s.environments.ProxyJSONRequest(ctx, run.EnvironmentID, http.MethodGet, runPath, nil, &remoteRun); err != nil {
		var status *remenv.StatusError
		if !errors.As(err, &status) || status.StatusCode != http.StatusNotFound {
			return remoteFailure(err)
		}
		if run.RemoteAccepted {
			return scheduler.Outcome{Status: scheduler.Failed, Message: "Agent no longer has the delivery receipt; verify the operation before retrying"}, nil
		}
		if !remoteJobEligible(catalog.Jobs, run.JobID) {
			return scheduler.Outcome{Status: scheduler.Canceled, Message: "Remote job is disabled or no longer eligible"}, nil
		}
		// A confirmed 404 permits another delivery of the same deduplicated ID.
		if updateRunErr := s.runs.UpdateRun(ctx, run, func(current *scheduler.Run) error {
			*current = scheduler.Run{ //exhaustruct:enforce
				ActivityEnvironmentID:   current.ActivityEnvironmentID,
				ActivityID:              current.ActivityID,
				Resolution:              current.Resolution,
				RequestedWithKey:        current.RequestedWithKey,
				ID:                      current.ID,
				JobID:                   current.JobID,
				EnvironmentID:           current.EnvironmentID,
				Trigger:                 current.Trigger,
				RequestedBy:             current.RequestedBy,
				Status:                  current.Status,
				CreatedAt:               current.CreatedAt,
				UpdatedAt:               current.UpdatedAt,
				StartedAt:               current.StartedAt,
				FinishedAt:              current.FinishedAt,
				NextAttempt:             current.NextAttempt,
				AttemptCount:            current.AttemptCount,
				Owner:                   current.Owner,
				Outcome:                 current.Outcome,
				Attempts:                current.Attempts,
				RemoteDeliveryAttempted: true,
				RemoteOutcome:           current.RemoteOutcome,
				RemoteRetryRequested:    current.RemoteRetryRequested,
				RemoteRetryAttempted:    current.RemoteRetryAttempted,
				RemoteAttemptCount:      current.RemoteAttemptCount,
				RemoteAccepted:          current.RemoteAccepted,
				RemoteSettled:           current.RemoteSettled,
				LastConfirmedAt:         current.LastConfirmedAt,
			}
			return nil
		}); updateRunErr != nil {
			return remoteFailure(updateRunErr)
		}
		body, marshalErr := json.Marshal(map[string]string{"runId": run.ID})
		if marshalErr != nil {
			return remoteFailure(marshalErr)
		}
		var accepted jobschedule.JobRunResponse
		if proxyJSONRequestErr := s.environments.ProxyJSONRequest(ctx, run.EnvironmentID, http.MethodPost, basePath+"/run", body, &accepted); proxyJSONRequestErr != nil {
			return remoteFailure(proxyJSONRequestErr)
		}
		if accepted.RunID != run.ID {
			return remoteFailure(errors.New("Agent returned an inconsistent delivery receipt")) //nolint:staticcheck // Preserve the existing error message.
		}
		remoteRun = scheduler.Run{
			ID:      run.ID,
			JobID:   run.JobID,
			Status:  accepted.Status,
			Outcome: scheduler.Outcome{Status: accepted.Status},
		}
	}
	if remoteRun.ID != run.ID || remoteRun.JobID != run.JobID {
		return scheduler.Outcome{Status: scheduler.Failed, Message: "Agent returned an inconsistent run identity"}, nil
	}
	if run.RemoteRetryRequested && (remoteRun.Status.Terminal() || remoteRun.Status == scheduler.NeedsAttention) && remoteRun.AttemptCount <= run.RemoteAttemptCount {
		if run.RemoteRetryAttempted {
			return scheduler.Outcome{Status: scheduler.Failed, Message: "Remote retry acknowledgement was lost; inspect the agent before retrying again"}, nil
		}
		if updateRunErr := s.runs.UpdateRun(ctx, run, func(current *scheduler.Run) error {
			*current = scheduler.Run{ //exhaustruct:enforce
				ActivityEnvironmentID:   current.ActivityEnvironmentID,
				ActivityID:              current.ActivityID,
				Resolution:              current.Resolution,
				RequestedWithKey:        current.RequestedWithKey,
				ID:                      current.ID,
				JobID:                   current.JobID,
				EnvironmentID:           current.EnvironmentID,
				Trigger:                 current.Trigger,
				RequestedBy:             current.RequestedBy,
				Status:                  current.Status,
				CreatedAt:               current.CreatedAt,
				UpdatedAt:               current.UpdatedAt,
				StartedAt:               current.StartedAt,
				FinishedAt:              current.FinishedAt,
				NextAttempt:             current.NextAttempt,
				AttemptCount:            current.AttemptCount,
				Owner:                   current.Owner,
				Outcome:                 current.Outcome,
				Attempts:                current.Attempts,
				RemoteDeliveryAttempted: current.RemoteDeliveryAttempted,
				RemoteOutcome:           current.RemoteOutcome,
				RemoteRetryRequested:    current.RemoteRetryRequested,
				RemoteRetryAttempted:    true,
				RemoteAttemptCount:      current.RemoteAttemptCount,
				RemoteAccepted:          current.RemoteAccepted,
				RemoteSettled:           current.RemoteSettled,
				LastConfirmedAt:         current.LastConfirmedAt,
			}
			return nil
		}); updateRunErr != nil {
			return remoteFailure(updateRunErr)
		}
		var retried scheduler.Run
		if proxyJSONRequestErr := s.environments.ProxyJSONRequest(ctx, run.EnvironmentID, http.MethodPost, runPath+"/retry", nil, &retried); proxyJSONRequestErr != nil {
			return remoteFailure(proxyJSONRequestErr)
		}
		if retried.ID != run.ID || retried.JobID != run.JobID {
			return remoteFailure(errors.New("Agent returned an inconsistent retry receipt")) //nolint:staticcheck // Preserve the existing error message.
		}
		remoteRun = retried
	}
	return s.Confirm(ctx, run, remoteRun, runPath)
}

// Confirm records the agent-reported state of a delivered run.
func (s *Service) Confirm(ctx context.Context, run, remoteRun scheduler.Run, runPath string) (scheduler.Outcome, error) {
	if remoteRun.Status == scheduler.NeedsAttention || (remoteRun.Status == scheduler.Canceled && kit.FromPtr(remoteRun.Resolution).ResolvedBy == user.SystemUser.Username) {
		settled, err := s.resolveAgentReview(ctx, run.EnvironmentID, run.JobID, run.ID, user.SystemUser.Username)
		if err != nil {
			return scheduler.Outcome{
				Status:     scheduler.Waiting,
				Message:    "Waiting for the agent to acknowledge the failed run",
				Targets:    remoteRun.Outcome.Targets,
				ActivityID: remoteRun.Outcome.ActivityID,
			}, err
		}
		remoteRun = settled
	}
	outcome := scheduler.Outcome{
		Status:     kit.Ternary(remoteRun.Status == scheduler.Canceled && kit.FromPtr(remoteRun.Resolution).ResolvedBy == user.SystemUser.Username, scheduler.Failed, remoteRun.Status),
		Message:    remoteRun.Outcome.Message,
		ActivityID: remoteRun.Outcome.ActivityID,
		Targets:    remoteRun.Outcome.Targets,
		Steps:      remoteRun.Outcome.Steps,
	}
	if err := s.runs.UpdateRun(ctx, run, func(current *scheduler.Run) error {
		*current = scheduler.Run{ //exhaustruct:enforce
			ActivityEnvironmentID:   current.ActivityEnvironmentID,
			ActivityID:              current.ActivityID,
			Resolution:              remoteRun.Resolution,
			RequestedWithKey:        current.RequestedWithKey,
			ID:                      current.ID,
			JobID:                   current.JobID,
			EnvironmentID:           current.EnvironmentID,
			Trigger:                 current.Trigger,
			RequestedBy:             current.RequestedBy,
			Status:                  current.Status,
			CreatedAt:               current.CreatedAt,
			UpdatedAt:               current.UpdatedAt,
			StartedAt:               remoteRun.StartedAt,
			FinishedAt:              remoteRun.FinishedAt,
			NextAttempt:             current.NextAttempt,
			AttemptCount:            current.AttemptCount,
			Owner:                   current.Owner,
			Outcome:                 outcome,
			Attempts:                current.Attempts,
			RemoteDeliveryAttempted: true,
			RemoteOutcome:           new(outcome),
			RemoteRetryRequested:    false,
			RemoteRetryAttempted:    current.RemoteRetryAttempted,
			RemoteAttemptCount:      remoteRun.AttemptCount,
			RemoteAccepted:          true,
			RemoteSettled:           remoteRun.RemoteSettled,
			LastConfirmedAt:         new(time.Now().UTC()),
		}
		return nil
	}); err != nil {
		return scheduler.Outcome{Status: scheduler.Retrying}, err
	}
	if outcome.Status.Terminal() {
		if remoteRun.RemoteSettled {
			return outcome, nil
		}
		return s.acknowledgeRemote(ctx, run, runPath, outcome)
	}
	return scheduler.Outcome{
		Status:     scheduler.Waiting,
		Message:    "Waiting for the agent to complete this run",
		Targets:    outcome.Targets,
		ActivityID: outcome.ActivityID,
	}, nil
}

func (s *Service) acknowledgeRemote(ctx context.Context, run scheduler.Run, path string, outcome scheduler.Outcome) (scheduler.Outcome, error) {
	var response scheduler.Run
	if err := s.environments.ProxyJSONRequest(ctx, run.EnvironmentID, http.MethodPost, path+"/ack", nil, &response); err != nil {
		return scheduler.Outcome{
			Status:     scheduler.Waiting,
			Message:    "Remote operation completed; waiting for delivery acknowledgement",
			Targets:    outcome.Targets,
			ActivityID: outcome.ActivityID,
		}, err
	}
	status := kit.Ternary(response.Status == scheduler.Canceled && kit.FromPtr(response.Resolution).ResolvedBy == user.SystemUser.Username, scheduler.Failed, response.Status)
	if response.ID != run.ID || response.JobID != run.JobID || response.EnvironmentID != "0" || status != outcome.Status || !response.RemoteSettled {
		return scheduler.Outcome{Status: scheduler.Waiting, Message: "Remote operation completed; acknowledgement not confirmed"}, nil
	}
	if err := s.runs.UpdateRun(ctx, run, func(current *scheduler.Run) error {
		*current = scheduler.Run{ //exhaustruct:enforce
			ActivityEnvironmentID:   current.ActivityEnvironmentID,
			ActivityID:              current.ActivityID,
			Resolution:              current.Resolution,
			RequestedWithKey:        current.RequestedWithKey,
			ID:                      current.ID,
			JobID:                   current.JobID,
			EnvironmentID:           current.EnvironmentID,
			Trigger:                 current.Trigger,
			RequestedBy:             current.RequestedBy,
			Status:                  current.Status,
			CreatedAt:               current.CreatedAt,
			UpdatedAt:               current.UpdatedAt,
			StartedAt:               response.StartedAt,
			FinishedAt:              response.FinishedAt,
			NextAttempt:             current.NextAttempt,
			AttemptCount:            current.AttemptCount,
			Owner:                   current.Owner,
			Outcome:                 current.Outcome,
			Attempts:                current.Attempts,
			RemoteDeliveryAttempted: current.RemoteDeliveryAttempted,
			RemoteOutcome:           current.RemoteOutcome,
			RemoteRetryRequested:    current.RemoteRetryRequested,
			RemoteRetryAttempted:    current.RemoteRetryAttempted,
			RemoteAttemptCount:      current.RemoteAttemptCount,
			RemoteAccepted:          current.RemoteAccepted,
			RemoteSettled:           true,
			LastConfirmedAt:         current.LastConfirmedAt,
		}
		return nil
	}); err != nil {
		return scheduler.Outcome{Status: scheduler.Retrying}, err
	}
	return outcome, nil
}

// Catalog retains the last observed agent catalog while its environment is offline.
func (s *Service) Catalog(ctx context.Context, environmentID string) (*jobschedule.JobListResponse, error) {
	if _, err := s.environments.GetEnvironmentByID(ctx, environmentID); err != nil {
		return nil, err
	}
	key := "jobs-catalog/" + kit.SHA256Hex(environmentID)
	var catalog jobschedule.JobListResponse
	remoteErr := s.environments.ProxyJSONRequest(ctx, environmentID, http.MethodGet, "/api/environments/0/jobs", nil, &catalog)
	if remoteErr == nil {
		observed := jobschedule.JobListResponse{ //exhaustruct:enforce
			DurableRuns: catalog.DurableRuns,
			ObservedAt:  time.Now().UTC(),
			Offline:     false,
			Jobs:        catalog.Jobs,
			IsAgent:     catalog.IsAgent,
		}
		if observed.DurableRuns {
			if err := s.ReconcileLegacyCatalog(ctx, environmentID, observed.Jobs); err != nil {
				return nil, err
			}
		}
		raw, err := json.Marshal(observed)
		if err != nil {
			return nil, err
		}
		if setErr := s.store.Set(ctx, key, string(raw)); setErr != nil {
			return nil, setErr
		}
		return &observed, nil
	}
	var status *remenv.StatusError
	if errors.As(remoteErr, &status) && (status.StatusCode == http.StatusUnauthorized || status.StatusCode == http.StatusForbidden) {
		return nil, remoteErr
	}
	raw, found, err := s.store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if found {
		if unmarshalErr := json.Unmarshal([]byte(raw), &catalog); unmarshalErr != nil {
			return nil, unmarshalErr
		}
	}
	jobs := catalog.Jobs
	for _, metadata := range meta.GetAllJobMetadata() {
		if !found && !metadata.ManagerOnly {
			jobs = append(jobs, metadata.ToJobStatus("", nil, metadata.CanRunManually, []jobschedule.JobPrerequisite{}))
		}
	}
	return &jobschedule.JobListResponse{ //exhaustruct:enforce
		DurableRuns: catalog.DurableRuns,
		ObservedAt:  catalog.ObservedAt,
		Offline:     true,
		Jobs:        jobs,
		IsAgent:     catalog.IsAgent || !found,
	}, nil
}

// RetryRun queues one explicit retry of an authorized manager-owned run while
// retaining the agent's target progress.
func (s *Service) RetryRun(ctx context.Context, run scheduler.Run) (scheduler.Run, error) {
	if kit.FromPtr(run.Resolution).ResolvedBy == user.SystemUser.Username {
		var remote scheduler.Run
		path := "/api/environments/0/jobs/" + url.PathEscape(run.JobID) + "/runs/" + url.PathEscape(run.ID)
		if proxyJSONRequestErr := s.environments.ProxyJSONRequest(ctx, run.EnvironmentID, http.MethodGet, path, nil, &remote); proxyJSONRequestErr != nil {
			return run, proxyJSONRequestErr
		}
		if remote.ID != run.ID || remote.JobID != run.JobID || remote.EnvironmentID != "0" || remote.Status != scheduler.Failed {
			return run, errors.New("upgrade the agent before retrying a legacy settled run")
		}
	}
	err := s.runs.UpdateRun(ctx, run, func(current *scheduler.Run) error {
		if current.Status != scheduler.Failed && current.Status != scheduler.Partial && current.Status != scheduler.NeedsAttention {
			return errors.New("run is not eligible for retry")
		}
		if !current.RemoteAccepted && current.RemoteDeliveryAttempted {
			return errors.New("remote delivery is uncertain; inspect the agent before submitting new work")
		}
		remoteStatus := kit.FromPtr(current.RemoteOutcome).Status
		if current.RemoteAccepted && !remoteStatus.Terminal() && remoteStatus != scheduler.NeedsAttention {
			return errors.New("remote outcome must be confirmed before retry")
		}
		*current = scheduler.Run{ //exhaustruct:enforce
			ActivityEnvironmentID:   current.ActivityEnvironmentID,
			ActivityID:              current.ActivityID,
			Resolution:              current.Resolution,
			RequestedWithKey:        current.RequestedWithKey,
			ID:                      current.ID,
			JobID:                   current.JobID,
			EnvironmentID:           current.EnvironmentID,
			Trigger:                 current.Trigger,
			RequestedBy:             current.RequestedBy,
			Status:                  scheduler.Queued,
			CreatedAt:               current.CreatedAt,
			UpdatedAt:               time.Now().UTC(),
			StartedAt:               current.StartedAt,
			FinishedAt:              nil,
			NextAttempt:             nil,
			AttemptCount:            current.AttemptCount,
			Owner:                   "",
			Outcome:                 current.Outcome,
			Attempts:                current.Attempts,
			RemoteDeliveryAttempted: current.RemoteDeliveryAttempted,
			RemoteOutcome:           current.RemoteOutcome,
			RemoteRetryRequested:    current.RemoteAccepted || current.RemoteRetryRequested,
			RemoteRetryAttempted:    !current.RemoteAccepted && current.RemoteRetryAttempted,
			RemoteAttemptCount:      current.RemoteAttemptCount,
			RemoteAccepted:          current.RemoteAccepted,
			RemoteSettled:           !current.RemoteAccepted && current.RemoteSettled,
			LastConfirmedAt:         current.LastConfirmedAt,
		}
		run = *current
		return nil
	})
	return run, err
}

// Runs merges agent history with requests accepted by this manager.
func (s *Service) Runs(ctx context.Context, environmentID, jobID string) ([]scheduler.Run, error) {
	if _, err := s.environments.GetEnvironmentByID(ctx, environmentID); err != nil {
		return nil, err
	}
	records, err := s.runs.Records(ctx)
	if err != nil {
		return nil, err
	}
	merged := make(map[string]scheduler.Run)
	for page := 1; ; page++ {
		path := "/api/environments/0/jobs/" + url.PathEscape(jobID) + "/runs?page=" + strconv.Itoa(page) + "&limit=100"
		var response scheduler.RunList
		if proxyJSONRequestErr := s.environments.ProxyJSONRequest(ctx, environmentID, http.MethodGet, path, nil, &response); proxyJSONRequestErr != nil {
			var status *remenv.StatusError
			if errors.As(proxyJSONRequestErr, &status) && (status.StatusCode == http.StatusForbidden || status.StatusCode == http.StatusUnauthorized) {
				return nil, proxyJSONRequestErr
			}
			clear(merged)
			break
		}
		previous := len(merged)
		for _, run := range response.Runs {
			merged[run.ID] = scheduler.Run{ //exhaustruct:enforce
				ActivityEnvironmentID:   kit.Ternary(run.ActivityID != "", environmentID, run.ActivityEnvironmentID),
				ActivityID:              run.ActivityID,
				Resolution:              run.Resolution,
				RequestedWithKey:        run.RequestedWithKey,
				ID:                      run.ID,
				JobID:                   run.JobID,
				EnvironmentID:           environmentID,
				Trigger:                 run.Trigger,
				RequestedBy:             run.RequestedBy,
				Status:                  run.Status,
				CreatedAt:               run.CreatedAt,
				UpdatedAt:               run.UpdatedAt,
				StartedAt:               run.StartedAt,
				FinishedAt:              run.FinishedAt,
				NextAttempt:             run.NextAttempt,
				AttemptCount:            run.AttemptCount,
				Owner:                   run.Owner,
				Outcome:                 run.Outcome,
				Attempts:                run.Attempts,
				RemoteDeliveryAttempted: run.RemoteDeliveryAttempted,
				RemoteOutcome:           run.RemoteOutcome,
				RemoteRetryRequested:    run.RemoteRetryRequested,
				RemoteRetryAttempted:    run.RemoteRetryAttempted,
				RemoteAttemptCount:      run.RemoteAttemptCount,
				RemoteAccepted:          run.RemoteAccepted,
				RemoteSettled:           run.RemoteSettled,
				LastConfirmedAt:         run.LastConfirmedAt,
			}
		}
		if len(merged) >= response.Total || len(merged) == previous {
			break
		}
	}
	for _, record := range records {
		if record.EnvironmentID != environmentID || record.JobID != jobID {
			continue
		}
		for _, run := range record.Runs {
			merged[run.ID] = run
		}
	}
	return slices.Collect(maps.Values(merged)), nil
}

// AgentRun queries agent-owned history for a run the manager never accepted.
func (s *Service) AgentRun(ctx context.Context, environmentID, jobID, runID string) (scheduler.Run, error) {
	var run scheduler.Run
	path := "/api/environments/0/jobs/" + url.PathEscape(jobID) + "/runs/" + url.PathEscape(runID)
	if proxyJSONRequestErr := s.environments.ProxyJSONRequest(ctx, environmentID, http.MethodGet, path, nil, &run); proxyJSONRequestErr != nil {
		return scheduler.Run{}, proxyJSONRequestErr
	}
	if run.ID != runID || run.JobID != jobID {
		return scheduler.Run{}, errors.New("agent returned an inconsistent run identity")
	}
	return scheduler.Run{ //exhaustruct:enforce
		ActivityEnvironmentID:   kit.Ternary(run.ActivityID != "", environmentID, run.ActivityEnvironmentID),
		ActivityID:              run.ActivityID,
		Resolution:              run.Resolution,
		RequestedWithKey:        run.RequestedWithKey,
		ID:                      run.ID,
		JobID:                   run.JobID,
		EnvironmentID:           environmentID,
		Trigger:                 run.Trigger,
		RequestedBy:             run.RequestedBy,
		Status:                  run.Status,
		CreatedAt:               run.CreatedAt,
		UpdatedAt:               run.UpdatedAt,
		StartedAt:               run.StartedAt,
		FinishedAt:              run.FinishedAt,
		NextAttempt:             run.NextAttempt,
		AttemptCount:            run.AttemptCount,
		Owner:                   run.Owner,
		Outcome:                 run.Outcome,
		Attempts:                run.Attempts,
		RemoteDeliveryAttempted: run.RemoteDeliveryAttempted,
		RemoteOutcome:           run.RemoteOutcome,
		RemoteRetryRequested:    run.RemoteRetryRequested,
		RemoteRetryAttempted:    run.RemoteRetryAttempted,
		RemoteAttemptCount:      run.RemoteAttemptCount,
		RemoteAccepted:          run.RemoteAccepted,
		RemoteSettled:           run.RemoteSettled,
		LastConfirmedAt:         run.LastConfirmedAt,
	}, nil
}

// MutateAgentRun forwards an explicit operator action to an agent-owned run.
func (s *Service) MutateAgentRun(ctx context.Context, environmentID, jobID, runID, action string) (scheduler.Run, error) {
	targetEnvironment, err := s.environments.GetEnvironmentByID(ctx, environmentID)
	if err != nil {
		return scheduler.Run{}, err
	}
	if !targetEnvironment.Enabled {
		return scheduler.Run{}, errors.New("environment is disabled")
	}
	path := "/api/environments/0/jobs/" + url.PathEscape(jobID) + "/runs/" + url.PathEscape(runID) + "/" + action
	var run scheduler.Run
	// This is an explicit operator action. Never replay a lost mutation response.
	if proxyJSONRequestErr := s.environments.ProxyJSONRequest(ctx, environmentID, http.MethodPost, path, nil, &run); proxyJSONRequestErr != nil {
		return scheduler.Run{}, proxyJSONRequestErr
	}
	if run.ID != runID || run.JobID != jobID {
		return scheduler.Run{}, errors.New("agent returned an inconsistent run identity")
	}
	return scheduler.Run{ //exhaustruct:enforce
		ActivityEnvironmentID:   kit.Ternary(run.ActivityID != "", environmentID, run.ActivityEnvironmentID),
		ActivityID:              run.ActivityID,
		Resolution:              run.Resolution,
		RequestedWithKey:        run.RequestedWithKey,
		ID:                      run.ID,
		JobID:                   run.JobID,
		EnvironmentID:           environmentID,
		Trigger:                 run.Trigger,
		RequestedBy:             run.RequestedBy,
		Status:                  run.Status,
		CreatedAt:               run.CreatedAt,
		UpdatedAt:               run.UpdatedAt,
		StartedAt:               run.StartedAt,
		FinishedAt:              run.FinishedAt,
		NextAttempt:             run.NextAttempt,
		AttemptCount:            run.AttemptCount,
		Owner:                   run.Owner,
		Outcome:                 run.Outcome,
		Attempts:                run.Attempts,
		RemoteDeliveryAttempted: run.RemoteDeliveryAttempted,
		RemoteOutcome:           run.RemoteOutcome,
		RemoteRetryRequested:    run.RemoteRetryRequested,
		RemoteRetryAttempted:    run.RemoteRetryAttempted,
		RemoteAttemptCount:      run.RemoteAttemptCount,
		RemoteAccepted:          run.RemoteAccepted,
		RemoteSettled:           run.RemoteSettled,
		LastConfirmedAt:         run.LastConfirmedAt,
	}, nil
}

// ReconcileLegacyCatalog adopts legacy agent-owned runs needing attention.
// TODO(v3): remove adoption of legacy agent-owned runs.
func (s *Service) ReconcileLegacyCatalog(ctx context.Context, environmentID string, jobs []jobschedule.JobStatus) error {
	for _, job := range jobs {
		for _, remote := range []*scheduler.Run{job.CurrentRun, job.LastRun} {
			if kit.FromPtr(remote).Status != scheduler.NeedsAttention {
				continue
			}
			if remote.JobID != job.ID || remote.EnvironmentID != "0" {
				return errors.New("agent returned an inconsistent run identity")
			}
			_, err := s.runs.Get(ctx, environmentID, job.ID, remote.ID)
			if err == nil {
				continue
			}
			if !errors.Is(err, runs.ErrRunNotFound) {
				return err
			}
			_, err = s.runs.Submit(ctx, scheduler.Request{
				RunID:            remote.ID,
				JobID:            job.ID,
				EnvironmentID:    environmentID,
				Trigger:          "recovery",
				ObservedAgentRun: remote,
			})
			if err != nil {
				return err
			}
		}
		if err := s.ReconcileLegacyCatalog(ctx, environmentID, job.Children); err != nil {
			return err
		}
	}
	return nil
}

// ResolveRun settles a remote run that needs operator review.
func (s *Service) ResolveRun(ctx context.Context, environmentID, jobID, runID, actor string) (scheduler.Run, error) {
	local, localErr := s.runs.Get(ctx, environmentID, jobID, runID)
	if localErr != nil && !errors.Is(localErr, runs.ErrRunNotFound) {
		return scheduler.Run{}, localErr
	}
	if localErr == nil {
		if local.Status == scheduler.Canceled && local.Resolution != nil {
			return local, nil
		}
		if local.Status != scheduler.NeedsAttention {
			return local, errors.New("only runs needing attention can be resolved")
		}
		if !local.RemoteDeliveryAttempted && !local.RemoteAccepted {
			return s.runs.Resolve(ctx, environmentID, jobID, runID, actor)
		}
	}
	acknowledged, err := s.resolveAgentReview(ctx, environmentID, jobID, runID, actor)
	if err != nil {
		return scheduler.Run{}, err
	}
	if localErr != nil {
		return scheduler.Run{ //exhaustruct:enforce
			ActivityEnvironmentID:   kit.Ternary(acknowledged.ActivityID != "", environmentID, acknowledged.ActivityEnvironmentID),
			ActivityID:              acknowledged.ActivityID,
			Resolution:              acknowledged.Resolution,
			RequestedWithKey:        acknowledged.RequestedWithKey,
			ID:                      acknowledged.ID,
			JobID:                   acknowledged.JobID,
			EnvironmentID:           environmentID,
			Trigger:                 acknowledged.Trigger,
			RequestedBy:             acknowledged.RequestedBy,
			Status:                  acknowledged.Status,
			CreatedAt:               acknowledged.CreatedAt,
			UpdatedAt:               acknowledged.UpdatedAt,
			StartedAt:               acknowledged.StartedAt,
			FinishedAt:              acknowledged.FinishedAt,
			NextAttempt:             acknowledged.NextAttempt,
			AttemptCount:            acknowledged.AttemptCount,
			Owner:                   acknowledged.Owner,
			Outcome:                 acknowledged.Outcome,
			Attempts:                acknowledged.Attempts,
			RemoteDeliveryAttempted: acknowledged.RemoteDeliveryAttempted,
			RemoteOutcome:           acknowledged.RemoteOutcome,
			RemoteRetryRequested:    acknowledged.RemoteRetryRequested,
			RemoteRetryAttempted:    acknowledged.RemoteRetryAttempted,
			RemoteAttemptCount:      acknowledged.RemoteAttemptCount,
			RemoteAccepted:          acknowledged.RemoteAccepted,
			RemoteSettled:           acknowledged.RemoteSettled,
			LastConfirmedAt:         acknowledged.LastConfirmedAt,
		}, nil
	}
	if updateRunErr := s.runs.UpdateRun(ctx, local, func(current *scheduler.Run) error {
		if current.Status != scheduler.NeedsAttention {
			return runs.ErrRunConflict
		}
		*current = scheduler.Run{ //exhaustruct:enforce
			ActivityEnvironmentID:   current.ActivityEnvironmentID,
			ActivityID:              current.ActivityID,
			Resolution:              current.Resolution,
			RequestedWithKey:        current.RequestedWithKey,
			ID:                      current.ID,
			JobID:                   current.JobID,
			EnvironmentID:           current.EnvironmentID,
			Trigger:                 current.Trigger,
			RequestedBy:             current.RequestedBy,
			Status:                  current.Status,
			CreatedAt:               current.CreatedAt,
			UpdatedAt:               current.UpdatedAt,
			StartedAt:               current.StartedAt,
			FinishedAt:              current.FinishedAt,
			NextAttempt:             current.NextAttempt,
			AttemptCount:            current.AttemptCount,
			Owner:                   current.Owner,
			Outcome:                 current.Outcome,
			Attempts:                current.Attempts,
			RemoteDeliveryAttempted: current.RemoteDeliveryAttempted,
			RemoteOutcome: &scheduler.Outcome{
				Status:     acknowledged.Status,
				Message:    acknowledged.Outcome.Message,
				ActivityID: acknowledged.Outcome.ActivityID,
				Targets:    acknowledged.Outcome.Targets,
				Steps:      acknowledged.Outcome.Steps,
			},
			RemoteRetryRequested: current.RemoteRetryRequested,
			RemoteRetryAttempted: current.RemoteRetryAttempted,
			RemoteAttemptCount:   current.RemoteAttemptCount,
			RemoteAccepted:       true,
			RemoteSettled:        true,
			LastConfirmedAt:      new(time.Now().UTC()),
		}
		return nil
	}); updateRunErr != nil {
		return scheduler.Run{}, updateRunErr
	}
	return s.runs.Resolve(ctx, environmentID, jobID, runID, acknowledged.Resolution.ResolvedBy)
}

// resolveAgentReview settles legacy inactive runs on their owning agent.
// TODO(v3): remove the legacy resolution protocol.
func (s *Service) resolveAgentReview(ctx context.Context, environmentID, jobID, runID, actor string) (scheduler.Run, error) {
	env, err := s.environments.GetEnvironmentByID(ctx, environmentID)
	if err != nil {
		return scheduler.Run{}, err
	}
	if !env.Enabled {
		return scheduler.Run{}, errors.New("environment is disabled")
	}
	path := "/api/environments/0/jobs/" + url.PathEscape(jobID) + "/runs/" + url.PathEscape(runID)
	var remote scheduler.Run
	// Read the owner on every request, including retries after a lost response.
	if proxyJSONRequestErr := s.environments.ProxyJSONRequest(ctx, environmentID, http.MethodGet, path, nil, &remote); proxyJSONRequestErr != nil {
		return scheduler.Run{}, proxyJSONRequestErr
	}
	if remote.ID != runID || remote.JobID != jobID || remote.EnvironmentID != "0" {
		return scheduler.Run{}, errors.New("agent returned an inconsistent run identity")
	}
	if remote.Status == scheduler.NeedsAttention {
		body, marshalErr := json.Marshal(map[string]string{"resolvedBy": actor})
		if marshalErr != nil {
			return scheduler.Run{}, marshalErr
		}
		if resolveRemoteJobErr := s.environments.ProxyJSONRequest(ctx, environmentID, http.MethodPost, path+"/resolve", body, &remote); resolveRemoteJobErr != nil {
			return scheduler.Run{}, resolveRemoteJobErr
		}
	} else if actor != user.SystemUser.Username && (remote.Status != scheduler.Canceled || remote.Resolution == nil) {
		return scheduler.Run{}, errors.New("agent run must need attention before review resolution")
	}
	if remote.ID != runID || remote.JobID != jobID || remote.EnvironmentID != "0" || !remote.Status.Terminal() ||
		(actor != user.SystemUser.Username && (remote.Status != scheduler.Canceled || remote.Resolution == nil)) {
		return scheduler.Run{}, errors.New("agent resolution was not confirmed")
	}
	var acknowledged scheduler.Run
	if acknowledgeRemoteResolutionErr := s.environments.ProxyJSONRequest(ctx, environmentID, http.MethodPost, path+"/ack", nil, &acknowledged); acknowledgeRemoteResolutionErr != nil {
		return scheduler.Run{}, acknowledgeRemoteResolutionErr
	}
	if acknowledged.ID != runID || acknowledged.JobID != jobID || acknowledged.EnvironmentID != "0" || acknowledged.Status != remote.Status || !acknowledged.RemoteSettled ||
		(remote.Resolution != nil && (acknowledged.Resolution == nil || acknowledged.Resolution.ResolvedBy != remote.Resolution.ResolvedBy)) {
		return scheduler.Run{}, errors.New("agent resolution acknowledgement was not confirmed")
	}
	return acknowledged, nil
}

func remoteJobEligible(jobs []jobschedule.JobStatus, id string) bool {
	for _, job := range jobs {
		if job.ID != id {
			if remoteJobEligible(job.Children, id) {
				return true
			}
			continue
		}
		return job.Enabled && job.CanRunManually && !job.ManagerOnly &&
			!slices.ContainsFunc(job.Prerequisites, func(prerequisite jobschedule.JobPrerequisite) bool { return !prerequisite.IsMet })
	}
	return false
}

func remoteFailure(err error) (scheduler.Outcome, error) {
	var status *remenv.StatusError
	rejected := errors.As(err, &status) && status.StatusCode >= 400 && status.StatusCode < 500 &&
		status.StatusCode != http.StatusRequestTimeout && status.StatusCode != http.StatusTooManyRequests
	if rejected {
		return scheduler.Outcome{Status: scheduler.Failed, Message: "Remote agent rejected the request; check configuration and permissions"}, err
	}
	return scheduler.Outcome{Status: scheduler.Waiting, Message: "Waiting for environment"}, err
}
