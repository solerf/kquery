// Package tree groups the listed root objects with the namespace objects that
// relate to them through real Kubernetes links (label selectors, owner
// references, scaleTargetRef, Ingress backends, a shared grouping label) and
// collects everything that matched no root in an unattached bucket.
package tree

import (
	"encoding/json"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// Kinds the tree view knows about.
const (
	KindService      = "Service"
	KindIngress      = "Ingress"
	KindPod          = "Pod"
	KindReplicaSet   = "ReplicaSet"
	KindJob          = "Job"
	KindHPA          = "HorizontalPodAutoscaler"
	KindScaledObject = "ScaledObject"
	KindPDB          = "PodDisruptionBudget"
)

// ChildResource is one resource the tree view fetches per ctx/namespace.
type ChildResource struct {
	Kind     string // display Kind and key into Input.Children
	Resource string // kubectl-style argument resolved through discovery
	Optional bool   // skip silently when the API has no such resource (CRD absent)
}

// ChildResources are listed once per ctx/namespace. ReplicaSets are fetched
// only to follow Deployment -> ReplicaSet -> Pod owner chains; they are never
// shown as a level.
var ChildResources = []ChildResource{
	{Kind: KindService, Resource: "services"},
	{Kind: KindIngress, Resource: "ingresses.networking.k8s.io"},
	{Kind: KindPod, Resource: "pods"},
	{Kind: KindReplicaSet, Resource: "replicasets.apps"},
	{Kind: KindJob, Resource: "jobs.batch"},
	{Kind: KindHPA, Resource: "horizontalpodautoscalers.autoscaling"},
	{Kind: KindScaledObject, Resource: "scaledobjects.keda.sh", Optional: true},
	{Kind: KindPDB, Resource: "poddisruptionbudgets.policy"},
}

// Input is everything listed for one ctx/namespace.
type Input struct {
	RootKind   string
	Roots      []*unstructured.Unstructured
	Children   map[string][]*unstructured.Unstructured // keyed by Kind
	GroupLabel string                                  // label linking Jobs to roots by equal value
}

// Node is one line of the tree.
type Node struct {
	Kind     string
	Obj      *unstructured.Unstructured
	Children []*Node
}

// Result is the built tree: roots sorted by name, then the unattached bucket
// in fixed kind order (Service > Ingress > Pod > Job > HPA > ScaledObject > PDB).
type Result struct {
	Roots      []*Node
	Unattached []*Node
}

type builder struct {
	in        Input
	byUID     map[types.UID]*unstructured.Unstructured
	ingByServ map[string][]*unstructured.Unstructured    // Service name -> Ingresses referencing it
	jobPods   map[types.UID][]*unstructured.Unstructured // Job UID -> Pods it owns
	jobOwned  map[*unstructured.Unstructured]bool        // Pods owned by any Job
	attached  map[*unstructured.Unstructured]bool        // children placed under a root
	placedIng map[*unstructured.Unstructured]bool        // Ingresses emitted under some Service node
}

// Build groups in.Children under in.Roots and returns the leftovers.
func Build(in Input) Result {
	b := &builder{
		in:        in,
		byUID:     map[types.UID]*unstructured.Unstructured{},
		ingByServ: map[string][]*unstructured.Unstructured{},
		jobPods:   map[types.UID][]*unstructured.Unstructured{},
		jobOwned:  map[*unstructured.Unstructured]bool{},
		attached:  map[*unstructured.Unstructured]bool{},
		placedIng: map[*unstructured.Unstructured]bool{},
	}

	for _, r := range in.Roots {
		b.byUID[r.GetUID()] = r
	}
	for kind, objs := range in.Children {
		if kind == in.RootKind {
			continue
		}
		for _, o := range objs {
			b.byUID[o.GetUID()] = o
		}
	}
	for _, ing := range b.kids(KindIngress) {
		for _, svc := range ingressBackends(ing) {
			b.ingByServ[svc] = append(b.ingByServ[svc], ing)
		}
	}
	jobs := b.kids(KindJob)
	for _, pod := range b.kids(KindPod) {
		for _, job := range jobs {
			if ownedBy(pod, job.GetUID(), b.byUID, 0) {
				b.jobPods[job.GetUID()] = append(b.jobPods[job.GetUID()], pod)
				b.jobOwned[pod] = true
				break
			}
		}
	}

	var res Result
	for _, root := range sortedByName(in.Roots) {
		res.Roots = append(res.Roots, b.rootNode(root))
	}
	res.Unattached = b.unattached()
	return res
}

// kids returns the listed objects of kind sorted by name. The root kind is
// never a child kind: the roots already are the complete set of it.
func (b *builder) kids(kind string) []*unstructured.Unstructured {
	if kind == b.in.RootKind {
		return nil
	}
	return sortedByName(b.in.Children[kind])
}

func (b *builder) rootNode(root *unstructured.Unstructured) *Node {
	n := &Node{Kind: b.in.RootKind, Obj: root}
	uid, name := root.GetUID(), root.GetName()
	podLabels := rootPodLabels(root)
	ownerBased := hasPodTemplate(root)

	// Service: selector is a non-empty subset of the root's pod labels. A root
	// that is itself a Service carries its Ingresses directly.
	for _, svc := range b.kids(KindService) {
		if selectorMatches(stringMap(svc, "spec", "selector"), podLabels) {
			n.Children = append(n.Children, b.serviceNode(svc))
			b.attached[svc] = true
		}
	}
	if b.in.RootKind == KindService {
		n.Children = append(n.Children, b.ingressNodes(root)...)
	}

	// Pod: owner chain for template-bearing roots, selector otherwise. Pods
	// owned by a Job always travel with their Job.
	for _, pod := range b.kids(KindPod) {
		if b.jobOwned[pod] {
			continue
		}
		var match bool
		if ownerBased {
			match = ownedBy(pod, uid, b.byUID, 0)
		} else {
			match = selectorMatches(podLabels, pod.GetLabels())
		}
		if match {
			n.Children = append(n.Children, &Node{Kind: KindPod, Obj: pod})
			b.attached[pod] = true
		}
	}

	// Job: equal grouping label (Jobs have no selector/owner link to a
	// Deployment), or owned by the root (CronJob roots).
	group := ""
	if b.in.GroupLabel != "" {
		group = root.GetLabels()[b.in.GroupLabel]
	}
	for _, job := range b.kids(KindJob) {
		if (group != "" && job.GetLabels()[b.in.GroupLabel] == group) || ownedBy(job, uid, b.byUID, 0) {
			n.Children = append(n.Children, b.jobNode(job))
			b.attached[job] = true
		}
	}

	// HPA / ScaledObject: scaleTargetRef names the root (kind must agree when set).
	for _, kind := range []string{KindHPA, KindScaledObject} {
		for _, o := range b.kids(kind) {
			if targetsRoot(o, name, b.in.RootKind) {
				n.Children = append(n.Children, &Node{Kind: kind, Obj: o})
				b.attached[o] = true
			}
		}
	}

	// PDB: matchLabels is a non-empty subset of the root's pod labels.
	for _, pdb := range b.kids(KindPDB) {
		if selectorMatches(stringMap(pdb, "spec", "selector", "matchLabels"), podLabels) {
			n.Children = append(n.Children, &Node{Kind: KindPDB, Obj: pdb})
			b.attached[pdb] = true
		}
	}
	return n
}

func (b *builder) serviceNode(svc *unstructured.Unstructured) *Node {
	return &Node{Kind: KindService, Obj: svc, Children: b.ingressNodes(svc)}
}

func (b *builder) ingressNodes(svc *unstructured.Unstructured) []*Node {
	var out []*Node
	for _, ing := range b.ingByServ[svc.GetName()] {
		out = append(out, &Node{Kind: KindIngress, Obj: ing})
		b.placedIng[ing] = true
	}
	return out
}

func (b *builder) jobNode(job *unstructured.Unstructured) *Node {
	n := &Node{Kind: KindJob, Obj: job}
	for _, pod := range b.jobPods[job.GetUID()] {
		n.Children = append(n.Children, &Node{Kind: KindPod, Obj: pod})
	}
	return n
}

// unattached collects every child that matched no root. Services go first so
// their Ingresses nest under them; only Ingresses whose Service does not exist
// surface at the top level.
func (b *builder) unattached() []*Node {
	var out []*Node
	for _, svc := range b.kids(KindService) {
		if !b.attached[svc] {
			out = append(out, b.serviceNode(svc))
		}
	}
	for _, ing := range b.kids(KindIngress) {
		if !b.placedIng[ing] {
			out = append(out, &Node{Kind: KindIngress, Obj: ing})
		}
	}
	for _, pod := range b.kids(KindPod) {
		if !b.attached[pod] && !b.jobOwned[pod] {
			out = append(out, &Node{Kind: KindPod, Obj: pod})
		}
	}
	for _, job := range b.kids(KindJob) {
		if !b.attached[job] {
			out = append(out, b.jobNode(job))
		}
	}
	for _, kind := range []string{KindHPA, KindScaledObject, KindPDB} {
		for _, o := range b.kids(kind) {
			if !b.attached[o] {
				out = append(out, &Node{Kind: kind, Obj: o})
			}
		}
	}
	return out
}

// --- relationship helpers ---

// ownedBy reports whether obj's ownerReference chain reaches target, walking
// through intermediates that were listed (ReplicaSets, Jobs).
func ownedBy(obj *unstructured.Unstructured, target types.UID, byUID map[types.UID]*unstructured.Unstructured, depth int) bool {
	if depth > 8 {
		return false
	}
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == target {
			return true
		}
		if owner, ok := byUID[ref.UID]; ok && ownedBy(owner, target, byUID, depth+1) {
			return true
		}
	}
	return false
}

