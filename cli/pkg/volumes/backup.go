package volumes

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/getarcaneapp/arcane/types/v2/base"
	uploadtypes "github.com/getarcaneapp/arcane/types/v2/upload"
	"github.com/getarcaneapp/arcane/types/v2/volume"
	"github.com/spf13/cobra"
	kit "go.getarcane.app/kit/pkg"

	"github.com/getarcaneapp/arcane/cli/v2/internal/client"
	"github.com/getarcaneapp/arcane/cli/v2/internal/cmdutil"
	"github.com/getarcaneapp/arcane/cli/v2/internal/output"
	"github.com/getarcaneapp/arcane/cli/v2/internal/prompt"
	"github.com/getarcaneapp/arcane/cli/v2/internal/types"
)

var (
	backupCreateDestination   string
	backupCreateS3Destination string
	backupCreatePolicyID      string
	backupUploadS3Destination string
	restorePaths              []string
	restoreArchiveFile        string
	policyEnabled             bool
	policySchedule            string
	policyRetention           int
	policyStopContainers      bool
	policyLocal               bool
	policyS3                  bool
	policyS3Destination       string
	policyID                  string
	policyFile                string
)

// backupsListResponse mirrors the backend's paginated volume backup response,
// which carries warnings alongside the standard pagination envelope.
type backupsListResponse struct {
	Success    bool                    `json:"success"`
	Data       []volume.BackupEntry    `json:"data"`
	Pagination base.PaginationResponse `json:"pagination"`
	Warnings   []string                `json:"warnings,omitempty"`
}

var renameCmd = &cobra.Command{
	Use:          "rename <volume-name> <new-name>",
	Short:        "Rename an unused volume",
	Args:         cobra.ExactArgs(2),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := cmdutil.ClientFromCommand(cmd)
		if err != nil {
			return err
		}

		allowPrompt := !forceFlag && !jsonOutput && prompt.IsInteractive()
		resolved, _, err := volumeRef.Resolve(cmd.Context(), c, args[0], allowPrompt)
		if err != nil {
			return err
		}

		if !forceFlag {
			confirmed, confirmErr := cmdutil.Confirm(cmd, fmt.Sprintf("Rename volume %s to %s? The volume is copied and the source is removed.", resolved.Name, args[1]))
			if confirmErr != nil {
				return confirmErr
			}
			if !confirmed {
				_, _ = fmt.Fprintln(output.Stdout(), "Cancelled")
				return nil
			}
		}

		result, err := c.PostJSON[volume.Volume](cmd.Context(), types.VolumeRename(c.EnvID(), resolved.Name), volume.Rename{Name: args[1]})
		if err != nil {
			return fmt.Errorf("failed to rename volume: %w", err)
		}

		if jsonOutput {
			return cmdutil.PrintJSON(result.Data)
		}

		output.Success("Volume %s renamed to %s", resolved.Name, result.Data.Name)
		return nil
	},
}

var backupsPolicyCmd = &cobra.Command{
	Use:          "policy <volume-name>",
	Short:        "Show volume backup policies",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := cmdutil.ClientFromCommand(cmd)
		if err != nil {
			return err
		}

		allowPrompt := !jsonOutput && prompt.IsInteractive()
		resolved, _, err := volumeRef.Resolve(cmd.Context(), c, args[0], allowPrompt)
		if err != nil {
			return err
		}

		result, err := c.GetJSON[volume.BackupPolicyCollection](cmd.Context(), types.VolumeBackupPolicy(c.EnvID(), resolved.Name))
		if err != nil {
			return fmt.Errorf("failed to get backup policies: %w", err)
		}

		if jsonOutput {
			return cmdutil.PrintJSON(result.Data)
		}

		return printBackupPolicies(resolved.Name, result.Data)
	},
}

