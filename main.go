// kquery lists Kubernetes objects across kubeconfig contexts and namespaces
// and prints caller-chosen attribute paths as columns, either as a tree of
// related objects or flat.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kong"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"kquery/internal/attrpath"
	"kquery/internal/kube"
	"kquery/internal/printer"
	"kquery/internal/tree"
)

var CLI struct {
	Contexts   []string      `name:"ctx" short:"c" required:"" placeholder:"NAME" help:"Kubeconfig context to query. Repeatable (or comma-separated); at least one is required. Contexts are queried concurrently and printed in the order given."`
	Namespaces []string      `name:"namespace" short:"n" required:"" placeholder:"NS" help:"Namespace to query in every context. Repeatable (or comma-separated); at least one is required."`
	Extras     []string      `name:"extra" short:"e" sep:"none" placeholder:"PATH" help:"Attribute path to print as an extra column, taken verbatim as the column header; in the tree view it is resolved on every node. Repeatable. 'labels.<key>' and 'annotations.<key>' read metadata.labels/annotations with the whole remainder as the key (labels.app.kubernetes.io/name); any other path splits on dots and accepts list indices (spec.template.spec.containers[0].image). Missing values print as an empty cell; maps and lists print as compact JSON."`
	Kind       string        `name:"kind" short:"k" default:"deployments" placeholder:"RESOURCE" help:"Resource to list (the tree roots), in any form kubectl accepts: plural, singular, Kind, shortname or resource.group (deploy, Deployment, svc, ingress, scaledobjects.keda.sh)."`
	Kubeconfig string        `name:"kubeconfig" type:"path" placeholder:"FILE" help:"Kubeconfig file. Defaults to $KUBECONFIG, then ~/.kube/config."`
	Format     string        `name:"format" short:"f" default:"tree" enum:"tree,csv,table" help:"Output format. tree: per ctx/namespace, each root with its related Services (+Ingresses), Pods, Jobs (+Pods), HPAs, ScaledObjects and PDBs nested, then an (unattached) bucket for leftovers. csv / table: one flat row per root object."`
	GroupLabel string        `name:"group-label" default:"app.kubernetes.io/name" placeholder:"LABEL" help:"Tree view: a Job belongs to a root when both carry this label with the same value (Jobs have no selector or owner link to a Deployment). Empty disables Job grouping."`
	Timeout    time.Duration `name:"timeout" default:"30s" help:"Per-request timeout against each API server."`
}

const description = `List Kubernetes objects across kubeconfig contexts and namespaces and print
caller-chosen attribute paths as columns.

Every --ctx is combined with every --namespace. The default tree view shows,
per ctx/namespace, each object of --kind with the namespace objects related to
it nested beneath (Service > Ingress, Pod, Job > Pod, HorizontalPodAutoscaler,
ScaledObject, PodDisruptionBudget) and a final "(unattached)" pseudo-root with
everything of those kinds that matched no root. csv and table print one flat
row per root: ctx,namespace,name,<extra-1>,<extra-2>,...

Failures of one context or namespace (unknown context, unreachable cluster,
RBAC, missing namespace) are reported on stderr with a ctx/namespace prefix;
the other combinations are still printed and the exit code is 1.`

func main() {
	kctx := kong.Parse(&CLI,
		kong.Name("kquery"),
		kong.Description(description),
		kong.UsageOnError(),
	)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	code, err := run(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kquery: error: %v\n", err)
		kctx.Exit(2)
	}
	kctx.Exit(code)
}

// nsResult is the outcome of listing one namespace in one context.
type nsResult struct {
	namespace string
	rootKind  string
	items     []*unstructured.Unstructured
	children  map[string][]*unstructured.Unstructured // tree view only, keyed by Kind
	notes     []string                                // child kinds skipped (forbidden / absent)
	err       error
}

// ctxResult is the outcome of one context: a context-level error (bad
// context, unreachable cluster, unknown kind) fails every namespace at once.
type ctxResult struct {
	context string
	err     error
	note    string
	ns      []nsResult
}

