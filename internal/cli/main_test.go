package cli

import (
	"os"
	"testing"
)

// TestMain clears BOTH stage-selecting env vars before the package's tests run, so
// no test inherits a developer's or CI runner's ambient stage from the process
// environment. This is load-bearing after RFC-0076 (backend#3391) made the reads
// alias-first: api.ResolveEnv now consults the canonical $TRACEBLOC_ENV BEFORE the
// legacy $CLIENT_ENV, so a test that pins the ambient stage by setting only one of
// the two names would be silently overridden by an ambient value of the other. A
// package-wide clean baseline fixes that for every current AND future site — the
// same class-not-instance guarantee the env-resolution guard in this package is
// built around — rather than relying on each test to neutralise both names.
func TestMain(m *testing.M) {
	os.Unsetenv("TRACEBLOC_ENV")
	os.Unsetenv("CLIENT_ENV")
	os.Exit(m.Run())
}
