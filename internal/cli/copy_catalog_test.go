package cli

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/tracebloc/cli/internal/doctor"
	"github.com/tracebloc/cli/internal/push"
	"github.com/tracebloc/cli/internal/submit"
	"github.com/tracebloc/cli/internal/ui"
)

// TestCopyCatalog generates the copy catalog under testdata/golden/ — ONE file
// per command, so every user-facing string can be reviewed a screen at a time
// without deploying. Each file leads with what you see when you RUN the command
// (byte-exact, colour off), covers every state/path we can render deterministically,
// and ends with the command's `--help`. Copy that only appears mid-flow (ingest
// steps + progress, the login device flow, delete confirmation, and every
// failure remedy — many of which embed the launcher name, which varies by
// install) can't be pinned as a stable screen; zz-all-strings.golden is the
// completeness backstop for those — every user-facing string in the source.
//
// The test fails on drift; regenerate after an intentional copy change:
//
//	TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestCopyCatalog
//
// Files are ordered by how often a user hits them (00 = most). Adding a command
// is a single entry in the files map below.
func TestCopyCatalog(t *testing.T) {
	bi := BuildInfo{Version: "1.4.4", GitSHA: "0000000", BuildDate: "2026-01-01"}

	// help captures a command's `--help` through the REAL flag path (SetArgs+
	// Execute — exactly what the binary runs), byte-exact.
	help := func(path ...string) string {
		var b bytes.Buffer
		r := NewRootCmd(bi)
		r.SetOut(&b)
		r.SetErr(&b)
		r.SetArgs(append(append([]string{}, path...), "--help"))
		_ = r.Execute()
		return b.String()
	}
	// rndr renders one screen via the real renderer, colour off.
	rndr := func(f func(*ui.Printer)) string {
		var b bytes.Buffer
		f(ui.New(&b, ui.WithColor(false)))
		return b.String()
	}

	// doc assembles one command file: a title, a "when you see this" note, then
	// labelled `$ cmd` blocks (verbatim output), then the command's --help
	// block(s) at the bottom (one command can have several — e.g. client has
	// create/list/status).
	type run struct {
		cmd, out string // "$ <cmd>" then verbatim <out>
	}
	doc := func(title, whenSeen string, runs []run, helps []run) string {
		var s strings.Builder
		s.WriteString(title + "\n" + strings.Repeat("=", len([]rune(title))) + "\n")
		s.WriteString(whenSeen + "\n")
		for _, r := range runs {
			s.WriteString("\n$ " + r.cmd + "\n")
			s.WriteString(r.out)
		}
		if len(helps) > 0 {
			s.WriteString("\n\n" + strings.Repeat("-", 60) + "\n--help\n" + strings.Repeat("-", 60) + "\n")
			for i, h := range helps {
				if i > 0 {
					s.WriteString("\n")
				}
				s.WriteString("$ " + h.cmd + "\n")
				s.WriteString(h.out)
			}
		}
		return s.String()
	}

	// ── 00 home — every state resolveHomeModel can produce ──────────────────────
	online := homeModel{
		state: homeOnline, email: "lukas@tracebloc.io", name: "Lukas", envName: "hello-world",
		compute: computeInfo{CPU: 12, MemGiB: 23}, hasCompute: true, inv: binTB, fullMenu: true, hasResources: true,
	}
	noComp := online
	noComp.state, noComp.hasCompute, noComp.compute = homeRunning, false, computeInfo{}
	notOnline := noComp
	notOnline.confirmedNotOnline = true
	starting := noComp
	starting.state = homeStarting
	offline := noComp
	offline.state = homeOffline
	noEnv := noComp
	noEnv.state, noEnv.fullMenu, noEnv.envName = homeNoEnv, false, ""
	signedOut := homeModel{state: homeNotSignedIn, inv: binTB}

	homeFile := doc(
		"tb / tracebloc — home",
		"What you see when you run `tb` (or `tracebloc`) with no arguments. Covers every\nstate the home view resolves to.",
		[]run{
			{"tb   # signed in · secure environment Online", rndr(func(p *ui.Printer) { renderHome(p, online) })},
			{"tb   # signed in · running, couldn't confirm connection", rndr(func(p *ui.Printer) { renderHome(p, noComp) })},
			{"tb   # signed in · running, backend reports not online", rndr(func(p *ui.Printer) { renderHome(p, notOnline) })},
			{"tb   # signed in · starting up", rndr(func(p *ui.Printer) { renderHome(p, starting) })},
			{"tb   # signed in · offline (can't reach it)", rndr(func(p *ui.Printer) { renderHome(p, offline) })},
			{"tb   # signed in · no secure environment on this machine", rndr(func(p *ui.Printer) { renderHome(p, noEnv) })},
			{"tb   # not signed in", rndr(func(p *ui.Printer) { renderHome(p, signedOut) })},
		},
		[]run{{"tracebloc --help", help()}},
	)

	// ── 01 data ingest — stage a dataset (the guided questionnaire) ──────────────
	// driveIngest runs the REAL guided flow (runInteractive) with a prompter that
	// prints each question as the terminal shows it, so the transcript is every
	// prompt in order: intro, the core questions, the family sniff/echo, the task
	// picker, the task-specific questions, the review, and the confirm. The intro
	// preamble mirrors data_ingest_local.go (its text is drift-guarded by the
	// backstop); the temp data dir is normalised to a stable placeholder.
	driveIngest := func(dir, shownPath string, answers map[string]string) string {
		var b bytes.Buffer
		p := ui.New(&b, ui.WithColor(false))
		p.Newline()
		p.Para("Ingest datasets to your secure environment.")
		p.Hintf("For help: https://docs.tracebloc.io/create-use-case/prepare-dataset")
		pr := &catalogPrompter{w: &b, answers: answers}
		a := &runDataIngestArgs{}
		if err := runInteractive(p, pr, a, false /*taskSet*/); err != nil {
			t.Fatalf("driveIngest(%s): %v", dir, err)
		}
		return strings.ReplaceAll(b.String(), dir, shownPath)
	}
	tabDir := tabularDir(t)
	imgDir := imageDirLayout(t)
	txtDir := textDirLayout(t)
	tabularIngest := driveIngest(tabDir, "~/data/patients", map[string]string{
		"Do you want to ingest training or test data?": "train",
		"Please name the dataset.":                     "hospital_train",
		"Where is your data?":                          tabDir,
		"Which task?":                                  "tabular_classification",
		"Which column holds the label?":                "churned",
	})
	imageIngest := driveIngest(imgDir, "~/data/xray", map[string]string{
		"Do you want to ingest training or test data?": "train",
		"Please name the dataset.":                     "xray_train",
		"Where is your data?":                          imgDir,
		"Which task?":                                  "image_classification",
		"Which column holds the label?":                "label",
		"Image resolution":                             "224x224",
	})
	// Text family: text_classification shows the label question; the picker lists
	// every text task + blurb. (Self-supervised text — masked/causal LM, seq2seq
	// — skips the label step; that path is covered by the backstop.)
	textIngest := driveIngest(txtDir, "~/data/reviews", map[string]string{
		"Do you want to ingest training or test data?": "train",
		"Please name the dataset.":                     "reviews_train",
		"Where is your data?":                          txtDir,
		"Which task?":                                  "text_classification",
		"Which column holds the label?":                "label",
	})
	// execIngest renders the run that follows the confirm — the three steps and
	// the final summary. printLocalSummary + submit.RenderSummary are the REAL
	// renderers (drift-caught); the step headers/hints mirror the run
	// orchestration (their strings are drift-guarded by zz-all-strings). The raw
	// ingestor stream the CLI streams through (MySQL waits, the 📊 banner,
	// per-validator lines) is the *ingestor's* stdout — not CLI copy — so it
	// isn't shown: this is the CLI's own view of the run.
	execIngest := func() string {
		var b bytes.Buffer
		p := ui.New(&b, ui.WithColor(false))
		layout := &push.LocalLayout{
			Root:       "~/data/patients",
			LabelsCSV:  "~/data/patients/data.csv",
			TotalBytes: 52807,
		}
		spec := map[string]any{
			"category": "tabular_classification",
			"table":    "hospital_train",
			"intent":   "train",
			"label":    "churned",
			"schema": map[string]string{
				"age": "INT", "income": "FLOAT", "tenure": "INT", "balance": "FLOAT",
				"products": "INT", "active": "INT", "region": "VARCHAR(16)",
			},
		}
		p.Step(1, 3, "Check your data")
		p.Hintf("Reading your files locally first — nothing has touched your secure environment yet — so a layout or settings problem shows up right away.")
		// Guided path: the Review already echoed the settings, so the duplicate
		// "Ingest settings" block is suppressed (showSettings=false). The flag-only
		// path passes true; that block's copy is in zz-all-strings.golden.
		printLocalSummary(p, layout, spec, false)
		p.Step(2, 3, "Copy into your secure environment")
		p.Hintf("Your files are copied securely into your secure environment's storage — set up and cleaned up for you.")
		p.Step(3, 3, "Validate and load")
		p.Hintf("Submitting the run, then following along as tracebloc validates your data and loads it into the table — progress streams below.")
		p.Hintf("This follows the run for up to an hour; a longer run keeps going on its own (or start it with --detach and check back later).")
		submit.RenderSummary(p, &submit.Summary{
			IngestorID:       "80c224bd-202b-4a6f-9362-61c84599f334",
			TotalRecords:     849,
			ProcessedRecords: 849,
			InsertedRecords:  849,
			APISentRecords:   849,
		})
		return b.String()
	}
	dataIngestFile := doc(
		"tb data ingest — stage a dataset into your secure environment",
		"What you see when you run `tb data ingest` with no flags: a short intro, a\nfour-step guided setup (intent, name, path, task) then the task-specific\nquestions, and — after you confirm — the run itself. The setup is\ndriven through the real flow for one task in each family (tabular, image, text)\nso the task-specific questions are visible; each core question prints as a\n`Step N of 4 · …` header, the task-specific ones (the label column, and extras\nlike resolution or schema) as their own header, the\nsupporting line beneath it, and the `?` line shows your answer. The run (shown\nonce, for tabular) is the three steps + the final summary as the CLI renders\nthem. Passing flags (--as, --task, a path, …) skips the matching questions. The\nother tasks' extra questions (keypoints, label policy, time column),\nself-supervised text (which skips the label question), and the failure-summary\nwordings are in zz-all-strings.golden. The raw ingestor stream the CLI streams\nthrough (MySQL waits, the 📊 banner, per-validator lines) is the engine's own\nstdout — not CLI copy — so it isn't shown. (`tb ingest` is a hidden deprecated\nalias; `push` is a deprecated alias of the verb.)",
		[]run{
			{"tb data ingest   # guided · tabular classification", tabularIngest},
			{"tb data ingest   # guided · image classification", imageIngest},
			{"tb data ingest   # guided · text classification", textIngest},
			{"tb data ingest   # after you confirm — the run (tabular)", execIngest()},
		},
		[]run{
			{"tracebloc data ingest --help", help("data", "ingest")},
			{"tracebloc data validate --help", help("data", "validate")},
		},
	)

	// ── 02 data list ────────────────────────────────────────────────────────────
	sample := []push.DatasetInfo{
		{Name: "xray_train", Intent: "train", Task: "image_classification", Records: 12000, Classes: 2, Extension: "jpg", SizeBytes: 1 << 30},
		{Name: "xray_test", Intent: "test", Task: "image_classification", Records: 3000, Classes: 2, Extension: "jpg", SizeBytes: 256 << 20},
		{Name: "ingest_run_journal", System: true},
	}
	dataListFile := doc(
		"tb data list — list your datasets",
		"What you see when you run `tb data list`. Covers empty, populated, and --all.",
		[]run{
			{"tb data list   # no datasets yet", rndr(func(p *ui.Printer) { renderDataList(p, "hello-world", nil, false) })},
			{"tb data list   # with datasets (system tables hidden)", rndr(func(p *ui.Printer) { renderDataList(p, "hello-world", sample, false) })},
			{"tb data list --all   # including system tables", rndr(func(p *ui.Printer) { renderDataList(p, "hello-world", sample, true) })},
		},
		[]run{{"tracebloc data list --help", help("data", "list")}},
	)

	// ── 03 data delete ──────────────────────────────────────────────────────────
	dataDeleteFile := doc(
		"tb data delete — delete a dataset",
		"What you see when you run `tb data delete <dataset>`. The command confirms before\nit deletes; the confirmation prompt, the progress, and the success/failure lines\nstream during the flow (not a stable screen) — they're all in zz-all-strings.golden.",
		nil,
		[]run{{"tracebloc data delete --help", help("data", "delete")}},
	)

	// ── 04 resources ─────────────────────────────────────────────────────────────
	resourcesFile := doc(
		"tb resources — see / change what a training run may use",
		"What you see when you run `tb resources`. The view reads live cluster capacity,\nso it isn't a stable screen: it prints \"Your secure environment is equipped\nwith: …\", \"A training run is allocated up to: …\", and a hint to run\n`tb resources set` — all indexed in zz-all-strings.golden. `tb resources set` is\na guided walkthrough (prompts stream during the flow; also in the backstop).",
		nil,
		[]run{
			{"tracebloc resources --help", help("resources")},
			{"tracebloc resources set --help", help("resources", "set")},
		},
	)

	// ── 05 doctor ────────────────────────────────────────────────────────────────
	// doctorRollup mirrors runDoctor's rollup tail (doctor.go ~197-224) using the
	// REAL summarizeDoctor + renderHealth + doctorVerdict, so this copy is
	// drift-caught. Only launcher-free rollups are rendered here — the failure
	// lines ("Not connected — …", "Not ready — …") and their remedies embed the
	// launcher name (tb vs tracebloc, install-dependent) and so are catalogued in
	// zz-all-strings.golden instead.
	doctorRollup := func(p *ui.Printer, email, envName string, results []doctor.Result, tok tokenState) {
		p.Para("Signed in as " + email)
		p.Para(fmt.Sprintf("Secure environment %q", envName))
		connected, ready := summarizeDoctor(results, tok)
		p.Newline()
		renderHealth(p, connected)
		renderHealth(p, ready)
		p.Newline()
		fail, allGood := doctorVerdict(connected.status, ready.status)
		switch {
		case fail:
			p.Hintf("Still stuck? Email support@tracebloc.io with the output of `%s doctor --diagnose`.", launcher())
		case allGood:
			p.Successf("Everything looks good — you're ready to run training.")
		default:
			p.Infof("No problems found, but some checks couldn't finish — re-run with --verbose for detail.")
		}
	}
	// Connected + readiness unknown: Pod health warns with a list failure, which
	// summarizeDoctor maps to an honest "couldn't check your workloads".
	cantCheck := []doctor.Result{{Name: "Pod health", Status: doctor.StatusWarn, Detail: "could not list pods: forbidden"}}
	doctorFile := doc(
		"tb doctor — is my secure environment healthy?",
		"What you see when you run `tb doctor`. The two rollup lines (Connected, Ready)\nplus a verdict are shown below for the healthy and the can't-fully-check cases.\nThe failure variants (Not connected — …, Not ready — …) and their remedies vary\nwith the reachability classification and embed the launcher name, so the full set\nis indexed in zz-all-strings.golden. --verbose adds a Kubernetes breakdown\n(context/server/namespace + each granular check); those strings are in the\nbackstop too.",
		[]run{
			{"tb doctor   # healthy", rndr(func(p *ui.Printer) { doctorRollup(p, "lukas@tracebloc.io", "hello-world", nil, tokenOK) })},
			{"tb doctor   # connected, but a check couldn't complete (e.g. RBAC)", rndr(func(p *ui.Printer) { doctorRollup(p, "lukas@tracebloc.io", "hello-world", cantCheck, tokenOK) })},
		},
		[]run{
			{"tracebloc doctor --help", help("doctor")},
			{"tracebloc cluster doctor --help", help("cluster", "doctor")},
		},
	)

	// ── 06 delete (offboard this machine) ─────────────────────────────────────────
	deleteFile := doc(
		"tb delete — remove tracebloc from this machine",
		"What you see when you run `tb delete`. The pre-flight summary (below) is shown\nbefore you confirm, for both keep-data and remove-data. The confirmation prompt\nand the teardown progress stream during the flow — those strings are in\nzz-all-strings.golden.",
		[]run{
			{"tb delete   # summary · keep my data", rndr(func(p *ui.Printer) { renderOffboardSummary(p, "lukas-macbook", true) })},
			{"tb delete --remove-data   # summary · remove my data too", rndr(func(p *ui.Printer) { renderOffboardSummary(p, "lukas-macbook", false) })},
		},
		[]run{{"tracebloc delete --help", help("delete")}},
	)

	// ── 07 login / logout / auth ──────────────────────────────────────────────────
	loginFile := doc(
		"tb login / logout — sign in and out",
		"What you see when you run `tb login`. Sign-in is a device flow: the CLI prints an\n\"Open <url>\" line and an \"Enter <code>\" line, waits, then confirms — that copy\nstreams during the flow (not a stable screen), so it's in zz-all-strings.golden.\n`tb auth status` reports who you're signed in as.",
		nil,
		[]run{
			{"tracebloc login --help", help("login")},
			{"tracebloc logout --help", help("logout")},
			{"tracebloc auth status --help", help("auth", "status")},
		},
	)

	// ── 08 client ──────────────────────────────────────────────────────────────────
	// The seal check (client status --seal) renders through the pure
	// renderSealResult, so all three verdicts are pinned as stable screens: the
	// live run streams the same header/check/verdict pieces (plus a per-check
	// spinner line, catalogued in the backstop).
	sealSealed := sealModel{envName: "lukas-macbook", checks: []sealCheck{
		{name: "egress-enforcement", passed: true},
		{name: "backend-reachability", passed: true},
	}}
	sealUnsealed := sealModel{envName: "lukas-macbook", checks: []sealCheck{
		{name: "egress-enforcement", passed: false,
			detail: "job failed: BackoffLimitExceeded",
			hint:   "see why: kubectl logs -n lukas-macbook job/lukas-macbook-egress-enforcement-check"},
		{name: "backend-reachability", passed: true},
	}}
	sealUnknown := sealModel{envName: "lukas-macbook"}
	clientFile := doc(
		"tb client — register / list / inspect environments",
		"What you see under `tb client`. `tb client create` shows a review (below) before\nit registers a new secure environment. `tb client status --seal` verifies the\nenvironment's protections by running the chart's conformance checks (the seal\ncheck) — all three verdicts are shown: sealed, unsealed (with the per-check\nfailure + fix hint), and unknown (a chart with no checks is honestly NOT called\nsealed). `tb client list` / plain `tb client status` read live backend state, so\nthey aren't stable screens — their strings are in zz-all-strings.golden.",
		[]run{
			{"tb client create   # review, before you confirm", rndr(func(p *ui.Printer) { renderClientReview(p, "lukas-macbook", "lukas-macbook", "DE", "a1b2c3d4") })},
			{"tb client status --seal   # sealed — every conformance check passed", rndr(func(p *ui.Printer) { renderSealResult(p, sealSealed) })},
			{"tb client status --seal   # unsealed — a protection is not enforced", rndr(func(p *ui.Printer) { renderSealResult(p, sealUnsealed) })},
			{"tb client status --seal   # unknown — this chart ships no conformance checks", rndr(func(p *ui.Printer) { renderSealResult(p, sealUnknown) })},
		},
		[]run{
			{"tracebloc client --help", help("client")},
			{"tracebloc client create --help", help("client", "create")},
			{"tracebloc client list --help", help("client", "list")},
			{"tracebloc client status --help", help("client", "status")},
		},
	)

	// ── 09 cluster ───────────────────────────────────────────────────────────────
	clusterFile := doc(
		"tb cluster — low-level cluster info",
		"What you see under `tb cluster`. `tb cluster info` reads live cluster state, so\nit isn't a stable screen; its strings are in zz-all-strings.golden. (`tb cluster\ndoctor` is the same health check as `tb doctor` — see 05-doctor.)",
		nil,
		[]run{
			{"tracebloc cluster --help", help("cluster")},
			{"tracebloc cluster info --help", help("cluster", "info")},
		},
	)

	// ── 10 version ───────────────────────────────────────────────────────────────
	versionFile := doc(
		"tb version — print the CLI version",
		"What you see when you run `tb version`. It prints one line:\n\n    tracebloc <version> (<git-sha>, built <date>, <go-version> on <os>/<arch>)\n\nThe go-version and os/arch are filled in at runtime, so the exact line varies by\nmachine (that's why it isn't pinned byte-exact here). `--output-json` emits the\nsame fields as indented JSON. Only the --help is byte-exact below.",
		nil,
		[]run{{"tracebloc version --help", help("version")}},
	)

	upgradeFile := doc(
		"tb upgrade — update to the latest release",
		"What you see when you run `tb upgrade`. On Linux/macOS it re-runs the\nofficial installer (signature-verified) to update the CLI and your secure\nenvironment together, so they never drift apart. On Windows a running .exe is\nlocked and install.ps1 is CLI-only, so it prints the command to run in a fresh\nshell instead. The update-check nudge and the \"CLI too old\" (426) message both\npoint here. The installer's own live output streams during the run (not a stable\nscreen); its copy is in the client repo's installer catalog.",
		nil,
		[]run{{"tracebloc upgrade --help", help("upgrade")}},
	)

	// ── 12 prepare-host ──────────────────────────────────────────────────────────
	prepareHostFile := doc(
		"tb prepare-host — one-time admin step so a non-admin can install",
		"What you see when you run `tb prepare-host` — the one-time administrator step\nthat readies a shared / HPC host so a non-admin user can then install tracebloc\nwith no root. It re-runs the installer's verified prepare-host step; the\nprivileged prep + its progress stream from the installer (not CLI copy). Only the\n--help is byte-exact below.",
		nil,
		[]run{{"tracebloc prepare-host --help", help("prepare-host")}},
	)

	files := map[string]string{
		"00-home.golden":         homeFile,
		"01-data-ingest.golden":  dataIngestFile,
		"02-data-list.golden":    dataListFile,
		"03-data-delete.golden":  dataDeleteFile,
		"04-resources.golden":    resourcesFile,
		"05-doctor.golden":       doctorFile,
		"06-delete.golden":       deleteFile,
		"07-login.golden":        loginFile,
		"08-client.golden":       clientFile,
		"09-cluster.golden":      clusterFile,
		"10-version.golden":      versionFile,
		"11-upgrade.golden":      upgradeFile,
		"12-prepare-host.golden": prepareHostFile,
		"zz-all-strings.golden": "every user-facing string in the source (AST-harvested — all arguments to the\n" +
			"Printer methods + errors.New/fmt.Errorf/fmt.Sprintf, plus the text/remedy\n" +
			"fields of healthLine{} and doctor.Result{} literals, both \"…\" and `…` raw\n" +
			"strings). The completeness backstop: catches the failure remedies and the\n" +
			"multi-step flows (ingest steps, login, progress, confirmations) not shown as a\n" +
			"screen. %s/%d are runtime placeholders.\n\n" + strings.Join(quoteAll(harvestMessages(t)), "\n") + "\n",
	}

	// Coverage guarantee: a command's PRIMARY PATH must be rendered as a screen,
	// not left to the string backstop — so a dropped/half-rendered flow fails the
	// test instead of silently vanishing from the catalog (which is how the whole
	// ingest execution went missing once). Each entry names markers that must
	// appear in that file's rendered content; add one when a file gains a phase.
	mustRender := map[string][]string{
		"00-home.golden": {"tracebloc --help"},
		"01-data-ingest.golden": {
			"Ingest datasets to your secure environment.", // intro
			"Step 1 of 4 ·", "Step 4 of 4 ·", // the guided questionnaire
			"Review", "Proceed with the ingest?", // review + confirm
			"Step 1/3", "Step 3/3", "Ingestion summary", "What's next", // the run
		},
		"02-data-list.golden": {"tracebloc data list --help"},
		"05-doctor.golden":    {"Connected to tracebloc", "Everything looks good"},
		"08-client.golden": { // the seal check's three verdicts (cli#393)
			"Sealed — all 2 conformance checks passed",
			"Unsealed — 1 of 2 conformance checks failed",
			"Seal unknown — this chart ships no conformance checks",
		},
	}
	for name, needles := range mustRender {
		got := files[name]
		for _, n := range needles {
			if !strings.Contains(got, n) {
				t.Errorf("%s is missing required primary-path copy %q — a screen may have been dropped or half-rendered", name, n)
			}
		}
	}

	update := os.Getenv("TB_UPDATE_GOLDEN") != ""
	for name, content := range files {
		path := filepath.Join("testdata/golden", name)
		if update {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s (regenerate: TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestCopyCatalog): %v", path, err)
		}
		if content != string(want) {
			t.Errorf("%s drifted. Regenerate + review the diff:\n  TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestCopyCatalog", path)
		}
	}
	if update {
		t.Logf("wrote %d catalog files under testdata/golden/", len(files))
	}
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strconv.Quote(s)
	}
	return out
}

