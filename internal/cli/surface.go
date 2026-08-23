package cli

import "strings"

// The CLI's command surface, declared where the commands are built.
//
// RFC-BACKEND-2363 D2 classifies every command by WHAT IT NEEDS AT RUNTIME,
// because that decides where a test for it can run: a command that needs only
// the binary runs per-PR on a hosted runner of every OS, while one that needs a
// cluster needs a substrate somebody pays for. The classification therefore has
// to be readable by a machine — the coverage sweep selects on it.
//
// So the class lives HERE, on the cobra command, next to the code whose
// dependencies it describes: a new command declares its class in the same edit
// that wires it up, and surface_contract_test.go refuses the tree if one
// doesn't. The predecessor plan kept this as prose instead, and was wrong in
// its total, its composition and its arithmetic simultaneously (backend#2366).
//
// A class describes the command's OWN needs, not its children's: `data` is a
// dispatch node (classDispatchOnly) even though `data ingest` needs a cluster.
const (
	// runtimeClassAnnotation carries one of the class constants below. EVERY
	// command in the tree must carry it — see TestEveryCommandDeclaresItsRuntimeClass.
	runtimeClassAnnotation = "tracebloc.io/runtime-class"

	// runtimeEscalationsAnnotation carries the flags that make a command need
	// MORE than its base class, as a comma-separated `flag=class` list — e.g.
	// `auth status` reads local config, but `auth status --check` calls the
	// backend. The named flag must exist on the command (the contract test
	// checks that), so removing or renaming it reddens rather than rotting.
	runtimeEscalationsAnnotation = "tracebloc.io/runtime-class-escalations"
)

// The five runtime classes of RFC-BACKEND-2363 D2, plus the dispatch node.
const (
	classBinaryOnly     = "a"       // the binary only — no network, no credentials
	classLocalHost      = "a-prime" // local host + GitHub releases, no tracebloc service
	classBackend        = "b"       // backend + auth, no cluster
	classBackendCluster = "b+c"     // backend and cluster
	classCluster        = "c"       // cluster / installed client only
	classDispatchOnly   = "group"   // routes to children; bare, it only prints help
)

// runtimeClasses is the closed set the annotation may name. A value outside it
// is a typo, and a typo that fell through would silently drop the command out
// of whichever class a coverage sweep selected on.
var runtimeClasses = map[string]bool{
	classBinaryOnly:     true,
	classLocalHost:      true,
	classBackend:        true,
	classBackendCluster: true,
	classCluster:        true,
	classDispatchOnly:   true,
}

// classesNeedingCluster names the classes that require a reachable cluster.
// This is the half of the classification the contract test can DERIVE from the
// code rather than take on trust — a command that talks to a cluster takes a
// --kubeconfig, and one that doesn't, doesn't — so it is the half a
// mis-declared class cannot hide in.
var classesNeedingCluster = map[string]bool{
	classCluster:        true,
	classBackendCluster: true,
}

// runtimeClassFor declares a command's class, for use in a cobra.Command
// literal: `Annotations: runtimeClassFor(classCluster)`.
func runtimeClassFor(class string) map[string]string {
	return map[string]string{runtimeClassAnnotation: class}
}

// runtimeClassForWith declares a class plus the flags that escalate it, e.g.
// `runtimeClassForWith(classBackend, "seal="+classBackendCluster)`.
func runtimeClassForWith(class string, escalations ...string) map[string]string {
	a := runtimeClassFor(class)
	if len(escalations) > 0 {
		a[runtimeEscalationsAnnotation] = strings.Join(escalations, ",")
	}
	return a
}
