//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A CLI whose own binary was replaced after it started runs a different
// version than the service would start, so it must not drain the daemon.
func TestDaemonServiceReplacedCLIIsAnotherInstallation(t *testing.T) {
	path, err := os.Executable()
	require.NoError(t, err)
	if os.Getenv("ROBOREV_TEST_EXECUTABLE_REPLACEMENT") == "1" {
		same, err := (&daemonService{executable: path}).sameInstallation()
		require.NoError(t, err)
		require.True(t, same)
		require.NoError(t, os.WriteFile(path+".next", []byte("replacement executable"), 0o700))
		require.NoError(t, os.Rename(path+".next", path))
		same, err = (&daemonService{executable: path}).sameInstallation()
		require.NoError(t, err)
		assert.False(t, same)
		return
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	copyPath := filepath.Join(t.TempDir(), "roborev-test")
	require.NoError(t, os.WriteFile(copyPath, data, 0o700))
	cmd := exec.CommandContext(t.Context(), copyPath, "-test.run=^TestDaemonServiceReplacedCLIIsAnotherInstallation$")
	cmd.Env = append(os.Environ(), "ROBOREV_TEST_EXECUTABLE_REPLACEMENT=1")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestLookupLaunchdService(t *testing.T) {
	domain := "domain = {\n\tservices = {\n\t\t42 0 com.example.daemon\n\t}\n}\n"
	job := "job = {\n\ttype = LaunchAgent\n\tprogram = /example/roborev\n\tpid = 42\n}\n"
	// Exit errors preserve launchctl's absent-domain status through wrapping.
	exits := map[int]error{}
	for _, code := range []int{113, 125} {
		cmd := exec.CommandContext(t.Context(), "sh", "-c", fmt.Sprintf("exit %d", code))
		exits[code] = fmt.Errorf("launchctl: %w", cmd.Run())
	}
	for _, tc := range []struct {
		name                    string
		chain                   []int
		ancestryErr             error
		gui, job                string
		guiErr, userErr, jobErr error
		unit                    string
		wantErr                 bool
	}{
		{name: "wrapper child", chain: []int{43, 42, 1}, unit: "gui/501/com.example.daemon"},
		{name: "direct owner", chain: []int{42, 1}, unit: "gui/501/com.example.daemon"},
		{name: "app job", chain: []int{42, 1}, job: "\ttype = Application\n\tpid = 42\n"},
		{name: "no matching ancestor", chain: []int{99, 1}},
		{name: "gui absent", chain: []int{42, 1}, guiErr: exits[113], unit: "user/501/com.example.daemon"},
		{name: "gui unavailable", chain: []int{42, 1}, guiErr: exits[125], unit: "user/501/com.example.daemon"},
		{name: "same label different domain", chain: []int{42, 1}, gui: "\tservices = {\n\t\t99 0 com.example.daemon\n\t}\n", unit: "user/501/com.example.daemon"},
		{name: "both domains absent", chain: []int{42, 1}, guiErr: exits[113], userErr: exits[125]},
		{name: "domain print fails", chain: []int{42, 1}, guiErr: errors.New("domain unreadable"), wantErr: true},
		{name: "domain table missing", chain: []int{42, 1}, gui: "domain = {}", wantErr: true},
		{name: "job print fails", chain: []int{42, 1}, jobErr: errors.New("job unreadable"), wantErr: true},
		{name: "job vanished", chain: []int{42, 1}, jobErr: exits[113], wantErr: true},
		{name: "job type missing", chain: []int{42, 1}, job: "\tpid = 42\n", wantErr: true},
		{name: "job pid changed", chain: []int{42, 1}, job: "\ttype = LaunchAgent\n\tpid = 99\n", wantErr: true},
		{name: "vanished pid", chain: []int{43}, ancestryErr: os.ErrNotExist, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := assert.New(t)
			var calls [][]string
			deps := launchdDeps{uid: 501, ancestors: func(ctx context.Context, pid int) ([]int, error) {
				return tc.chain, tc.ancestryErr
			}, run: func(ctx context.Context, args ...string) ([]byte, error) {
				calls = append(calls, args)
				require.Len(t, args, 2)
				check.Equal("print", args[0])
				switch args[1] {
				case "gui/501":
					if tc.guiErr != nil {
						return nil, tc.guiErr
					}
					if tc.gui != "" {
						return []byte(tc.gui), nil
					}
					return []byte(domain), nil
				case "user/501":
					if tc.userErr != nil {
						return nil, tc.userErr
					}
					return []byte(domain), nil
				default:
					if tc.jobErr != nil {
						return nil, tc.jobErr
					}
					if tc.job != "" {
						return []byte(tc.job), nil
					}
					return []byte(job), nil
				}
			}}
			owner, err := lookupLaunchdService(t.Context(), tc.chain[0], deps)
			if tc.wantErr {
				var managed *managedRecoveryError
				require.ErrorAs(t, err, &managed)
				if tc.ancestryErr != nil {
					require.ErrorIs(t, err, os.ErrNotExist)
					check.Empty(calls)
				}
				return
			}
			require.NoError(t, err)
			if tc.unit == "" {
				check.Nil(owner)
				return
			}
			require.NotNil(t, owner)
			check.Equal(tc.unit, owner.unit)
			check.Equal(42, owner.mainPID)
			check.Equal("/example/roborev", owner.executable)
			check.Equal("simple", owner.serviceType)
			check.Equal([]string{"print", tc.unit}, calls[len(calls)-1])
		})
	}
}