var backupsPolicyUpdateCmd = &cobra.Command{
	Use:          "update <volume-name>",
	Short:        "Update volume backup policies",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := cmdutil.ClientFromCommand(cmd)
		if err != nil {
			return err
		}

		allowPrompt := !jsonOutput && prompt.IsInteractive()
		resolved, _, err := volumeRef.Resolve(cmd.Context(), c, args[0], allowPrompt)
		if err != nil {
			return err
		}

		var payload volume.UpdateBackupPolicies
		if policyFile != "" {
			raw, readFileErr := os.ReadFile(policyFile)
			if readFileErr != nil {
				return fmt.Errorf("failed to read policy file: %w", readFileErr)
			}
			if unmarshalErr := json.Unmarshal(raw, &payload); unmarshalErr != nil {
				return fmt.Errorf("failed to parse policy file: %w", unmarshalErr)
			}
		} else {
			current, policyErr := c.GetJSON[volume.BackupPolicyCollection](cmd.Context(), types.VolumeBackupPolicy(c.EnvID(), resolved.Name))
			if policyErr != nil {
				return fmt.Errorf("failed to get current backup policies: %w", policyErr)
			}

			payload.Policies = make([]volume.UpdateBackupPolicy, len(current.Data.Policies))
			targetIndex := -1
			for i, p := range current.Data.Policies {
				payload.Policies[i] = volume.UpdateBackupPolicy{
					ID:              p.ID,
					Enabled:         p.Enabled,
					Schedule:        p.Schedule,
					RetentionCount:  p.RetentionCount,
					StopContainers:  p.StopContainers,
					LocalEnabled:    p.LocalEnabled,
					S3Enabled:       p.S3Enabled,
					S3DestinationID: p.S3DestinationID,
				}
				if policyID != "" && p.ID == policyID {
					targetIndex = i
				}
			}

			switch {
			case policyID != "" && targetIndex == -1:
				return fmt.Errorf("policy %q not found for volume %q", policyID, resolved.Name)
			case policyID == "" && len(payload.Policies) > 1:
				return fmt.Errorf("volume %q has %d backup policies; select one with --policy-id", resolved.Name, len(payload.Policies))
			case policyID == "" && len(payload.Policies) == 1:
				targetIndex = 0
			case targetIndex == -1:
				if policySchedule == "" {
					return errors.New("--schedule is required when creating a new backup policy")
				}
				// New policies default to local storage unless --local was set explicitly.
				payload.Policies = append(payload.Policies, volume.UpdateBackupPolicy{LocalEnabled: !cmd.Flags().Changed("local") || policyLocal})
				targetIndex = len(payload.Policies) - 1
			}

			target := &payload.Policies[targetIndex]
			if cmd.Flags().Changed("enabled") {
				target.Enabled = policyEnabled
			}
			if cmd.Flags().Changed("schedule") {
				target.Schedule = policySchedule
			}
			if cmd.Flags().Changed("retention") {
				target.RetentionCount = policyRetention
			}
			if cmd.Flags().Changed("stop-containers") {
				target.StopContainers = policyStopContainers
			}
			if cmd.Flags().Changed("local") {
				target.LocalEnabled = policyLocal
			}
			if cmd.Flags().Changed("s3") {
				target.S3Enabled = policyS3
			}
			if cmd.Flags().Changed("s3-destination") {
				target.S3DestinationID = policyS3Destination
			}
		}

		result, err := c.PutJSON[volume.BackupPolicyCollection](cmd.Context(), types.VolumeBackupPolicy(c.EnvID(), resolved.Name), payload)
		if err != nil {
			return fmt.Errorf("failed to update backup policies: %w", err)
		}

		if jsonOutput {
			return cmdutil.PrintJSON(result.Data)
		}

		output.Success("Backup policies for volume %s saved successfully", resolved.Name)
		return printBackupPolicies(resolved.Name, result.Data)
	},
}

func printBackupPolicies(volumeName string, collection volume.BackupPolicyCollection) error {
	output.Header("Backup Policies: %s", volumeName)
	output.KeyValue("S3 Available", collection.S3Available)
	if len(collection.Policies) == 0 {
		_, writeErr := fmt.Fprintln(output.Stdout(), "No backup policies configured")
		return writeErr
	}

	headers := []string{"ID", "ENABLED", "SCHEDULE", "RETENTION", "STOP CONTAINERS", "LOCAL", "S3", "S3 DESTINATION"}
	rows := make([][]string, len(collection.Policies))
	for i, p := range collection.Policies {
		s3Destination := cmp.Or(p.S3DestinationName, p.S3DestinationID)
		rows[i] = []string{
			p.ID,
			output.TintEnabled(kit.Ternary(p.Enabled, "Yes", "No")),
			p.Schedule,
			strconv.Itoa(p.RetentionCount),
			kit.Ternary(p.StopContainers, "Yes", "No"),
			kit.Ternary(p.LocalEnabled, "Yes", "No"),
			kit.Ternary(p.S3Enabled, "Yes", "No"),
			s3Destination,
		}
	}
	return output.Table(headers, rows)
}

