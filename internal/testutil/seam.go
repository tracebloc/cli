// Package testutil holds small helpers shared across the CLI's test suites.
//
// Test-support only: production code must never import this package. It lives
// under internal/ like everything else, so the compiler can't stop a stray
// import — code review must.
package testutil

import "testing"

// SwapSeam replaces the value behind ptr — typically a package-level function
// variable used as a test seam (watchJobFn, newAPIClient, helm.Runner, …), but
// any swappable package var (e.g. a timeout) works — with stub for the
// duration of the test, restoring the original via t.Cleanup.
//
// It replaces the hand-rolled three-line save/stub/restore block:
//
//	orig := watchJobFn
//	watchJobFn = stub
//	t.Cleanup(func() { watchJobFn = orig })
//
// with a single call:
//
//	testutil.SwapSeam(t, &watchJobFn, stub)
//
// Cleanup ordering follows t.Cleanup semantics (LIFO), so nested swaps of the
// same seam restore correctly. NOT safe for use with t.Parallel() siblings
// that share the same seam — package-level seams never are.
func SwapSeam[T any](t testing.TB, ptr *T, stub T) {
	t.Helper()
	orig := *ptr
	*ptr = stub
	t.Cleanup(func() { *ptr = orig })
}