// catalogPrompter is the prompter seam's catalog double: it prints each question
// the way the terminal shows it ("? <question> <answer>") and returns a scripted
// answer, so driving the REAL runInteractive produces a byte-exact transcript of
// the guided flow — every prompt, in order. The description line above each
// question is the real p.PromptHint in runInteractive; only the "? …" line is
// rendered here (survey draws it in production).
type catalogPrompter struct {
	w       *bytes.Buffer
	answers map[string]string
}

func (c *catalogPrompter) pick(label, def string) string {
	if a, ok := c.answers[label]; ok {
		return a
	}
	return def
}

// answerLine renders the input line the way the bare surveyPrompter does for the
// guided flow: the question is already printed by the CLI (PromptStep/Section),
// so the prompt shows only "? <answer>" (or "?" on a blank/accept-default).
func (c *catalogPrompter) answerLine(ans string) {
	if ans == "" {
		fmt.Fprintf(c.w, "?\n")
		return
	}
	fmt.Fprintf(c.w, "? %s\n", ans)
}

func (c *catalogPrompter) Input(label, _, def string, _ func(string) error) (string, error) {
	ans := c.pick(label, def)
	c.answerLine(ans)
	return ans, nil
}

func (c *catalogPrompter) Select(label, _ string, _ []string, def string) (string, error) {
	ans := c.pick(label, def)
	c.answerLine(ans)
	return ans, nil
}