var backupsCmd = &cobra.Command{
	Use:          "backups <volume-name>",
	Short:        "Manage volume backups",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runListBackups,
}

var backupsListCmd = &cobra.Command{
	Use:          "list <volume-name>",
	Aliases:      []string{"ls"},
	Short:        "List volume backups",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runListBackups,
}

func runListBackups(cmd *cobra.Command, args []string) error {
	c, err := cmdutil.ClientFromCommand(cmd)
	if err != nil {
		return err
	}

	allowPrompt := !jsonOutput && prompt.IsInteractive()
	resolved, _, err := volumeRef.Resolve(cmd.Context(), c, args[0], allowPrompt)
	if err != nil {
		return err
	}

	path, err := cmdutil.ApplyPaginationParams(cmd, types.VolumeBackups(c.EnvID(), resolved.Name), cmdutil.ListParams{
		Resource:        "backups",
		Limit:           limitFlag,
		FallbackDefault: 20,
		Start:           startFlag,
		All:             allFlag,
	})
	if err != nil {
		return fmt.Errorf("failed to build pagination query: %w", err)
	}

	result, err := c.DoJSON[backupsListResponse](cmd.Context(), http.MethodGet, path, nil)
	if err != nil {
		return fmt.Errorf("failed to list backups: %w", err)
	}

	if jsonOutput {
		return cmdutil.PrintJSON(result)
	}

	headers := []string{"ID", "SIZE", "CREATED", "STATUS", "TRIGGER", "DESTINATION", "FORMAT"}
	rows := make([][]string, len(result.Data))
	for i, backup := range result.Data {
		rows[i] = []string{
			backup.ID,
			output.Bytes(backup.Size),
			backup.CreatedAt,
			output.TintStatus(backup.Status),
			backup.Trigger,
			string(backup.Destination),
			string(backup.Format),
		}
	}

	if tableErr := output.Table(headers, rows); tableErr != nil {
		return tableErr
	}
	output.Showing(len(result.Data), result.Pagination.TotalItems, "backups")
	for _, warning := range result.Warnings {
		output.Warning("%s", warning)
	}
	return nil
}

var backupsCreateCmd = &cobra.Command{
	Use:          "create <volume-name>",
	Short:        "Create a volume backup",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := cmdutil.ClientFromCommand(cmd)
		if err != nil {
			return err
		}

		allowPrompt := !jsonOutput && prompt.IsInteractive()
		resolved, _, err := volumeRef.Resolve(cmd.Context(), c, args[0], allowPrompt)
		if err != nil {
			return err
		}

		req := volume.CreateBackupRequest{
			Destination:     volume.BackupDestination(backupCreateDestination),
			PolicyID:        backupCreatePolicyID,
			S3DestinationID: backupCreateS3Destination,
		}

		result, err := c.PostJSON[volume.BackupEntry](cmd.Context(), types.VolumeBackups(c.EnvID(), resolved.Name), req)
		if err != nil {
			return fmt.Errorf("failed to create backup: %w", err)
		}

		if jsonOutput {
			return cmdutil.PrintJSON(result.Data)
		}

		output.Success("Backup started for volume %s", resolved.Name)
		output.KeyValue("ID", result.Data.ID)
		output.KeyValue("Status", result.Data.Status)
		output.KeyValue("Destination", string(result.Data.Destination))
		if result.Data.Size > 0 {
			output.KeyValue("Size", output.Bytes(result.Data.Size))
		}
		return nil
	},
}

