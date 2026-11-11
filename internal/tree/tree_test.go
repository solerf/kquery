package tree

import (
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// obj builds an unstructured object; extra is merged into the top level
// (spec/status) and labels into metadata.labels.
func obj(kind, name, uid string, labels map[string]string, extra map[string]interface{}) *unstructured.Unstructured {
	meta := map[string]interface{}{"name": name, "uid": uid}
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

func owned(o *unstructured.Unstructured, ownerKind, ownerName, ownerUID string) *unstructured.Unstructured {
	refs, _, _ := unstructured.NestedSlice(o.Object, "metadata", "ownerReferences")
	refs = append(refs, map[string]interface{}{"kind": ownerKind, "name": ownerName, "uid": ownerUID, "apiVersion": "v1"})
	_ = unstructured.SetNestedSlice(o.Object, refs, "metadata", "ownerReferences")
	return o
}

func deployment(name, uid string, podLabels map[string]string) *unstructured.Unstructured {
	tl := map[string]interface{}{}
	for k, v := range podLabels {
		tl[k] = v
	}
	return obj("Deployment", name, uid, podLabels, map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{"metadata": map[string]interface{}{"labels": tl}},
		},
	})
}

func service(name, uid string, selector map[string]string) *unstructured.Unstructured {
	sel := map[string]interface{}{}
	for k, v := range selector {
		sel[k] = v
	}
	return obj("Service", name, uid, nil, map[string]interface{}{"spec": map[string]interface{}{"selector": sel}})
}

func ingress(name, uid string, backends ...string) *unstructured.Unstructured {
	var paths []interface{}
	for _, b := range backends {
		paths = append(paths, map[string]interface{}{"path": "/", "backend": map[string]interface{}{"service": map[string]interface{}{"name": b}}})
	}
	return obj("Ingress", name, uid, nil, map[string]interface{}{
		"spec": map[string]interface{}{"rules": []interface{}{map[string]interface{}{"http": map[string]interface{}{"paths": paths}}}},
	})
}

func scaler(kind, name, uid, targetKind, targetName string) *unstructured.Unstructured {
	ref := map[string]interface{}{"name": targetName}
	if targetKind != "" {
		ref["kind"] = targetKind
	}
	return obj(kind, name, uid, nil, map[string]interface{}{"spec": map[string]interface{}{"scaleTargetRef": ref}})
}

func pdb(name, uid string, matchLabels map[string]string) *unstructured.Unstructured {
	ml := map[string]interface{}{}
	for k, v := range matchLabels {
		ml[k] = v
	}
	return obj("PodDisruptionBudget", name, uid, nil, map[string]interface{}{
		"spec": map[string]interface{}{"selector": map[string]interface{}{"matchLabels": ml}},
	})
}

// render flattens a Result into "indent Kind/name" lines for easy assertions.
func render(r Result) []string {
	var lines []string
	var walk func(n *Node, depth int)
	walk = func(n *Node, depth int) {
		lines = append(lines, strings.Repeat("  ", depth)+n.Kind+"/"+n.Obj.GetName())
		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}
	for _, n := range r.Roots {
		walk(n, 0)
	}
	if len(r.Unattached) > 0 {
		lines = append(lines, "(unattached)")
		for _, n := range r.Unattached {
			walk(n, 1)
		}
	}
	return lines
}

