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
		return 0
	}
	var ee *exitError
	if asExitError(err, &ee) {
		return ee.code
	}
	return 1
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