var backupsRestoreCmd = &cobra.Command{
	Use:   "restore <volume-name> [backup-id]",
	Short: "Restore a volume from a backup",
	Long: `Restore a volume from an existing backup, or from a local tar.gz archive.

With a backup ID the whole backup is restored; add one or more --path flags to
restore only those paths. With --file a local tar.gz backup archive is uploaded
and restored instead (no backup ID).`,
	Args:         cobra.RangeArgs(1, 2),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if restoreArchiveFile != "" && len(args) == 2 {
			return errors.New("--file and a backup ID are mutually exclusive")
		}
		if restoreArchiveFile == "" && len(args) < 2 {
			return errors.New("a backup ID or --file is required")
		}
		if restoreArchiveFile != "" && len(restorePaths) > 0 {
			return errors.New("--path cannot be used with --file")
		}

		c, err := cmdutil.ClientFromCommand(cmd)
		if err != nil {
			return err
		}

		allowPrompt := !forceFlag && prompt.IsInteractive()
		resolved, _, err := volumeRef.Resolve(cmd.Context(), c, args[0], allowPrompt)
		if err != nil {
			return err
		}

		backupID := ""
		confirmation := fmt.Sprintf("Restore volume %s from %s? Current volume contents will be replaced.", resolved.Name, restoreArchiveFile)
		if restoreArchiveFile == "" {
			backupID = args[1]
			confirmation = fmt.Sprintf("Restore volume %s from backup %s? Current volume contents will be replaced.", resolved.Name, backupID)
			if len(restorePaths) > 0 {
				confirmation = fmt.Sprintf("Restore %d path(s) into volume %s from backup %s?", len(restorePaths), resolved.Name, backupID)
			}
		}
		if !forceFlag {
			confirmed, confirmErr := cmdutil.Confirm(cmd, confirmation)
			if confirmErr != nil {
				return confirmErr
			}
			if !confirmed {
				_, _ = fmt.Fprintln(output.Stdout(), "Cancelled")
				return nil
			}
		}
		if restoreArchiveFile != "" {
			c.SetTimeout(30 * time.Minute)
			sessionID, uploadErr := cmdutil.UploadFileInChunks(cmd.Context(), c, uploadtypes.KindVolumeBackup, restoreArchiveFile, !jsonOutput)
			if uploadErr != nil {
				return uploadErr
			}
			respBody, restoreErr := c.DoRaw(cmd.Context(), http.MethodPost, types.VolumeBackupUploadRestore(c.EnvID(), resolved.Name), uploadtypes.ConsumeRequest{UploadID: sessionID})
			if restoreErr != nil {
				cmdutil.AbortUploadSession(cmd.Context(), c, uploadtypes.KindVolumeBackup, sessionID)
				return fmt.Errorf("failed to upload and restore backup: %w", restoreErr)
			}
			if jsonOutput {
				return cmdutil.PrintRawJSON(respBody)
			}
			output.Success("Backup uploaded and restored into volume %s successfully", resolved.Name)
			return nil
		}
		spec := cmdutil.PostActionSpec{
			Path:           types.VolumeBackupRestore(c.EnvID(), resolved.Name, backupID),
			FailureMessage: "failed to restore backup",
			SuccessMessage: fmt.Sprintf("Restore of volume %s from backup %s initiated", resolved.Name, backupID),
			JSON:           jsonOutput,
		}
		if len(restorePaths) > 0 {
			spec.Path = types.VolumeBackupRestoreFiles(c.EnvID(), resolved.Name, backupID)
			spec.Body = struct {
				Paths []string `json:"paths"`
			}{Paths: restorePaths}
			spec.FailureMessage = "failed to restore backup files"
			spec.SuccessMessage = fmt.Sprintf("Restore of %d path(s) into volume %s initiated", len(restorePaths), resolved.Name)
		}
		return cmdutil.RunPostAction[base.MessageResponse](cmd, c, spec)
	},
}

var backupsDeleteCmd = &cobra.Command{
	Use:          "delete <backup-id>",
	Aliases:      []string{"rm", "remove"},
	Short:        "Delete a volume backup",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !forceFlag {
			confirmed, err := cmdutil.Confirm(cmd, fmt.Sprintf("Are you sure you want to delete backup %s?", args[0]))
			if err != nil {
				return err
			}
			if !confirmed {
				_, _ = fmt.Fprintln(output.Stdout(), "Cancelled")
				return nil
			}
		}

		c, err := cmdutil.ClientFromCommand(cmd)
		if err != nil {
			return err
		}

		if _, deleteBackupErr := c.DeleteJSON[base.MessageResponse](cmd.Context(), types.VolumeBackup(c.EnvID(), args[0])); deleteBackupErr != nil {
			return fmt.Errorf("failed to delete backup: %w", deleteBackupErr)
		}

		output.Success("Backup %s deleted successfully", args[0])
		return nil
	},
}

