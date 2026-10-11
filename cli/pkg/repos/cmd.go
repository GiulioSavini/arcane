package repos

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/getarcaneapp/arcane/types/v2/base"
	"github.com/getarcaneapp/arcane/types/v2/gitops"
	"github.com/spf13/cobra"
	"go.getarcane.app/kit/pkg"

	"github.com/getarcaneapp/arcane/cli/v2/internal/client"
	"github.com/getarcaneapp/arcane/cli/v2/internal/cmdutil"
	"github.com/getarcaneapp/arcane/cli/v2/internal/output"
	"github.com/getarcaneapp/arcane/cli/v2/internal/types"
)

// Command is the parent command for git repository operations.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "repos",
		Aliases: []string{"repo", "git-repositories", "git-repos"},
		Short:   "Manage git repositories",
	}
	cmd.AddCommand(newListCmd())
	cmd.AddCommand(newCreateCmd())
	cmd.AddCommand(newGetCmd())
	cmd.AddCommand(newUpdateCmd())
	cmd.AddCommand(newDeleteCmd())
	cmd.AddCommand(newTestCmd())
	cmd.AddCommand(newBranchesCmd())
	cmd.AddCommand(newFilesCmd())
	return cmd
}

func newListCmd() *cobra.Command {
	var opts struct {
		limit int
		start int
		all   bool
	}
	cmd := &cobra.Command{
		Use:          "list",
		Aliases:      []string{"ls"},
		Short:        "List git repositories",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.NewFromConfig()
			if err != nil {
				return err
			}

			return cmdutil.RunList(cmd, c, cmdutil.ListSpec[gitops.GitRepository]{
				Resource: "repositories",
				Endpoint: types.GitRepositories(),
				Params:   cmdutil.ListParams{Resource: "repos", Limit: opts.limit, FallbackDefault: 20, Start: opts.start, All: opts.all},
				Headers:  []string{"ID", "NAME", "URL", "AUTH TYPE", "ENABLED", "CREATED"},
				Row: func(repo gitops.GitRepository) []string {
					return []string{
						repo.ID,
						repo.Name,
						repo.URL,
						repo.AuthType,
						strconv.FormatBool(repo.Enabled),
						repo.CreatedAt.Format("2006-01-02 15:04:05"),
					}
				},
			})
		},
	}
	cmd.Flags().IntVarP(&opts.limit, "limit", "n", 20, "Number of repositories to show")
	cmd.Flags().IntVar(&opts.start, "start", 0, cmdutil.StartFlagUsage)
	cmd.Flags().BoolVarP(&opts.all, "all", "a", false, cmdutil.AllFlagUsage)
	return cmd
}

func newCreateCmd() *cobra.Command {
	var opts struct {
		name             string
		url              string
		authType         string
		token            string
		username         string
		sshKey           string
		sshHostKeyVerify string
		authorName       string
		authorEmail      string
		signingKey       string
		signingKeyPass   string
		description      string
		enabled          bool
	}
	cmd := &cobra.Command{
		Use:          "create",
		Short:        "Create a git repository",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.NewFromConfig()
			if err != nil {
				return err
			}

			var sshKey string
			if cmd.Flags().Changed("ssh-key") {
				sshKeyData, readFileErr := os.ReadFile(opts.sshKey)
				if readFileErr != nil {
					return fmt.Errorf("failed to read SSH key file: %w", readFileErr)
				}
				sshKey = string(sshKeyData)
			}
			var signingKey string
			if cmd.Flags().Changed("signing-key") {
				signingKeyData, readSigningKeyErr := os.ReadFile(opts.signingKey)
				if readSigningKeyErr != nil {
					return fmt.Errorf("failed to read signing key file: %w", readSigningKeyErr)
				}
				signingKey = string(signingKeyData)
			}

			req := gitops.CreateRepositoryRequest{
				Name:                   opts.name,
				URL:                    opts.url,
				AuthType:               opts.authType,
				Token:                  opts.token,
				Username:               opts.username,
				SSHHostKeyVerification: opts.sshHostKeyVerify,
				CommitAuthorName:       opts.authorName,
				CommitAuthorEmail:      opts.authorEmail,
				SigningKeyPassphrase:   opts.signingKeyPass,
				Description:            kit.Ternary(cmd.Flags().Changed("description"), &opts.description, nil),
				Enabled:                kit.Ternary(cmd.Flags().Changed("enabled"), &opts.enabled, nil),
				SSHKey:                 sshKey,
				SigningKey:             signingKey,
			}

			return cmdutil.Run(cmd, c, cmdutil.Spec[gitops.GitRepository]{
				Method:         http.MethodPost,
				Path:           types.GitRepositories(),
				Body:           req,
				FailureMessage: "failed to create repository",
				Render: func(data gitops.GitRepository) error {
					output.Success("Repository created successfully")
					output.KeyValue("ID", data.ID)
					output.KeyValue("Name", data.Name)
					output.KeyValue("URL", data.URL)
					output.KeyValue("Auth Type", data.AuthType)
					output.KeyValue("Enabled", data.Enabled)
					return nil
				},
			})
		},
	}
	cmd.Flags().StringVar(&opts.name, "name", "", "Repository name")
	cmd.Flags().StringVar(&opts.url, "url", "", "Repository URL")
	cmdutil.EnumVarP(cmd, cmd.Flags(), &opts.authType, "auth-type", "", "", "Authentication type", "none", "http", "ssh")
	cmd.Flags().StringVar(&opts.token, "token", "", "Token for HTTP authentication")
	cmd.Flags().StringVar(&opts.username, "username", "", "Username for HTTP authentication")
	cmd.Flags().StringVar(&opts.sshKey, "ssh-key", "", "Path to SSH key file")
	cmdutil.EnumVarP(cmd, cmd.Flags(), &opts.sshHostKeyVerify, "ssh-host-key-verification", "", "", "SSH host key verification", "strict", "accept_new", "skip")
	cmd.Flags().StringVar(&opts.authorName, "author-name", "", "Author name for commits Arcane pushes")
	cmd.Flags().StringVar(&opts.authorEmail, "author-email", "", "Author email for commits Arcane pushes")
	cmd.Flags().StringVar(&opts.signingKey, "signing-key", "", "Path to an armored OpenPGP private key used to sign commits")
	cmd.Flags().StringVar(&opts.signingKeyPass, "signing-key-passphrase", "", "Passphrase for the signing key")
	cmd.Flags().StringVar(&opts.description, "description", "", "Repository description")
	cmd.Flags().BoolVar(&opts.enabled, "enabled", true, "Enable the repository")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("url")
	_ = cmd.MarkFlagRequired("auth-type")
	return cmd
}

func newGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "get <repository>",
		Short:        "Get git repository details",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.NewFromConfig()
			if err != nil {
				return err
			}

			resolved, _, err := repoRef.Resolve(cmd.Context(), c, args[0], cmdutil.AllowPrompt(cmd))
			if err != nil {
				return err
			}

			if cmdutil.JSONOutputEnabled(cmd) {
				return cmdutil.PrintJSON(resolved)
			}

			output.Header("Repository Details")
			output.KeyValue("ID", resolved.ID)
			output.KeyValue("Name", resolved.Name)
			output.KeyValue("URL", resolved.URL)
			output.KeyValue("Auth Type", resolved.AuthType)
			if resolved.Username != "" {
				output.KeyValue("Username", resolved.Username)
			}
			if resolved.SSHHostKeyVerification != "" {
				output.KeyValue("SSH Host Key Verification", resolved.SSHHostKeyVerification)
			}
			if resolved.CommitAuthorName != "" || resolved.CommitAuthorEmail != "" {
				output.KeyValue("Commit Author", strings.TrimSpace(resolved.CommitAuthorName+" <"+resolved.CommitAuthorEmail+">"))
			}
			output.KeyValue("Signing Key", resolved.HasSigningKey)
			if resolved.Description != nil {
				output.KeyValue("Description", *resolved.Description)
			}
			output.KeyValue("Enabled", resolved.Enabled)
			output.KeyValue("Created", resolved.CreatedAt.Format("2006-01-02 15:04:05"))
			output.KeyValue("Updated", resolved.UpdatedAt.Format("2006-01-02 15:04:05"))
			return nil
		},
	}
}

