package cli

// ExitCodeFromError extracts an exit code from a handler-returned
// error. Handlers that need a specific exit code wrap their return
// in an *exitError (see ingest.go); everything else defaults to 1.
//
// Public-but-package-keyed: main.go is the only intended caller,
// and the exitError type itself stays unexported so subcommand
// handlers go through the constructor.
func ExitCodeFromError(err error) int {
	if err == nil {
		return exitOK
	}
	var ee *exitError
	if asExitError(err, &ee) {
		return ee.code
	}
	return exitFailure
}

// IsSilentError reports whether a handler-returned error wants
// main() to suppress its own "Error: ..." stderr line on the way
// out. The contract: a handler that has already printed a
// structured diagnostic itself (e.g. the schema-validate path
// prints per-violation lines to stderr) returns
// `&exitError{code: N, err: nil}` to signal "exit non-zero but
// don't print anything more." Errors with a non-nil inner err
// (file-read failures, parse errors, schema-compile bugs) are
// NOT silent — main() prints them so the customer doesn't see a
// bare non-zero exit with no explanation.
//
// Caller pattern in main.go:
//
//	if err != nil && !cli.IsSilentError(err) {
//	    fmt.Fprintln(os.Stderr, "Error:", err)
//	}
//	os.Exit(cli.ExitCodeFromError(err))
func IsSilentError(err error) bool {
	if err == nil {
		return false
	}
	var ee *exitError
	if asExitError(err, &ee) {
		return ee.err == nil
	}
	return false
}

// asExitError walks the wrapped-error chain looking for an
// *exitError. Same pattern as errors.As but with a typed target so
// callers don't have to import errors at every site.
func asExitError(err error, target **exitError) bool {
	for cur := err; cur != nil; cur = unwrapError(cur) {
		if ee, ok := cur.(*exitError); ok {
			*target = ee
			return true
		}
	}
	return false
}

func unwrapError(err error) error {
	type unwrapper interface{ Unwrap() error }
	if u, ok := err.(unwrapper); ok {
		return u.Unwrap()
	}
	return nil
}
