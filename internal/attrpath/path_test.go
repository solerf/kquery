package attrpath

import "testing"

// deployment mimics the unstructured form of an apps/v1 Deployment as decoded
// by the dynamic client (integers arrive as int64).
var deployment = map[string]interface{}{
	"metadata": map[string]interface{}{
		"name":              "web-deployment",
		"creationTimestamp": "2026-01-01T00:00:00Z",
		"labels": map[string]interface{}{
			"app.kubernetes.io/name": "web",
			"helm.sh/version":        "0.29.0",
		},
		"annotations": map[string]interface{}{
			"example.com/deploy-id": "run-42",
		},
	},
	"spec": map[string]interface{}{
		"progressDeadlineSeconds": int64(600),
		"paused":                  true,
		"ratio":                   1.5,
		"whole":                   float64(3),
		"template": map[string]interface{}{
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{"name": "app", "image": "registry/web:v2"},
					map[string]interface{}{"name": "sidecar", "image": "registry/sidecar:v1"},
				},
			},
		},
		"selector": map[string]interface{}{
			"matchLabels": map[string]interface{}{"app": "web"},
		},
	},
	"status": map[string]interface{}{
		"readyReplicas": int64(3),
		"conditions": []interface{}{
			map[string]interface{}{"type": "Available", "status": "True"},
		},
	},
}

func TestResolve(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		// labels./annotations. keep the whole remainder as the map key
		{"labels.app.kubernetes.io/name", "web"},
		{"labels.helm.sh/version", "0.29.0"},
		{"annotations.example.com/deploy-id", "run-42"},
		{"metadata.labels.app.kubernetes.io/name", "web"},
		{"metadata.annotations.example.com/deploy-id", "run-42"},
		{"labels.missing/key", ""},
		{"labels", `{"app.kubernetes.io/name":"web","helm.sh/version":"0.29.0"}`},
		// nested scalar fields
		{"metadata.name", "web-deployment"},
		{"metadata.creationTimestamp", "2026-01-01T00:00:00Z"},
		{"spec.progressDeadlineSeconds", "600"},
		{"status.readyReplicas", "3"},
		{"spec.paused", "true"},
		{"spec.ratio", "1.5"},
		{"spec.whole", "3"},
		// list index
		{"spec.template.spec.containers[0].image", "registry/web:v2"},
		{"spec.template.spec.containers[1].name", "sidecar"},
		{"status.conditions[0].type", "Available"},
		{"spec.template.spec.containers[5].image", ""},
		// missing / wrong shape
		{"spec.nope", ""},
		{"spec.nope.deeper", ""},
		{"metadata.name.further", ""},
		{"spec.progressDeadlineSeconds[0]", ""},
		{"spec.template.spec.containers.image", ""},
		// containers render as compact JSON
		{"spec.selector", `{"matchLabels":{"app":"web"}}`},
		{"status.conditions", `[{"status":"True","type":"Available"}]`},
		{"spec.template.spec.containers[1]", `{"image":"registry/sidecar:v1","name":"sidecar"}`},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			p, err := Parse(c.path)
			if err != nil {
				t.Fatalf("Parse(%q): %v", c.path, err)
			}
			if p.String() != c.path {
				t.Errorf("String() = %q, want the path as typed %q", p.String(), c.path)
			}
			if got := p.Resolve(deployment); got != c.want {
				t.Errorf("Resolve(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}

func TestResolveOnEmptyObject(t *testing.T) {
	p, err := Parse("labels.app.kubernetes.io/name")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Resolve(map[string]interface{}{}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if got := p.Resolve(nil); got != "" {
		t.Errorf("nil object: got %q, want empty", got)
	}
}

func TestParseErrors(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"labels.",
		"annotations.",
		"metadata.labels.",
		"spec.",
		".spec",
		"spec..x",
		"[0]",
		"spec.containers[x]",
		"spec.containers[-1]",
		"spec.containers[0",
		"spec.containers[0]x",
	}
	for _, raw := range bad {
		if _, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q): expected an error", raw)
		}
	}
}

func TestFormat(t *testing.T) {
	cases := []struct {
		in   interface{}
		want string
	}{
		{nil, ""},
		{"x", "x"},
		{true, "true"},
		{int64(42), "42"},
		{7, "7"},
		{2.25, "2.25"},
		{float64(10), "10"},
		{[]interface{}{"a", int64(1)}, `["a",1]`},
		{map[string]interface{}{"b": nil, "a": "z"}, `{"a":"z","b":null}`},
	}
	for _, c := range cases {
		if got := Format(c.in); got != c.want {
			t.Errorf("Format(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}