// run returns the process exit code (0 = all combinations listed, 1 = at
// least one failed) or an error for usage-level problems.
func run(ctx context.Context) (int, error) {
	paths := make([]attrpath.Path, 0, len(CLI.Extras))
	for _, raw := range CLI.Extras {
		p, err := attrpath.Parse(raw)
		if err != nil {
			return 0, err
		}
		paths = append(paths, p)
	}
	contexts := dedupe(CLI.Contexts)
	namespaces := dedupe(CLI.Namespaces)
	if len(contexts) == 0 || len(namespaces) == 0 {
		return 0, fmt.Errorf("at least one non-empty --ctx and --namespace are required")
	}

	// One goroutine per context: each needs its own auth exchange and discovery
	// round trip, which dominate the runtime. Results are kept in CLI order.
	results := make([]ctxResult, len(contexts))
	var wg sync.WaitGroup
	for i, name := range contexts {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			results[i] = queryContext(ctx, name, namespaces, CLI.Format == "tree")
		}(i, name)
	}
	wg.Wait()

	failed := reportErrors(results)

	var err error
	if CLI.Format == "tree" {
		err = printer.WriteTree(os.Stdout, treeBlocks(results, paths))
	} else {
		err = printer.Write(os.Stdout, CLI.Format, flatTable(results, paths))
	}
	if err != nil {
		return 0, err
	}
	if failed {
		return 1, nil
	}
	return 0, nil
}

// reportErrors prints every context/namespace failure and skipped child kind
// to stderr, in CLI order, and reports whether any combination failed.
func reportErrors(results []ctxResult) bool {
	failed := false
	for _, cr := range results {
		if cr.err != nil {
			failed = true
			fmt.Fprintf(os.Stderr, "kquery: %s: %v\n", cr.context, cr.err)
			continue
		}
		if cr.note != "" {
			fmt.Fprintf(os.Stderr, "kquery: %s: %s\n", cr.context, cr.note)
		}
		for _, nr := range cr.ns {
			prefix := cr.context
			if nr.namespace != "" {
				prefix += "/" + nr.namespace
			}
			if nr.err != nil {
				failed = true
				fmt.Fprintf(os.Stderr, "kquery: %s: %v\n", prefix, nr.err)
				continue
			}
			for _, note := range nr.notes {
				fmt.Fprintf(os.Stderr, "kquery: %s: %s\n", prefix, note)
			}
		}
	}
	return failed
}

func resolveCells(paths []attrpath.Path, obj *unstructured.Unstructured) []string {
	cells := make([]string, 0, len(paths))
	for _, p := range paths {
		cells = append(cells, p.Resolve(obj.Object))
	}
	return cells
}

// flatTable builds the csv/table output: one row per root object.
func flatTable(results []ctxResult, paths []attrpath.Path) printer.Table {
	header := []string{"ctx", "namespace", "name"}
	for _, p := range paths {
		header = append(header, p.String())
	}
	t := printer.Table{Header: header}
	for _, cr := range results {
		if cr.err != nil {
			continue
		}
		for _, nr := range cr.ns {
			if nr.err != nil {
				continue
			}
			sort.Slice(nr.items, func(a, b int) bool { return nr.items[a].GetName() < nr.items[b].GetName() })
			for _, obj := range nr.items {
				row := append([]string{cr.context, obj.GetNamespace(), obj.GetName()}, resolveCells(paths, obj)...)
				t.Rows = append(t.Rows, row)
			}
		}
	}
	return t
}