var backupsUploadCmd = &cobra.Command{
	Use:          "upload <backup-id>",
	Short:        "Upload an existing local backup to an S3 destination",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := cmdutil.ClientFromCommand(cmd)
		if err != nil {
			return err
		}

		// Uploading large backups can take a long time
		c.SetTimeout(30 * time.Minute)

		req := volume.UploadBackupRequest{S3DestinationID: backupUploadS3Destination}
		result, err := c.PostJSON[volume.BackupEntry](cmd.Context(), types.VolumeBackupUpload(c.EnvID(), args[0]), req)
		if err != nil {
			return fmt.Errorf("failed to upload backup: %w", err)
		}

		if jsonOutput {
			return cmdutil.PrintJSON(result.Data)
		}

		output.Success("Backup %s uploaded to S3 successfully", args[0])
		return nil
	},
}

var backupsDownloadCmd = &cobra.Command{
	Use:          "download <backup-id> [output-file]",
	Short:        "Download a volume backup archive",
	Args:         cobra.RangeArgs(1, 2),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := cmdutil.ClientFromCommand(cmd)
		if err != nil {
			return err
		}

		// Downloading large backups can take a long time
		c.SetTimeout(30 * time.Minute)

		resp, err := c.Get(cmd.Context(), types.VolumeBackupDownload(c.EnvID(), args[0]))
		if err != nil {
			return fmt.Errorf("failed to download backup: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if ensureSuccessStatusErr := cmdutil.EnsureSuccessStatus(resp); ensureSuccessStatusErr != nil {
			return fmt.Errorf("failed to download backup: %w", ensureSuccessStatusErr)
		}

		outputFile := ""
		if len(args) == 2 {
			outputFile = args[1]
		}
		if outputFile == "" {
			outputFile = args[0] + ".tar.gz"
			if _, params, parseErr := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); parseErr == nil {
				outputFile = cmp.Or(params["filename"], outputFile)
			}
		}

		file, openErr := os.OpenFile(outputFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if openErr != nil {
			return fmt.Errorf("failed to create file %s: %w", outputFile, openErr)
		}
		if _, copyErr := output.Copy(cmd.Context(), file, resp.Body, resp.ContentLength, "Downloading backup", !cmdutil.JSONOutputEnabled(cmd)); copyErr != nil {
			_ = file.Close()
			return fmt.Errorf("failed to write file %s: %w", outputFile, copyErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("failed to write file %s: %w", outputFile, closeErr)
		}

		output.Success("Backup downloaded to %s", outputFile)
		return nil
	},
}

var backupsFilesCmd = &cobra.Command{
	Use:          "files <backup-id>",
	Short:        "List files in a volume backup",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := cmdutil.ClientFromCommand(cmd)
		if err != nil {
			return err
		}

		result, err := c.GetJSON[[]string](cmd.Context(), types.VolumeBackupFiles(c.EnvID(), args[0]))
		if err != nil {
			return fmt.Errorf("failed to list backup files: %w", err)
		}

		if jsonOutput {
			return cmdutil.PrintJSON(result.Data)
		}

		for _, file := range result.Data {
			_, _ = fmt.Fprintln(output.Stdout(), file)
		}
		return nil
	},
}

