// Package deployment runs the image and bind-directory side of project
// deployments: pulls, builds, update reconciliation and dependents.
package deployment

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"slices"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/getarcaneapp/arcane/types/v2"
	"github.com/getarcaneapp/arcane/types/v2/containerregistry"
	projecttypes "github.com/getarcaneapp/arcane/types/v2/project"
	usertypes "github.com/getarcaneapp/arcane/types/v2/user"
	"go.getarcane.app/acfs"
	buildtypes "go.getarcane.app/builds/types"

	"github.com/getarcaneapp/arcane/backend/v2/internal/docker"
	"github.com/getarcaneapp/arcane/backend/v2/internal/image"
	"github.com/getarcaneapp/arcane/backend/v2/internal/settings"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/libarcane/timeouts"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/projects"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/utils"
)

// Builder builds Compose service images.
type Builder interface {
	BuildImage(ctx context.Context, environmentID string, req buildtypes.BuildRequest, progressWriter io.Writer, serviceName string, user *usertypes.Actor) (*buildtypes.BuildResult, error)
	BuildSettings() buildtypes.BuildSettings
}

// Service runs the image side of deployments: pulls, update reconciliation, builds and dependents.
type Service struct {
	settingsService *settings.SettingsService
	imageService    *image.ImageService
	dockerService   *docker.DockerClientService
	buildService    Builder
}

func New(settingsService *settings.SettingsService, imageService *image.ImageService, dockerService *docker.DockerClientService, buildService Builder) *Service {
	return &Service{
		settingsService: settingsService,
		imageService:    imageService,
		dockerService:   dockerService,
		buildService:    buildService,
	}
}

// Pull pulls one image within the configured pull timeout and reconciles its update state.
func (s *Service) Pull(
	ctx context.Context,
	imageRef string,
	progressWriter io.Writer,
	user usertypes.Actor,
	credentials []containerregistry.Credential,
) error {
	cfg := s.settingsService.GetSettingsConfig()

	pullCtx, pullCancel := context.WithTimeout(ctx, timeouts.GetDuration(cfg.DockerImagePullTimeout.AsInt(), timeouts.DefaultDockerImagePull))
	defer pullCancel()

	if err := s.imageService.PullImage(pullCtx, imageRef, progressWriter, user, credentials); err != nil {
		if errors.Is(pullCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("image pull timed out for %s (increase DOCKER_IMAGE_PULL_TIMEOUT or setting)", imageRef)
		}
		return fmt.Errorf("failed to pull image %s: %w", imageRef, err)
	}

	if err := s.imageService.ReconcilePulledImageUpdate(ctx, imageRef); err != nil {
		slog.WarnContext(ctx, "failed to reconcile pulled image update state", "image", imageRef, "error", err)
	}
	return nil
}

// NamespaceDependents splits dependents into running and stopped-only services; never-created ones are skipped.
// A ComposePs failure treats every dependent as running, since a dangling namespace reference is worse.
func (s *Service) NamespaceDependents(ctx context.Context, compProj *composetypes.Project, services []string) (running, stopped []string) {
	if len(services) == 0 {
		return nil, nil
	}
	deps := projects.NamespaceDependents(compProj, services)
	if len(deps) == 0 {
		return nil, nil
	}
	summaries, err := projects.ComposePs(ctx, s.dockerService.DockerHost(), compProj, deps, true)
	if err != nil {
		slog.WarnContext(ctx, "failed to list namespace dependent containers; recreating all dependents", "project", compProj.Name, "dependents", deps, "error", err)
		return deps, nil
	}
	for _, name := range deps {
		switch {
		case slices.ContainsFunc(summaries, func(summary api.ContainerSummary) bool {
			return summary.Service == name && summary.State == "running"
		}):
			running = append(running, name)
		case slices.ContainsFunc(summaries, func(summary api.ContainerSummary) bool {
			return summary.Service == name
		}):
			stopped = append(stopped, name)
		}
	}
	return running, stopped
}

// ImageOperations wires image pulls, existence checks and builds for Compose.
func (s *Service) ImageOperations(user *usertypes.Actor, credentials []containerregistry.Credential) projecttypes.ComposeImageOperations {
	return projecttypes.ComposeImageOperations{
		Exists:     s.imageService.ImageExistsLocally,
		LastTagged: s.imageService.ImageLastTagTime,
		Pull: func(ctx context.Context, image string, progress io.Writer) error {
			return s.Pull(ctx, image, progress, *cmp.Or(user, &usertypes.SystemUser), credentials)
		},
		Build: func(ctx context.Context, request buildtypes.BuildRequest, progress io.Writer, service string) error {
			_, err := s.buildService.BuildImage(ctx, types.LocalDockerEnvironmentID, request, progress, service, user)
			return err
		},
		BuildProvider: s.buildService.BuildSettings().BuildProvider,
	}
}

// PrepareProjectBindDirectories creates missing auto-created bind sources as Arcane's user, not root (#4132).
// Existing, escaping (#4195) and uninspectable sources are left to Docker.
func PrepareProjectBindDirectories(projectPath string) projects.PrepareProjectFunc {
	return func(ctx context.Context, project *composetypes.Project) error {
		for _, serviceName := range slices.Sorted(maps.Keys(project.Services)) {
			for _, volume := range project.Services[serviceName].Volumes {
				if volume.Type != composetypes.VolumeTypeBind || volume.Source == "" {
					continue
				}
				if volume.Bind != nil && !bool(volume.Bind.CreateHostPath) {
					continue
				}
				logicalPath, err := acfs.LogicalPath(projectPath, volume.Source)
				if err == nil && logicalPath != "/" {
					exists, existsErr := acfs.Exists(ctx, projectPath, logicalPath)
					switch {
					case exists || errors.Is(existsErr, acfs.ErrOutsideRoot) || errors.Is(existsErr, fs.ErrPermission):
						// Leave existing, escaping, and uninspectable sources to the Docker daemon.
					case existsErr != nil:
						err = existsErr
					default:
						if err = acfs.MkdirAll(ctx, projectPath, logicalPath, utils.DirPerm); err == nil {
							slog.InfoContext(ctx, "created missing bind directory for project deployment", "projectPath", projectPath, "source", volume.Source)
						}
					}
				}
				if err != nil && !errors.Is(err, acfs.ErrOutsideRoot) {
					return fmt.Errorf("bind source %s for service %s: %w", volume.Source, serviceName, err)
				}
			}
		}
		return nil
	}
}
