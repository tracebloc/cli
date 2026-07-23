package cli

import (
	"strings"
	"testing"
)

// The prepare-host command shells out to `bash -c prepareHostInstallerCmd`. If
// curl fails, a plain `curl | bash` pipeline still exits 0 (bash reads empty
// stdin), so the command would report success while nothing ran. `set -o
// pipefail` is what makes curl's failure propagate — guard it against removal
// (Bugbot #394).
func TestPrepareHostCmdUsesPipefail(t *testing.T) {
	if !strings.HasPrefix(prepareHostInstallerCmd, "set -o pipefail;") {
		t.Fatalf("prepareHostInstallerCmd must start with `set -o pipefail;` so a curl failure isn't swallowed; got: %q", prepareHostInstallerCmd)
	}
	if !strings.Contains(prepareHostInstallerCmd, "prepare-host") {
		t.Fatalf("prepareHostInstallerCmd should invoke the installer's prepare-host step; got: %q", prepareHostInstallerCmd)
	}
}

func TestPrepareHostCmdMetadata(t *testing.T) {
	c := newPrepareHostCmd()
	if c.Use != "prepare-host" {
		t.Errorf("Use = %q, want prepare-host", c.Use)
	}
	// NoArgs: prepare-host takes no positional arguments.
	if err := c.Args(c, []string{"unexpected"}); err == nil {
		t.Error("prepare-host should reject positional arguments (cobra.NoArgs)")
	}
}