func assertLines(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("tree mismatch\n--- got ---\n%s\n--- want ---\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestDeploymentTree(t *testing.T) {
	app := map[string]string{"app.kubernetes.io/name": "web", "tier": "fe"}
	dep := deployment("web-deployment", "d1", app)
	rs := owned(obj("ReplicaSet", "web-deployment-abc", "rs1", app, nil), "Deployment", "web-deployment", "d1")
	pod := owned(obj("Pod", "web-deployment-abc-xyz", "p1", app, nil), "ReplicaSet", "web-deployment-abc", "rs1")
	strayPod := owned(obj("Pod", "stray-pod", "p2", app, nil), "ReplicaSet", "gone-rs", "rs-gone")

	svc := service("web-svc", "s1", map[string]string{"app.kubernetes.io/name": "web"}) // subset of pod labels
	svcNo := service("other-svc", "s2", map[string]string{"app.kubernetes.io/name": "other"})
	svcEmpty := service("headless-no-selector", "s3", nil) // empty selector never matches
	ing := ingress("web-ingress", "i1", "web-svc")
	ingOther := ingress("other-ingress", "i2", "other-svc")
	ingDangling := ingress("dangling-ingress", "i3", "missing-svc")

	job := obj("Job", "web-e2e-job", "j1", map[string]string{"app.kubernetes.io/name": "web"}, nil)
	jobPod := owned(obj("Pod", "web-e2e-job-k2j", "p3", map[string]string{"job-name": "web-e2e-job"}, nil), "Job", "web-e2e-job", "j1")
	oldJob := obj("Job", "old-job", "j2", map[string]string{"app.kubernetes.io/name": "retired"}, nil)
	oldJobPod := owned(obj("Pod", "old-job-9zz", "p4", nil, nil), "Job", "old-job", "j2")

	hpa := scaler("HorizontalPodAutoscaler", "web-hpa", "h1", "Deployment", "web-deployment")
	hpaKeda := scaler("HorizontalPodAutoscaler", "keda-hpa-web", "h2", "", "web-deployment") // kind unset still matches
	hpaWrongKind := scaler("HorizontalPodAutoscaler", "sts-hpa", "h3", "StatefulSet", "web-deployment")
	so := scaler("ScaledObject", "web-scaledobject", "so1", "Deployment", "web-deployment")
	soOther := scaler("ScaledObject", "other-scaledobject", "so2", "Deployment", "other")

	pdbOK := pdb("web-pdb", "pdb1", map[string]string{"tier": "fe"})
	pdbNo := pdb("other-pdb", "pdb2", map[string]string{"tier": "be"})

	res := Build(Input{
		RootKind:   "Deployment",
		Roots:      []*unstructured.Unstructured{dep},
		GroupLabel: "app.kubernetes.io/name",
		Children: map[string][]*unstructured.Unstructured{
			KindService:      {svcNo, svc, svcEmpty},
			KindIngress:      {ingDangling, ingOther, ing},
			KindPod:          {strayPod, pod, oldJobPod, jobPod},
			KindReplicaSet:   {rs},
			KindJob:          {oldJob, job},
			KindHPA:          {hpaWrongKind, hpaKeda, hpa},
			KindScaledObject: {soOther, so},
			KindPDB:          {pdbNo, pdbOK},
		},
	})

	assertLines(t, render(res), []string{
		"Deployment/web-deployment",
		"  Service/web-svc",
		"    Ingress/web-ingress",
		"  Pod/web-deployment-abc-xyz",
		"  Job/web-e2e-job",
		"    Pod/web-e2e-job-k2j",
		"  HorizontalPodAutoscaler/keda-hpa-web",
		"  HorizontalPodAutoscaler/web-hpa",
		"  ScaledObject/web-scaledobject",
		"  PodDisruptionBudget/web-pdb",
		"(unattached)",
		"  Service/headless-no-selector",
		"  Service/other-svc",
		"    Ingress/other-ingress",
		"  Ingress/dangling-ingress",
		"  Pod/stray-pod",
		"  Job/old-job",
		"    Pod/old-job-9zz",
		"  HorizontalPodAutoscaler/sts-hpa",
		"  ScaledObject/other-scaledobject",
		"  PodDisruptionBudget/other-pdb",
	})
}

func TestRootsSortedAndNoRootsStillYieldsUnattached(t *testing.T) {
	b := deployment("b-deployment", "d2", map[string]string{"app": "b"})
	a := deployment("a-deployment", "d1", map[string]string{"app": "a"})
	res := Build(Input{RootKind: "Deployment", Roots: []*unstructured.Unstructured{b, a}})
	assertLines(t, render(res), []string{"Deployment/a-deployment", "Deployment/b-deployment"})

	// the leftover-hunting case: the Deployment is gone, its Service+Ingress+PDB remain
	res = Build(Input{
		RootKind: "Deployment",
		Children: map[string][]*unstructured.Unstructured{
			KindService: {service("gone-svc", "s1", map[string]string{"app": "gone"})},
			KindIngress: {ingress("gone-ingress", "i1", "gone-svc")},
			KindPDB:     {pdb("gone-pdb", "pdb1", map[string]string{"app": "gone"})},
		},
	})
	assertLines(t, render(res), []string{
		"(unattached)",
		"  Service/gone-svc",
		"    Ingress/gone-ingress",
		"  PodDisruptionBudget/gone-pdb",
	})
}

func TestServiceRootUsesSelectorForPodsAndCarriesIngress(t *testing.T) {
	svc := service("web-svc", "s1", map[string]string{"app": "web"})
	podIn := obj("Pod", "web-1", "p1", map[string]string{"app": "web", "x": "y"}, nil)
	podOut := obj("Pod", "db-1", "p2", map[string]string{"app": "db"}, nil)
	ing := ingress("web-ingress", "i1", "web-svc")
	other := service("other-svc", "s2", map[string]string{"app": "other"}) // same kind as root: never a child

	res := Build(Input{
		RootKind: "Service",
		Roots:    []*unstructured.Unstructured{svc},
		Children: map[string][]*unstructured.Unstructured{
			KindService: {other},
			KindPod:     {podOut, podIn},
			KindIngress: {ing},
		},
	})
	assertLines(t, render(res), []string{
		"Service/web-svc",
		"  Ingress/web-ingress",
		"  Pod/web-1",
		"(unattached)",
		"  Pod/db-1",
	})
}

func TestOwnerChainDepthAndSharedLabelMultiAttach(t *testing.T) {
	// a Job shares the group label with two roots: it is shown under both, not unattached
	labels := map[string]string{"app.kubernetes.io/name": "shared"}
	d1 := deployment("one", "d1", map[string]string{"app.kubernetes.io/name": "shared", "role": "a"})
	d2 := deployment("two", "d2", map[string]string{"app.kubernetes.io/name": "shared", "role": "b"})
	job := obj("Job", "shared-job", "j1", labels, nil)
	res := Build(Input{
		RootKind: "Deployment", Roots: []*unstructured.Unstructured{d1, d2}, GroupLabel: "app.kubernetes.io/name",
		Children: map[string][]*unstructured.Unstructured{KindJob: {job}},
	})
	assertLines(t, render(res), []string{"Deployment/one", "  Job/shared-job", "Deployment/two", "  Job/shared-job"})

	// owner chain through an unknown intermediate stops; through a known one continues
	rs := owned(obj("ReplicaSet", "rs", "rs1", nil, nil), "Deployment", "dep", "d1")
	pod := owned(obj("Pod", "pod", "p1", nil, nil), "ReplicaSet", "rs", "rs1")
	if !ownedBy(pod, "d1", toUIDMap(map[string]*unstructured.Unstructured{"rs1": rs}), 0) {
		t.Error("pod -> rs -> deployment chain should resolve")
	}
	if ownedBy(pod, "d1", nil, 0) {
		t.Error("chain must stop when the intermediate ReplicaSet was not listed")
	}
}

func TestGroupLabelDisabled(t *testing.T) {
	dep := deployment("web", "d1", map[string]string{"app.kubernetes.io/name": "web"})
	job := obj("Job", "web-job", "j1", map[string]string{"app.kubernetes.io/name": "web"}, nil)
	res := Build(Input{RootKind: "Deployment", Roots: []*unstructured.Unstructured{dep}, GroupLabel: "",
		Children: map[string][]*unstructured.Unstructured{KindJob: {job}}})
	assertLines(t, render(res), []string{"Deployment/web", "(unattached)", "  Job/web-job"})
}

func TestIngressBackendsCoverAllForms(t *testing.T) {
	ing := obj("Ingress", "i", "i1", nil, map[string]interface{}{
		"spec": map[string]interface{}{
			"defaultBackend": map[string]interface{}{"service": map[string]interface{}{"name": "default-svc"}},
			"rules": []interface{}{
				map[string]interface{}{"http": map[string]interface{}{"paths": []interface{}{
					map[string]interface{}{"backend": map[string]interface{}{"service": map[string]interface{}{"name": "a-svc"}}},
					map[string]interface{}{"backend": map[string]interface{}{"serviceName": "legacy-svc"}},
					map[string]interface{}{"backend": map[string]interface{}{"service": map[string]interface{}{"name": "a-svc"}}},
				}}},
			},
		},
	})
	got := fmt.Sprint(ingressBackends(ing))
	if got != "[default-svc a-svc legacy-svc]" {
		t.Errorf("ingressBackends = %s", got)
	}
}

func TestIngressBackendsResolveALBActionAnnotation(t *testing.T) {
	// AWS Load Balancer Controller shape: backend "forward-targets" with port name use-annotation,
	// real Services in the action annotation (here mid-canary: main + canary + baseline)
	ing := obj("Ingress", "alb-ingress", "i1", nil, map[string]interface{}{
		"spec": map[string]interface{}{
			"rules": []interface{}{
				map[string]interface{}{"http": map[string]interface{}{"paths": []interface{}{
					map[string]interface{}{"path": "/", "backend": map[string]interface{}{"service": map[string]interface{}{
						"name": "forward-targets", "port": map[string]interface{}{"name": "use-annotation"},
					}}},
				}}},
			},
		},
	})
	ing.SetAnnotations(map[string]string{
		"alb.ingress.kubernetes.io/actions.forward-targets": `{"type":"forward","forwardConfig":{"targetGroups":[` +
			`{"serviceName":"web-svc","servicePort":"80","weight":90},` +
			`{"serviceName":"web-canary-svc","servicePort":"80","weight":5},` +
			`{"serviceName":"web-baseline-svc","servicePort":"80","weight":5}],"TargetGroupStickinessConfig":{"Enabled":false}}}`,
	})
	if got := fmt.Sprint(ingressBackends(ing)); got != "[web-svc web-canary-svc web-baseline-svc]" {
		t.Errorf("ingressBackends = %s", got)
	}

	// the Ingress nests under the attached main Service and under the (unattached) pool Services
	dep := deployment("web", "d1", map[string]string{"app": "web"})
	main := service("web-svc", "s1", map[string]string{"app": "web"})
	canary := service("web-canary-svc", "s2", map[string]string{"app": "web-canary"})
	res := Build(Input{RootKind: "Deployment", Roots: []*unstructured.Unstructured{dep},
		Children: map[string][]*unstructured.Unstructured{KindService: {canary, main}, KindIngress: {ing}}})
	assertLines(t, render(res), []string{
		"Deployment/web",
		"  Service/web-svc",
		"    Ingress/alb-ingress",
		"(unattached)",
		"  Service/web-canary-svc",
		"    Ingress/alb-ingress",
	})

	// a use-annotation backend whose annotation is missing or malformed resolves to nothing
	ing.SetAnnotations(map[string]string{"alb.ingress.kubernetes.io/actions.forward-targets": "{not json"})
	if got := ingressBackends(ing); len(got) != 0 {
		t.Errorf("malformed annotation should yield no backends, got %v", got)
	}
}
