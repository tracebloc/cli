package resources

// contract.go carries the training-envelope contract, vendored from
// tracebloc/client-runtime (backend#2220, RFC-BACKEND-664 §P0).
//
// The question "how much of this machine may ONE training run have" used to be
// answered independently in four places: this package, the bash installer, its
// PowerShell twin, and a fifth 0.75-fraction policy inside client-runtime
// itself. None derived from the others, and two of them disagreed by
// construction — most sharply on the node tie-break, where this package ranked
// candidates (cpu, memory) and the bash installer ranked them (memory, cpu), so
// on a cluster of 8c/16Gi + 4c/32Gi `tracebloc resources set` and the installer
// anchored on DIFFERENT nodes and gave different answers about one machine.
//
// client-runtime now owns the arithmetic (node_sizing.envelope_from_allocatable)
// and its constants live in envelope_contract.json. Unlike the installers, Go
// needs no generator to read it: the file is embedded verbatim at compile time,
// so the vendored artifact is byte-identical to upstream and the cross-repo
// drift gate is a plain diff (.github/workflows/envelope-contract-drift.yml at
// scripts/.client-runtime-ref).
//
// What is NOT changed here is cli#143 Decision A. The number the user sets is
// still the per-run ceiling, written to RESOURCE_* verbatim; Overhead() is still
// a fit-check safety margin that is never subtracted from it. Only the duplicate
// *definition* of these four numbers is gone.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

// contractBytes is the raw contract as vendored. Exposed for the drift test,
// which asserts the embedded bytes still parse and still carry vectors.
//
//go:embed envelope_contract.json
var contractBytes []byte

// envelopeContract is the decoded shape. Only the fields this package needs are
// named: the anchors and rendering rules are the installers' and jobs-manager's
// business, and decoding them here would invite drift of a different kind.
type envelopeContract struct {
	ContractVersion int `json:"contract_version"`
	Overhead        struct {
		CPUMilli    int64 `json:"cpu_millicores"`
		MemoryBytes int64 `json:"memory_bytes"`
	} `json:"overhead"`
	Floor struct {
		CPUMilli    int64 `json:"cpu_millicores"`
		MemoryBytes int64 `json:"memory_bytes"`
	} `json:"floor"`
	Vectors struct {
		SingleNode []struct {
			Label             string `json:"label"`
			AllocatableCPU    string `json:"allocatable_cpu"`
			AllocatableMemory string `json:"allocatable_memory"`
			Expected          *struct {
				CPUMilli    int64 `json:"cpu_millicores"`
				MemoryBytes int64 `json:"memory_bytes"`
				Viable      bool  `json:"viable"`
				RenderGi    struct {
					CPU    string `json:"cpu"`
					Memory string `json:"memory"`
				} `json:"render_gi"`
			} `json:"expected"`
		} `json:"single_node"`
	} `json:"vectors"`
}

// mustContract decodes and validates the embedded contract exactly once.
//
// It panics on a malformed contract, the same way regexp.MustCompile does for a
// bad literal pattern: the file is embedded at COMPILE time, so the only way it
// can be invalid is a hand-edit or a botched re-vendor, and that is a broken
// build rather than a runtime condition a user could hit. Silently falling back
// to defaults would be worse than a panic — a default here is a fifth policy,
// which is the whole thing backend#2220 removes. TestContractIsValid keeps the
// panic from ever reaching a release.
var mustContract = sync.OnceValue(func() envelopeContract {
	var c envelopeContract
	if err := json.Unmarshal(contractBytes, &c); err != nil {
		panic(fmt.Sprintf("envelope_contract.json is not valid JSON: %v", err))
	}
	if c.ContractVersion < 1 {
		panic(fmt.Sprintf("envelope_contract.json has no usable contract_version: %d", c.ContractVersion))
	}
	for name, v := range map[string]int64{
		"overhead.cpu_millicores": c.Overhead.CPUMilli,
		"overhead.memory_bytes":   c.Overhead.MemoryBytes,
		"floor.cpu_millicores":    c.Floor.CPUMilli,
		"floor.memory_bytes":      c.Floor.MemoryBytes,
	} {
		if v <= 0 {
			panic(fmt.Sprintf("envelope_contract.json %s must be a positive int, got %d", name, v))
		}
	}
	return c
})
