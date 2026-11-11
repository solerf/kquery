package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"kquery/internal/attrpath"
	"kquery/internal/printer"
	"kquery/internal/tree"
)

// --- fixtures -------------------------------------------------------------

// obj builds an unstructured object with kind/namespace/name/uid, labels and
// any extra top-level fields (spec, status).
func obj(kind, namespace, name, uid string, labels map[string]string, extra map[string]interface{}) *unstructured.Unstructured {
	meta := map[string]interface{}{"name": name, "uid": uid}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	if labels != nil {
		l := map[string]interface{}{}
		for k, v := range labels {
			l[k] = v
		}
		meta["labels"] = l
	}
	o := map[string]interface{}{"kind": kind, "metadata": meta}
	for k, v := range extra {
		o[k] = v
	}
	return &unstructured.Unstructured{Object: o}
}

func deployment(namespace, name, uid string, podLabels map[string]string, ready int64) *unstructured.Unstructured {
	tl := map[string]interface{}{}
	for k, v := range podLabels {
		tl[k] = v
	}
	return obj("Deployment", namespace, name, uid, podLabels, map[string]interface{}{
		"spec":   map[string]interface{}{"template": map[string]interface{}{"metadata": map[string]interface{}{"labels": tl}}},
		"status": map[string]interface{}{"readyReplicas": ready},
	})
}

func owned(o *unstructured.Unstructured, ownerKind, ownerName, ownerUID string) *unstructured.Unstructured {
	refs, _, _ := unstructured.NestedSlice(o.Object, "metadata", "ownerReferences")
	refs = append(refs, map[string]interface{}{"kind": ownerKind, "name": ownerName, "uid": ownerUID, "apiVersion": "v1"})
	_ = unstructured.SetNestedSlice(o.Object, refs, "metadata", "ownerReferences")
	return o
}

func mustPaths(t *testing.T, raw ...string) []attrpath.Path {
	t.Helper()
	out := make([]attrpath.Path, 0, len(raw))
	for _, r := range raw {
		p, err := attrpath.Parse(r)
		if err != nil {
			t.Fatalf("Parse(%q): %v", r, err)
		}
		out = append(out, p)
	}
	return out
}

// sampleResults is two contexts × one namespace, with one failing namespace
// in the second context and a third context that failed outright.
func sampleResults() []ctxResult {
	ns := "shop"
	web := deployment(ns, "web", "d-web", map[string]string{"app": "web"}, 2)
	api := deployment(ns, "api", "d-api", map[string]string{"app": "api"}, 1)
	svc := obj("Service", ns, "web-svc", "s1", nil, map[string]interface{}{
		"spec": map[string]interface{}{"selector": map[string]interface{}{"app": "web"}},
	})
	ing := obj("Ingress", ns, "web-ingress", "i1", nil, map[string]interface{}{
		"spec": map[string]interface{}{"rules": []interface{}{map[string]interface{}{"http": map[string]interface{}{"paths": []interface{}{
			map[string]interface{}{"path": "/", "backend": map[string]interface{}{"service": map[string]interface{}{"name": "web-svc"}}},
		}}}}},
	})
	rs := owned(obj("ReplicaSet", ns, "web-abc", "rs1", nil, nil), "Deployment", "web", "d-web")
	pod := owned(obj("Pod", ns, "web-abc-xyz", "p1", map[string]string{"app": "web"}, nil), "ReplicaSet", "web-abc", "rs1")
	oldJob := obj("Job", ns, "old-job", "j1", map[string]string{"app.kubernetes.io/name": "retired"}, nil)

	westWeb := deployment(ns, "web", "d-web-w", map[string]string{"app": "web"}, 3)

	return []ctxResult{
		{
			context: "east",
			ns: []nsResult{{
				namespace: ns,
				rootKind:  "Deployment",
				items:     []*unstructured.Unstructured{web, api}, // unsorted on purpose
				children: map[string][]*unstructured.Unstructured{
					tree.KindService:    {svc},
					tree.KindIngress:    {ing},
					tree.KindReplicaSet: {rs},
					tree.KindPod:        {pod},
					tree.KindJob:        {oldJob},
				},
			}},
		},
		{
			context: "west",
			ns: []nsResult{
				{namespace: ns, rootKind: "Deployment", items: []*unstructured.Unstructured{westWeb}},
				{namespace: "missing", rootKind: "Deployment", err: errorsNew(`namespace "missing" not found`)},
			},
		},
		{context: "broken", err: errorsNew(`context "broken" not found in kubeconfig`)},
	}
}

type strErr string

func (e strErr) Error() string { return string(e) }
func errorsNew(s string) error { return strErr(s) }

func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	f()
	w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// --- dedupe ---------------------------------------------------------------

func TestDedupe(t *testing.T) {
	got := dedupe([]string{" east ", "", "west", "east", "  ", "west", "north"})
	want := "east,west,north"
	if strings.Join(got, ",") != want {
		t.Errorf("dedupe = %q, want %q", strings.Join(got, ","), want)
	}
	if got := dedupe(nil); len(got) != 0 {
		t.Errorf("dedupe(nil) = %v, want empty", got)
	}
}