func init() {
	cmdutil.SetResource(backupRef, backupsDeleteCmd, backupsUploadCmd, backupsDownloadCmd, backupsFilesCmd)
	VolumesCmd.AddCommand(renameCmd)
	VolumesCmd.AddCommand(backupsCmd)

	backupsCmd.AddCommand(backupsListCmd)
	backupsCmd.AddCommand(backupsPolicyCmd)
	backupsCmd.AddCommand(backupsCreateCmd)
	backupsCmd.AddCommand(backupsRestoreCmd)
	backupsCmd.AddCommand(backupsDeleteCmd)
	backupsCmd.AddCommand(backupsUploadCmd)
	backupsCmd.AddCommand(backupsDownloadCmd)
	backupsCmd.AddCommand(backupsFilesCmd)

	backupsPolicyCmd.AddCommand(backupsPolicyUpdateCmd)

	renameCmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Rename without confirmation")
	renameCmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")

	backupsPolicyCmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")

	backupsPolicyUpdateCmd.Flags().BoolVar(&policyEnabled, "enabled", false, "Enable or disable the policy")
	backupsPolicyUpdateCmd.Flags().StringVar(&policySchedule, "schedule", "", "Backup schedule (cron expression)")
	backupsPolicyUpdateCmd.Flags().IntVar(&policyRetention, "retention", 0, "Number of backups to retain")
	backupsPolicyUpdateCmd.Flags().BoolVar(&policyStopContainers, "stop-containers", false, "Stop containers using the volume during backup")
	backupsPolicyUpdateCmd.Flags().BoolVar(&policyLocal, "local", false, "Store backups locally")
	backupsPolicyUpdateCmd.Flags().BoolVar(&policyS3, "s3", false, "Store backups in S3")
	backupsPolicyUpdateCmd.Flags().StringVar(&policyS3Destination, "s3-destination", "", "Saved S3 destination ID")
	backupsPolicyUpdateCmd.Flags().StringVar(&policyID, "policy-id", "", "Policy to update when the volume has multiple policies")
	backupsPolicyUpdateCmd.Flags().StringVar(&policyFile, "file", "", "JSON file with the full policy collection to PUT")
	backupsPolicyUpdateCmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")

	for _, listLike := range []*cobra.Command{backupsCmd, backupsListCmd} {
		listLike.Flags().IntVarP(&limitFlag, "limit", "n", 20, "Number of backups to show")
		listLike.Flags().IntVar(&startFlag, "start", 0, cmdutil.StartFlagUsage)
		listLike.Flags().BoolVarP(&allFlag, "all", "a", false, cmdutil.AllFlagUsage)
		listLike.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	}

	backupsCreateCmd.Flags().StringVar(&backupCreateDestination, "destination", "", "Backup destination (local, s3, local_s3)")
	backupsCreateCmd.Flags().StringVar(&backupCreateS3Destination, "s3-destination", "", "Saved S3 destination ID for a remote backup")
	backupsCreateCmd.Flags().StringVar(&backupCreatePolicyID, "policy-id", "", "Backup policy whose settings should be used")
	backupsCreateCmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")

	backupsRestoreCmd.Flags().StringArrayVar(&restorePaths, "path", nil, "Restore only this path from the backup (repeatable)")
	backupsRestoreCmd.Flags().StringVar(&restoreArchiveFile, "file", "", "Local tar.gz backup archive to upload and restore")
	backupsRestoreCmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Restore without confirmation")
	backupsRestoreCmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")

	backupsDeleteCmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Delete without confirmation")

	backupsUploadCmd.Flags().StringVar(&backupUploadS3Destination, "s3-destination", "", "S3 destination for the uploaded backup")
	backupsUploadCmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	_ = backupsUploadCmd.MarkFlagRequired("s3-destination")

	backupsFilesCmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
}

var backupRef = cmdutil.ResourceRef[volume.BackupEntry, volume.BackupEntry]{
	EnvironmentScoped: true, Singular: "backup", Plural: "backups", IDHint: "the backup ID", ListCmd: "arcane-cli volumes backups list <volume-name>",
	SearchCandidates: func(ctx context.Context, c *client.Client, identifier string) ([]volume.BackupEntry, error) {
		volumes, err := c.GetJSON[[]volume.Volume](ctx, types.Volumes(c.EnvID())+"?limit=-1")
		if err != nil {
			return nil, err
		}
		var matches []volume.BackupEntry
		var failures []error
		for _, item := range volumes.Data {
			backups, listErr := c.GetJSON[[]volume.BackupEntry](ctx, types.VolumeBackups(c.EnvID(), item.Name)+"?limit=-1")
			if listErr != nil {
				failures = append(failures, fmt.Errorf("%s: %w", item.Name, listErr))
				continue
			}
			matches = append(matches, slices.DeleteFunc(backups.Data, func(entry volume.BackupEntry) bool { return entry.ID != identifier })...)
		}
		return matches, errors.Join(failures...)
	},
	Label:   func(item volume.BackupEntry) string { return item.VolumeName + " (" + item.ID + ")" },
	Promote: func(item volume.BackupEntry) *volume.BackupEntry { return &item },
}