func newUpdateCmd() *cobra.Command {
	var opts struct {
		name             string
		url              string
		authType         string
		token            string
		username         string
		sshKey           string
		sshHostKeyVerify string
		authorName       string
		authorEmail      string
		signingKey       string
		signingKeyPass   string
		description      string
		enabled          bool
	}
	cmd := &cobra.Command{
		Use:          "update <repository>",
		Short:        "Update a git repository",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.NewFromConfig()
			if err != nil {
				return err
			}

			resolved, _, err := repoRef.Resolve(cmd.Context(), c, args[0], cmdutil.AllowPrompt(cmd))
			if err != nil {
				return err
			}

			var sshKey *string
			if cmd.Flags().Changed("ssh-key") {
				sshKeyData, readFileErr := os.ReadFile(opts.sshKey)
				if readFileErr != nil {
					return fmt.Errorf("failed to read SSH key file: %w", readFileErr)
				}
				sshKey = new(string(sshKeyData))
			}
			var signingKey *string
			if cmd.Flags().Changed("signing-key") {
				signingKeyValue := ""
				if opts.signingKey != "" {
					signingKeyData, readSigningKeyErr := os.ReadFile(opts.signingKey)
					if readSigningKeyErr != nil {
						return fmt.Errorf("failed to read signing key file: %w", readSigningKeyErr)
					}
					signingKeyValue = string(signingKeyData)
				}
				signingKey = &signingKeyValue
			}

			req := gitops.UpdateRepositoryRequest{
				Name:                   kit.Ternary(cmd.Flags().Changed("name"), &opts.name, nil),
				URL:                    kit.Ternary(cmd.Flags().Changed("url"), &opts.url, nil),
				AuthType:               kit.Ternary(cmd.Flags().Changed("auth-type"), &opts.authType, nil),
				Token:                  kit.Ternary(cmd.Flags().Changed("token"), &opts.token, nil),
				Username:               kit.Ternary(cmd.Flags().Changed("username"), &opts.username, nil),
				SSHHostKeyVerification: kit.Ternary(cmd.Flags().Changed("ssh-host-key-verification"), &opts.sshHostKeyVerify, nil),
				CommitAuthorName:       kit.Ternary(cmd.Flags().Changed("author-name"), &opts.authorName, nil),
				CommitAuthorEmail:      kit.Ternary(cmd.Flags().Changed("author-email"), &opts.authorEmail, nil),
				SigningKeyPassphrase:   kit.Ternary(cmd.Flags().Changed("signing-key-passphrase"), &opts.signingKeyPass, nil),
				Description:            kit.Ternary(cmd.Flags().Changed("description"), &opts.description, nil),
				Enabled:                kit.Ternary(cmd.Flags().Changed("enabled"), &opts.enabled, nil),
				SSHKey:                 sshKey,
				SigningKey:             signingKey,
			}

			return cmdutil.Run(cmd, c, cmdutil.Spec[gitops.GitRepository]{
				Method:         http.MethodPut,
				Path:           types.GitRepository(resolved.ID),
				Body:           req,
				FailureMessage: "failed to update repository",
				Render: func(data gitops.GitRepository) error {
					output.Success("Repository updated successfully")
					output.KeyValue("ID", data.ID)
					output.KeyValue("Name", data.Name)
					output.KeyValue("URL", data.URL)
					output.KeyValue("Auth Type", data.AuthType)
					output.KeyValue("Enabled", data.Enabled)
					return nil
				},
			})
		},
	}
	cmd.Flags().StringVar(&opts.name, "name", "", "Repository name")
	cmd.Flags().StringVar(&opts.url, "url", "", "Repository URL")
	cmdutil.EnumVarP(cmd, cmd.Flags(), &opts.authType, "auth-type", "", "", "Authentication type", "none", "http", "ssh")
	cmd.Flags().StringVar(&opts.token, "token", "", "Token for HTTP authentication")
	cmd.Flags().StringVar(&opts.username, "username", "", "Username for HTTP authentication")
	cmd.Flags().StringVar(&opts.sshKey, "ssh-key", "", "Path to SSH key file")
	cmdutil.EnumVarP(cmd, cmd.Flags(), &opts.sshHostKeyVerify, "ssh-host-key-verification", "", "", "SSH host key verification", "strict", "accept_new", "skip")
	cmd.Flags().StringVar(&opts.authorName, "author-name", "", "Author name for commits Arcane pushes")
	cmd.Flags().StringVar(&opts.authorEmail, "author-email", "", "Author email for commits Arcane pushes")
	cmd.Flags().StringVar(&opts.signingKey, "signing-key", "", "Path to an armored OpenPGP private key used to sign commits (empty clears it)")
	cmd.Flags().StringVar(&opts.signingKeyPass, "signing-key-passphrase", "", "Passphrase for the signing key")
	cmd.Flags().StringVar(&opts.description, "description", "", "Repository description")
	cmd.Flags().BoolVar(&opts.enabled, "enabled", true, "Enable the repository")
	return cmd
}

func newDeleteCmd() *cobra.Command {
	var opts struct {
		force bool
	}
	cmd := &cobra.Command{
		Use:          "delete <repository>",
		Aliases:      []string{"rm", "remove"},
		Short:        "Delete a git repository",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.NewFromConfig()
			if err != nil {
				return err
			}

			resolved, _, err := repoRef.Resolve(cmd.Context(), c, args[0], cmdutil.AllowPrompt(cmd))
			if err != nil {
				return err
			}

			return cmdutil.RunDelete(cmd, c, cmdutil.DeleteSpec{
				Force:          opts.force,
				Prompt:         fmt.Sprintf("Are you sure you want to delete repository %s (%s)?", resolved.Name, resolved.ID),
				Path:           types.GitRepository(resolved.ID),
				Body:           nil,
				FailureMessage: "failed to delete repository",
				SuccessMessage: "Repository deleted successfully",
			})
		},
	}
	cmd.Flags().BoolVarP(&opts.force, "force", "f", false, "Force deletion without confirmation")
	return cmd
}

func newTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "test <repository>",
		Short:        "Test git repository connection",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.NewFromConfig()
			if err != nil {
				return err
			}

			resolved, _, err := repoRef.Resolve(cmd.Context(), c, args[0], cmdutil.AllowPrompt(cmd))
			if err != nil {
				return err
			}

			return cmdutil.Run(cmd, c, cmdutil.Spec[base.MessageResponse]{
				Method:         http.MethodPost,
				Path:           types.GitRepositoryTest(resolved.ID),
				FailureMessage: "repository connection test failed",
				Render: func(base.MessageResponse) error {
					output.Success("%s", "Repository connection test successful")
					return nil
				},
			})
		},
	}
}

func newBranchesCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "branches <repository>",
		Short:        "List branches for a git repository",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.NewFromConfig()
			if err != nil {
				return err
			}

			resolved, _, err := repoRef.Resolve(cmd.Context(), c, args[0], cmdutil.AllowPrompt(cmd))
			if err != nil {
				return err
			}

			return cmdutil.Run(cmd, c, cmdutil.Spec[gitops.BranchesResponse]{
				Method:         http.MethodGet,
				Path:           types.GitRepositoryBranches(resolved.ID),
				FailureMessage: "failed to list branches",
				Render: func(data gitops.BranchesResponse) error {
					headers := []string{"BRANCH", "DEFAULT"}
					rows := make([][]string, len(data.Branches))
					for i, branch := range data.Branches {
						isDefault := ""
						if branch.IsDefault {
							isDefault = "*"
						}
						rows[i] = []string{
							branch.Name,
							isDefault,
						}
					}

					if tableErr := output.Table(headers, rows); tableErr != nil {
						return tableErr
					}
					_, _ = fmt.Fprintf(output.Stdout(), "\nTotal: %d branches\n", len(data.Branches))
					return nil
				},
			})
		},
	}
}

func newFilesCmd() *cobra.Command {
	var opts struct {
		branch string
		path   string
	}
	cmd := &cobra.Command{
		Use:          "files <repository>",
		Short:        "List files in a git repository",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client.NewFromConfig()
			if err != nil {
				return err
			}

			resolved, _, err := repoRef.Resolve(cmd.Context(), c, args[0], cmdutil.AllowPrompt(cmd))
			if err != nil {
				return err
			}

			path := types.GitRepositoryFiles(resolved.ID)
			params := url.Values{}
			if opts.branch != "" {
				params.Set("branch", opts.branch)
			}
			if opts.path != "" {
				params.Set("path", opts.path)
			}
			if len(params) > 0 {
				path = cmdutil.AppendQuery(path, params)
			}

			return cmdutil.Run(cmd, c, cmdutil.Spec[gitops.BrowseResponse]{
				Method:         http.MethodGet,
				Path:           path,
				FailureMessage: "failed to list files",
				Render: func(data gitops.BrowseResponse) error {
					files := data.Files
					headers := []string{"NAME", "TYPE", "PATH", "SIZE"}
					rows := make([][]string, len(files))
					for i, node := range files {
						size := ""
						if node.Type == gitops.FileTreeNodeTypeFile {
							size = strconv.FormatInt(node.Size, 10)
						}
						rows[i] = []string{
							node.Name,
							string(node.Type),
							node.Path,
							size,
						}
					}

					if tableErr := output.Table(headers, rows); tableErr != nil {
						return tableErr
					}
					_, _ = fmt.Fprintf(output.Stdout(), "\nTotal: %d entries\n", len(files))
					return nil
				},
			})
		},
	}
	cmd.Flags().StringVar(&opts.branch, "branch", "", "Branch to browse (required)")
	_ = cmd.MarkFlagRequired("branch")
	cmd.Flags().StringVar(&opts.path, "path", "", "Path within repository")
	return cmd
}

var repoRef = cmdutil.ResourceRef[gitops.GitRepository, gitops.GitRepository]{
	Singular: "repository",
	Plural:   "repositories",
	IDHint:   "the repository ID",
	ListCmd:  "arcane repos list",
	GetPath:  func(_, identifier string) string { return types.GitRepository(identifier) },
	ListPath: func(string) string { return types.GitRepositories() },
	Matches: func(item gitops.GitRepository, identifierLower, original string) bool {
		return strings.EqualFold(item.Name, original) || strings.EqualFold(item.ID, original) ||
			strings.HasPrefix(strings.ToLower(item.ID), identifierLower) || strings.Contains(strings.ToLower(item.Name), identifierLower)
	},
	Label: func(match gitops.GitRepository) string {
		return fmt.Sprintf("%s (%s)", match.Name, match.ID)
	},
	Promote: func(match gitops.GitRepository) *gitops.GitRepository { return &match },
	Exact: func(item gitops.GitRepository, _, original string) bool {
		return strings.EqualFold(item.Name, original) || strings.EqualFold(item.ID, original)
	},
}