// --- flat output ----------------------------------------------------------

func TestFlatTableCSV(t *testing.T) {
	tbl := flatTable(sampleResults(), mustPaths(t, "status.readyReplicas", "labels.app"))

	var buf bytes.Buffer
	if err := printer.Write(&buf, "csv", tbl); err != nil {
		t.Fatal(err)
	}
	// header = ctx,namespace,name + extras as typed; rows sorted by name within
	// a ctx/namespace; ctx order preserved; failed ns and failed ctx skipped
	want := strings.Join([]string{
		"ctx,namespace,name,status.readyReplicas,labels.app",
		"east,shop,api,1,api",
		"east,shop,web,2,web",
		"west,shop,web,3,web",
		"",
	}, "\n")
	if buf.String() != want {
		t.Errorf("csv output\n--- got ---\n%s--- want ---\n%s", buf.String(), want)
	}
}

func TestFlatTableNoExtrasAndClusterScoped(t *testing.T) {
	// cluster-scoped roots have no namespace: the cell is empty, the row still prints
	node := obj("Node", "", "ip-10-0-0-1", "n1", nil, nil)
	results := []ctxResult{{context: "east", ns: []nsResult{{rootKind: "Node", items: []*unstructured.Unstructured{node}}}}}
	tbl := flatTable(results, nil)

	var buf bytes.Buffer
	if err := printer.Write(&buf, "csv", tbl); err != nil {
		t.Fatal(err)
	}
	want := "ctx,namespace,name\neast,,ip-10-0-0-1\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestFlatTableTableFormatAligns(t *testing.T) {
	tbl := flatTable(sampleResults(), mustPaths(t, "status.readyReplicas"))
	var buf bytes.Buffer
	if err := printer.Write(&buf, "table", tbl); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want header + 3 rows, got %d lines:\n%s", len(lines), buf.String())
	}
	col := strings.Index(lines[0], "status.readyReplicas")
	for i, want := range []string{"1", "2", "3"} {
		if got := strings.TrimSpace(lines[i+1][col:]); got != want {
			t.Errorf("row %d: value under status.readyReplicas = %q, want %q (line %q)", i+1, got, want, lines[i+1])
		}
	}
}

// --- tree output ----------------------------------------------------------

func TestTreeBlocks(t *testing.T) {
	CLI.GroupLabel = "app.kubernetes.io/name"
	t.Cleanup(func() { CLI.GroupLabel = "" })

	blocks := treeBlocks(sampleResults(), mustPaths(t, "status.readyReplicas"))

	// one block per successful ctx/namespace, in CLI order; failures skipped
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Title != "east/shop" || blocks[1].Title != "west/shop" {
		t.Errorf("titles = %q, %q", blocks[0].Title, blocks[1].Title)
	}
	if strings.Join(blocks[0].Header, ",") != "status.readyReplicas" {
		t.Errorf("header = %v", blocks[0].Header)
	}

	var buf bytes.Buffer
	if err := printer.WriteTree(&buf, blocks); err != nil {
		t.Fatal(err)
	}
	// widest first-column cell is "          web-ingress" (21) + 2 padding = 23
	pad := func(s string) string { return s + strings.Repeat(" ", 23-len(s)) }
	want := strings.Join([]string{
		"east/shop",
		pad("name") + "status.readyReplicas",
		"Deployment", // Kind header, no values
		pad("  api") + "1",
		pad("  web") + "2",
		"    Service", // children one indent under the name, with their own Kind header
		"      web-svc",
		"        Ingress",
		"          web-ingress",
		"    Pod",
		"      web-abc-xyz",
		"(unattached)",
		"  Job",
		"    old-job",
		"",
		"west/shop",
		"name        status.readyReplicas", // narrower block: widths are per block ("Deployment" = 10 + 2)
		"Deployment",
		"  web       3",
		"",
	}, "\n")
	if buf.String() != want {
		t.Errorf("tree output\n--- got ---\n%s--- want ---\n%s", buf.String(), want)
	}
}

func TestTreeBlocksNoUnattachedLineWhenNothingLeft(t *testing.T) {
	dep := deployment("ns", "web", "d1", map[string]string{"app": "web"}, 1)
	results := []ctxResult{{context: "east", ns: []nsResult{{namespace: "ns", rootKind: "Deployment", items: []*unstructured.Unstructured{dep}}}}}
	blocks := treeBlocks(results, nil)
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d", len(blocks))
	}
	for _, l := range blocks[0].Lines {
		if l.Label == "(unattached)" {
			t.Errorf("(unattached) printed although nothing was unattached: %+v", blocks[0].Lines)
		}
	}
	var buf bytes.Buffer
	if err := printer.WriteTree(&buf, blocks); err != nil {
		t.Fatal(err)
	}
	// no --extra: no header row
	if want := "east/ns\nDeployment\n  web\n"; buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestTreeBlocksClusterScopedTitleIsBareContext(t *testing.T) {
	node := obj("Node", "", "ip-10-0-0-1", "n1", nil, nil)
	results := []ctxResult{{context: "east", ns: []nsResult{{rootKind: "Node", items: []*unstructured.Unstructured{node}}}}}
	blocks := treeBlocks(results, nil)
	if len(blocks) != 1 || blocks[0].Title != "east" {
		t.Fatalf("want one block titled %q, got %+v", "east", blocks)
	}
}