// selectorMatches is true when sel is non-empty and every key/value of sel is
// present in labels.
func selectorMatches(sel, labels map[string]string) bool {
	if len(sel) == 0 {
		return false
	}
	for k, v := range sel {
		if lv, ok := labels[k]; !ok || lv != v {
			return false
		}
	}
	return true
}

// rootPodLabels returns the labels a root's pods carry: the pod template's
// labels, else a matchLabels selector, else a flat selector (Service), else
// the object's own labels when the root is a Pod. Nil means "no pod relation".
func rootPodLabels(o *unstructured.Unstructured) map[string]string {
	for _, path := range [][]string{
		{"spec", "template", "metadata", "labels"},
		{"spec", "jobTemplate", "spec", "template", "metadata", "labels"},
		{"spec", "selector", "matchLabels"},
		{"spec", "selector"},
	} {
		if m := stringMap(o, path...); len(m) > 0 {
			return m
		}
	}
	if o.GetKind() == KindPod {
		return o.GetLabels()
	}
	return nil
}

func hasPodTemplate(o *unstructured.Unstructured) bool {
	if _, found, _ := unstructured.NestedMap(o.Object, "spec", "template"); found {
		return true
	}
	_, found, _ := unstructured.NestedMap(o.Object, "spec", "jobTemplate")
	return found
}

