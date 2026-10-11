package testutil

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"

	"github.com/getarcaneapp/arcane/cli/v2/pkg"
)

var stdoutCaptureMu sync.Mutex

// WriteConfig writes isolated configuration for command tests.
func WriteConfig(t *testing.T, serverURL string, preview ...bool) string {
	t.Helper()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "arcanecli.yml")
	configContent := strings.Join([]string{
		"server_url: " + serverURL,
		"api_key: arc_test_key",
		"default_environment: \"0\"",
		"previews:\n  allEnvironments: " + strconv.FormatBool(len(preview) > 0 && preview[0]),
		"log_level: info",
		"",
	}, "\n")
	{
		err := os.WriteFile(configPath, []byte(configContent), 0o600)
		require.NoError(t, err,
			"write config: %v", err)
	}

	return configPath
}

// Execute runs the root command with isolated flags and captures its output.
func Execute(t *testing.T, args []string) (string, string, error) {
	t.Helper()

	root := cli.RootCommand()
	resetCommandFlags(t, root)
	errOut := &strings.Builder{}
	root.SetErr(errOut)
	root.SetArgs(args)

	stdoutCaptureMu.Lock()
	defer stdoutCaptureMu.Unlock()

	stdout := os.Stdout
	reader, writer, err := os.Pipe()

	require.NoError(t, err,
		"pipe: %v", err)

	os.Stdout = writer
	defer func() {
		_ = writer.Close()
		os.Stdout = stdout
	}()

	runErr := root.Execute()

	_ = writer.Close()
	stdoutBytes, readErr := io.ReadAll(reader)

	require.NoError(t, readErr,
		"read stdout: %v", readErr)

	outBuf := string(stdoutBytes)
	if runtime.GOOS == "windows" {
		outBuf = strings.ReplaceAll(outBuf, "\r\n", "\n")
	}

	return outBuf, errOut.String(), runErr
}

func resetCommandFlags(t *testing.T, root *cobra.Command) {
	t.Helper()

	if root == nil {
		return
	}

	for _, flagSet := range []*pflag.FlagSet{root.PersistentFlags(), root.Flags()} {
		flagSet.VisitAll(func(flag *pflag.Flag) {
			defValue := flag.DefValue
			if (flag.Value.Type() == "stringSlice" || flag.Value.Type() == "stringArray") && defValue == "[]" {
				defValue = ""
			}
			require.NoError(t, flag.Value.Set(defValue), "reset flag %s", flag.Name)
			flag.Changed = false
		})
	}
	for _, cmd := range root.Commands() {
		resetCommandFlags(t, cmd)
	}
}