// treeBlocks builds one block per ctx/namespace: resources grouped under
// Kind header lines, the related objects nested under each root and the
// leftovers under "(unattached)".
func treeBlocks(results []ctxResult, paths []attrpath.Path) []printer.TreeBlock {
	header := make([]string, 0, len(paths))
	for _, p := range paths {
		header = append(header, p.String())
	}
	var blocks []printer.TreeBlock
	for _, cr := range results {
		if cr.err != nil {
			continue
		}
		for _, nr := range cr.ns {
			if nr.err != nil {
				continue
			}
			title := cr.context
			if nr.namespace != "" {
				title += "/" + nr.namespace
			}
			block := printer.TreeBlock{Title: title, Header: header}
			// Siblings are grouped under one Kind header line (no extra values)
			// in first-seen order; each name sits one indent under its Kind and
			// its children start one indent under the name with their own headers.
			var emit func(nodes []*tree.Node, depth int)
			emit = func(nodes []*tree.Node, depth int) {
				var kinds []string
				byKind := map[string][]*tree.Node{}
				for _, n := range nodes {
					if _, seen := byKind[n.Kind]; !seen {
						kinds = append(kinds, n.Kind)
					}
					byKind[n.Kind] = append(byKind[n.Kind], n)
				}
				for _, kind := range kinds {
					block.Lines = append(block.Lines, printer.TreeLine{Depth: depth, Label: kind})
					for _, n := range byKind[kind] {
						block.Lines = append(block.Lines, printer.TreeLine{
							Depth: depth + 1,
							Label: n.Obj.GetName(),
							Cells: resolveCells(paths, n.Obj),
						})
						emit(n.Children, depth+2)
					}
				}
			}
			res := tree.Build(tree.Input{
				RootKind:   nr.rootKind,
				Roots:      nr.items,
				Children:   nr.children,
				GroupLabel: CLI.GroupLabel,
			})
			emit(res.Roots, 0)
			if len(res.Unattached) > 0 {
				block.Lines = append(block.Lines, printer.TreeLine{Label: "(unattached)"})
				emit(res.Unattached, 1)
			}
			blocks = append(blocks, block)
		}
	}
	return blocks
}

// queryContext resolves the kind once per context and lists it in every
// namespace; in tree mode it also lists the related child kinds per
// namespace. Cluster-scoped roots are listed once with --namespace ignored
// and get no children (relationships are namespace-local).
func queryContext(ctx context.Context, name string, namespaces []string, withChildren bool) ctxResult {
	r := ctxResult{context: name}
	client, err := kube.NewClient(CLI.Kubeconfig, name, CLI.Timeout)
	if err != nil {
		r.err = err
		return r
	}
	res, err := client.Resolve(CLI.Kind)
	if err != nil {
		r.err = err
		return r
	}
	if !res.Namespaced {
		r.note = fmt.Sprintf("%s is cluster-scoped; listed once, --namespace ignored", res.GVR.Resource)
		items, err := client.List(ctx, res, "")
		r.ns = []nsResult{{rootKind: res.Kind, items: items, err: err}}
		return r
	}
	for _, ns := range namespaces {
		nr := nsResult{namespace: ns, rootKind: res.Kind}
		nr.items, nr.err = client.List(ctx, res, ns)
		if nr.err == nil && len(nr.items) == 0 && client.NamespaceMissing(ctx, ns) {
			nr.err = fmt.Errorf("namespace %q not found", ns)
		}
		if nr.err == nil && withChildren {
			nr.children, nr.notes = fetchChildren(ctx, client, ns)
		}
		r.ns = append(r.ns, nr)
	}
	return r
}

// fetchChildren lists every tree.ChildResources kind once in namespace, all
// concurrently. A kind that cannot be resolved or listed is skipped with a
// note (silently when it is an optional CRD that the cluster does not have).
func fetchChildren(ctx context.Context, client *kube.Client, namespace string) (map[string][]*unstructured.Unstructured, []string) {
	type fetched struct {
		items []*unstructured.Unstructured
		note  string
	}
	out := make([]fetched, len(tree.ChildResources))
	var wg sync.WaitGroup
	for i, cr := range tree.ChildResources {
		wg.Add(1)
		go func(i int, cr tree.ChildResource) {
			defer wg.Done()
			res, err := client.Resolve(cr.Resource)
			if err != nil {
				if cr.Optional && meta.IsNoMatchError(err) {
					return
				}
				out[i] = fetched{note: fmt.Sprintf("%s skipped: %v", cr.Kind, err)}
				return
			}
			items, err := client.List(ctx, res, namespace)
			if err != nil {
				out[i] = fetched{note: fmt.Sprintf("%s skipped: %v", cr.Kind, err)}
				return
			}
			out[i] = fetched{items: items}
		}(i, cr)
	}
	wg.Wait()

	children := make(map[string][]*unstructured.Unstructured, len(tree.ChildResources))
	var notes []string
	for i, cr := range tree.ChildResources {
		if out[i].note != "" {
			notes = append(notes, out[i].note)
			continue
		}
		children[cr.Kind] = out[i].items
	}
	return children, notes
}

// dedupe drops empty and repeated values while keeping first-seen order.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