func targetsRoot(o *unstructured.Unstructured, name, kind string) bool {
	ref := stringMap(o, "spec", "scaleTargetRef")
	return ref["name"] == name && (ref["kind"] == "" || strings.EqualFold(ref["kind"], kind))
}

// albActionPrefix marks AWS Load Balancer Controller action annotations. A
// path whose backend port is named "use-annotation" does not point at a
// Service; the Services are the targetGroups of the action with that name.
// This is how Ingresses that drive weighted target groups (e.g. a canary split) route.
const albActionPrefix = "alb.ingress.kubernetes.io/actions."

// ingressBackends returns the Service names an Ingress routes to: the
// defaultBackend and every rule path (legacy serviceName form included), with
// ALB action backends resolved through their annotation.
func ingressBackends(ing *unstructured.Unstructured) []string {
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	annotations := ing.GetAnnotations()
	addBackend := func(backend map[string]interface{}) {
		if backend == nil {
			return
		}
		name, _, _ := unstructured.NestedString(backend, "service", "name")
		portName, _, _ := unstructured.NestedString(backend, "service", "port", "name")
		if portName == "use-annotation" {
			for _, svc := range albActionServices(annotations[albActionPrefix+name]) {
				add(svc)
			}
			return
		}
		add(name)
		legacy, _, _ := unstructured.NestedString(backend, "serviceName")
		add(legacy)
	}

	defaultBackend, _, _ := unstructured.NestedMap(ing.Object, "spec", "defaultBackend")
	addBackend(defaultBackend)
	legacyBackend, _, _ := unstructured.NestedMap(ing.Object, "spec", "backend")
	addBackend(legacyBackend)
	rules, _, _ := unstructured.NestedSlice(ing.Object, "spec", "rules")
	for _, r := range rules {
		rm, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		paths, _, _ := unstructured.NestedSlice(rm, "http", "paths")
		for _, p := range paths {
			pm, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			backend, _, _ := unstructured.NestedMap(pm, "backend")
			addBackend(backend)
		}
	}
	return names
}

// albActionServices extracts forwardConfig.targetGroups[].serviceName from an
// ALB action annotation value; anything unparsable yields nothing.
func albActionServices(raw string) []string {
	if raw == "" {
		return nil
	}
	var action struct {
		ForwardConfig struct {
			TargetGroups []struct {
				ServiceName string `json:"serviceName"`
			} `json:"targetGroups"`
		} `json:"forwardConfig"`
	}
	if err := json.Unmarshal([]byte(raw), &action); err != nil {
		return nil
	}
	var out []string
	for _, tg := range action.ForwardConfig.TargetGroups {
		if tg.ServiceName != "" {
			out = append(out, tg.ServiceName)
		}
	}
	return out
}

func stringMap(o *unstructured.Unstructured, fields ...string) map[string]string {
	m, found, err := unstructured.NestedStringMap(o.Object, fields...)
	if !found || err != nil {
		return nil
	}
	return m
}

func sortedByName(objs []*unstructured.Unstructured) []*unstructured.Unstructured {
	out := append([]*unstructured.Unstructured(nil), objs...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out
}