// --- stderr reporting -----------------------------------------------------

func TestReportErrors(t *testing.T) {
	results := sampleResults()
	results[0].note = "nodes is cluster-scoped; listed once, --namespace ignored"
	results[0].ns[0].notes = []string{"ScaledObject skipped: forbidden"}

	var failed bool
	out := captureStderr(t, func() { failed = reportErrors(results) })

	if !failed {
		t.Error("reportErrors = false, want true (one ns and one ctx failed)")
	}
	want := strings.Join([]string{
		"kquery: east: nodes is cluster-scoped; listed once, --namespace ignored",
		"kquery: east/shop: ScaledObject skipped: forbidden",
		`kquery: west/missing: namespace "missing" not found`,
		`kquery: broken: context "broken" not found in kubeconfig`,
		"",
	}, "\n")
	if out != want {
		t.Errorf("stderr\n--- got ---\n%s--- want ---\n%s", out, want)
	}
}

func TestReportErrorsAllGood(t *testing.T) {
	results := sampleResults()[:1]
	out := captureStderr(t, func() {
		if reportErrors(results) {
			t.Error("reportErrors = true, want false")
		}
	})
	if out != "" {
		t.Errorf("unexpected stderr: %q", out)
	}
}

// --- CLI parsing ----------------------------------------------------------

// zeroCLI is the CLI struct before any parse (captured at package init).
var zeroCLI = CLI

// parseCLI resets the real CLI struct, parses args into it (defaults
// applied) and restores it when the test ends. Parsing the real struct keeps
// the tests bound to the tags in main.go.
func parseCLI(t *testing.T, args ...string) error {
	t.Helper()
	saved := CLI
	t.Cleanup(func() { CLI = saved })
	CLI = zeroCLI
	p, err := kong.New(&CLI, kong.Name("kquery"), kong.Exit(func(int) {}), kong.Writers(io.Discard, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Parse(args)
	return err
}

func TestCLIDefaultsAndRepeatableFlags(t *testing.T) {
	if err := parseCLI(t, "-c", "east", "--ctx", "west", "-n", "shop"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(CLI.Contexts, ","); got != "east,west" {
		t.Errorf("Contexts = %q", got)
	}
	if got := strings.Join(CLI.Namespaces, ","); got != "shop" {
		t.Errorf("Namespaces = %q", got)
	}
	if CLI.Kind != "deployments" || CLI.Format != "tree" || CLI.GroupLabel != "app.kubernetes.io/name" || CLI.Timeout != 30*time.Second {
		t.Errorf("defaults: kind=%q format=%q group-label=%q timeout=%s", CLI.Kind, CLI.Format, CLI.GroupLabel, CLI.Timeout)
	}
	if len(CLI.Extras) != 0 {
		t.Errorf("Extras = %v, want none", CLI.Extras)
	}
}

func TestCLICommaSeparatedContextsButNotExtras(t *testing.T) {
	if err := parseCLI(t, "-c", "east,west", "-n", "a,b", "-e", "labels.app.kubernetes.io/name", "-e", "spec.template.spec.containers[0].image"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(CLI.Contexts, "|"); got != "east|west" {
		t.Errorf("Contexts = %q, want comma-split", got)
	}
	if got := strings.Join(CLI.Namespaces, "|"); got != "a|b" {
		t.Errorf("Namespaces = %q, want comma-split", got)
	}
	// sep:"none": a path is taken verbatim, dots and brackets included
	if got := strings.Join(CLI.Extras, "|"); got != "labels.app.kubernetes.io/name|spec.template.spec.containers[0].image" {
		t.Errorf("Extras = %q", got)
	}
}

func TestCLIRejectsBadInput(t *testing.T) {
	cases := map[string][]string{
		"missing --namespace": {"-c", "east"},
		"missing --ctx":       {"-n", "ns"},
		"unknown format":      {"-c", "east", "-n", "ns", "-f", "yaml"},
		"bad timeout":         {"-c", "east", "-n", "ns", "--timeout", "soon"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if err := parseCLI(t, args...); err == nil {
				t.Errorf("args %v: expected a parse error", args)
			}
		})
	}
}

// --- run(): usage errors surface before any cluster is contacted -----------

func TestRunUsageErrors(t *testing.T) {
	saved := CLI
	t.Cleanup(func() { CLI = saved })

	CLI.Contexts, CLI.Namespaces, CLI.Format = []string{"east"}, []string{"ns"}, "csv"

	CLI.Extras = []string{"spec."}
	if _, err := run(context.Background()); err == nil {
		t.Error("bad --extra path: expected an error")
	}

	CLI.Extras = nil
	CLI.Contexts = []string{" ", ""}
	if _, err := run(context.Background()); err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Errorf("blank contexts: got %v, want 'at least one ...' error", err)
	}
}
