package printer

import (
	"bytes"
	"strings"
	"testing"
)

var sample = Table{
	Header: []string{"ctx", "namespace", "name", "labels.app.kubernetes.io/name"},
	Rows: [][]string{
		{"ctx_1", "nms_1", "obj-a", "value"},
		{"ctx_1", "nms_1", "obj-b", `has,comma and "quotes"`},
		{"ctx_2", "nms_1", "obj-c", ""},
	},
}

func TestWriteCSV(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCSV(&buf, sample); err != nil {
		t.Fatal(err)
	}
	want := "ctx,namespace,name,labels.app.kubernetes.io/name\n" +
		"ctx_1,nms_1,obj-a,value\n" +
		"ctx_1,nms_1,obj-b,\"has,comma and \"\"quotes\"\"\"\n" +
		"ctx_2,nms_1,obj-c,\n"
	if buf.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestWriteTable(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTable(&buf, sample); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 lines, got %d:\n%s", len(lines), buf.String())
	}
	// every data row starts its name column at the same offset as the header
	// ("name" also occurs inside "namespace", so look past that token first)
	after := strings.Index(lines[0], "namespace") + len("namespace")
	col := after + strings.Index(lines[0][after:], "name")
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l[col:], "obj-") {
			t.Errorf("misaligned row: %q", l)
		}
	}
}

func TestWriteTableSanitizesCells(t *testing.T) {
	var buf bytes.Buffer
	tbl := Table{Header: []string{"a", "b"}, Rows: [][]string{{"x\ty", "line1\nline2"}}}
	if err := WriteTable(&buf, tbl); err != nil {
		t.Fatal(err)
	}
	if strings.Count(buf.String(), "\n") != 2 {
		t.Errorf("embedded newline leaked into output:\n%q", buf.String())
	}
}

func TestWriteUnknownFormat(t *testing.T) {
	if err := Write(&bytes.Buffer{}, "yaml", sample); err == nil {
		t.Error("expected an error for an unknown format")
	}
}

func TestWriteTree(t *testing.T) {
	var buf bytes.Buffer
	blocks := []TreeBlock{
		{
			Title:  "ctx-a/ns",
			Header: []string{"status.readyReplicas"},
			Lines: []TreeLine{
				{Depth: 0, Label: "Deployment"}, // Kind header: no cells
				{Depth: 1, Label: "web", Cells: []string{"3"}},
				{Depth: 2, Label: "Service"},
				{Depth: 3, Label: "web-svc", Cells: []string{""}},
				{Depth: 4, Label: "Ingress"},
				{Depth: 5, Label: "web-ingress", Cells: []string{""}},
				{Depth: 0, Label: "(unattached)"},
				{Depth: 1, Label: "Job"},
				{Depth: 2, Label: "old", Cells: []string{""}},
			},
		},
		{Title: "ctx-b/ns", Lines: []TreeLine{{Depth: 0, Label: "Deployment"}, {Depth: 1, Label: "api"}}},
	}
	if err := WriteTree(&buf, blocks); err != nil {
		t.Fatal(err)
	}
	// widest first-column cell is "          web-ingress" (21) + 2 padding = 23;
	// lines without a second value have their trailing padding trimmed
	pad := func(s string) string { return s + strings.Repeat(" ", 23-len(s)) }
	want := strings.Join([]string{
		"ctx-a/ns",
		pad("name") + "status.readyReplicas",
		"Deployment",
		pad("  web") + "3",
		"    Service",
		"      web-svc",
		"        Ingress",
		"          web-ingress",
		"(unattached)",
		"  Job",
		"    old",
		"",
		"ctx-b/ns",
		"Deployment",
		"  api",
		"",
	}, "\n")
	if buf.String() != want {
		t.Errorf("got:\n%q\nwant:\n%q", buf.String(), want)
	}
}