// Confirm keeps its label (never bare — see surveyPrompter.Confirm): a y/N
// prompt has no header of its own, so survey draws "? <question> <answer>".
// Mirror that here so the catalog shows the confirm question.
func (c *catalogPrompter) Confirm(label string, def bool) (bool, error) {
	ans := "No"
	if def {
		ans = "Yes"
	}
	fmt.Fprintf(c.w, "? %s %s\n", label, ans)
	return def, nil
}

// harvestMessages parses the user-facing packages and returns every string
// literal that reaches a user: ALL arguments to a Printer method or an error /
// format constructor (errors.New, fmt.Errorf, fmt.Sprintf), PLUS the string
// fields of healthLine{} and doctor.Result{} composite literals — those carry
// user-facing text (the doctor rollup lines, check details + remedies) that is
// never passed to a Printer call, so an arguments-only harvest would miss it.
// Both "…" and `…` raw strings; deduped + sorted.
func harvestMessages(t *testing.T) []string {
	t.Helper()
	methods := map[string]bool{
		"Successf": true, "Warnf": true, "Errorf": true, "Infof": true, "Hintf": true,
		"Detailf": true, "Para": true, "Section": true, "PromptHint": true, "PromptHeader": true,
		"PromptStep": true,
		"WarnLine":   true, "CrossLine": true, "CheckLine": true, "Step": true, "Action": true,
		"Stat": true, "Field": true, "MenuRow": true, "Banner": true, "Command": true,
		// Spinner — live-wait lines ("Waiting for tracebloc to confirm…",
		// "Checking <check>…") print as a static line on non-TTY runs, so their
		// copy is user-facing too.
		"Spinner": true,
		// prompter seam (survey) — question labels + help text for every guided
		// flow (ingest, client create, delete), incl. flows not driven as a screen.
		"Input": true, "Select": true, "Confirm": true,
	}
	isCopyCall := func(call *ast.CallExpr) bool {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		if methods[sel.Sel.Name] {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); ok {
			return (x.Name == "errors" && sel.Sel.Name == "New") ||
				(x.Name == "fmt" && (sel.Sel.Name == "Errorf" || sel.Sel.Name == "Sprintf"))
		}
		return false
	}
	// healthLine{} (this package) and doctor.Result{} (this package as
	// doctor.Result, the doctor package as a bare Result) hold user-facing text
	// in struct fields, not call arguments.
	isCopyStruct := func(t ast.Expr) bool {
		switch tt := t.(type) {
		case *ast.Ident:
			return tt.Name == "healthLine" || tt.Name == "Result"
		case *ast.SelectorExpr:
			return tt.Sel.Name == "Result"
		}
		return false
	}

	seen := map[string]struct{}{}
	collect := func(exprs []ast.Expr) {
		for _, arg := range exprs {
			lit, ok := arg.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			s, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				continue
			}
			s = strings.TrimSpace(s)
			// Skip empties and format-only fragments (e.g. "%s", " ") — no words.
			if len([]rune(s)) < 4 || !strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyz") {
				continue
			}
			seen[s] = struct{}{}
		}
	}
	fset := token.NewFileSet()
	for _, dir := range []string{".", "../submit", "../push", "../doctor", "../cluster"} {
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CallExpr:
					if isCopyCall(node) {
						collect(node.Args)
					}
				case *ast.CompositeLit:
					if isCopyStruct(node.Type) {
						for _, el := range node.Elts {
							if kv, ok := el.(*ast.KeyValueExpr); ok {
								collect([]ast.Expr{kv.Value})
							} else {
								collect([]ast.Expr{el})
							}
						}
					}
				}
				return true
			})
			return nil
		})
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
