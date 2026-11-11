// Package printer renders a header plus rows as CSV or as an aligned table.
package printer

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Table is the output: one header and zero or more rows of equal width.
type Table struct {
	Header []string
	Rows   [][]string
}

// Write renders t in the named format ("csv" or "table").
func Write(w io.Writer, format string, t Table) error {
	switch format {
	case "csv":
		return WriteCSV(w, t)
	case "table":
		return WriteTable(w, t)
	default:
		return fmt.Errorf("unknown format %q", format)
	}
}

// WriteCSV writes RFC 4180 CSV; cells containing commas, quotes or newlines
// are quoted by encoding/csv.
func WriteCSV(w io.Writer, t Table) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(t.Header); err != nil {
		return err
	}
	for _, r := range t.Rows {
		if err := cw.Write(r); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteTable writes columns aligned with text/tabwriter. Tabs and newlines
// inside a cell would break the alignment, so they are replaced by spaces.
func WriteTable(w io.Writer, t Table) error {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	writeRow(tw, t.Header)
	for _, r := range t.Rows {
		writeRow(tw, r)
	}
	return tw.Flush()
}

var cellSanitizer = strings.NewReplacer("\t", " ", "\r\n", " ", "\n", " ", "\r", " ")

func writeRow(w io.Writer, cells []string) {
	clean := make([]string, len(cells))
	for i, c := range cells {
		clean[i] = cellSanitizer.Replace(c)
	}
	fmt.Fprintln(w, strings.Join(clean, "\t"))
}

// TreeLine is one line of the tree view: its depth, its label (a Kind header,
// an object name or "(unattached)") and, for object lines, the extra-column
// values resolved on that object. Header lines leave Cells empty.
type TreeLine struct {
	Depth int
	Label string
	Cells []string
}

// TreeBlock is one ctx/namespace block of the tree view.
type TreeBlock struct {
	Title  string   // "ctx/namespace"
	Header []string // extra column headers; empty when no --extra was given
	Lines  []TreeLine
}

// WriteTree prints each block as its title line, an optional header row and
// the indented lines (two spaces per level), columns aligned per block. Lines
// with fewer cells than the header (Kind headers, "(unattached)") are padded
// so tabwriter keeps one column block; trailing spaces are then trimmed.
func WriteTree(w io.Writer, blocks []TreeBlock) error {
	for i, b := range blocks {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, b.Title)
		var buf bytes.Buffer
		tw := tabwriter.NewWriter(&buf, 0, 8, 2, ' ', 0)
		if len(b.Header) > 0 {
			writeRow(tw, append([]string{"name"}, b.Header...))
		}
		for _, l := range b.Lines {
			cells := make([]string, 0, 1+len(b.Header))
			cells = append(cells, strings.Repeat("  ", l.Depth)+l.Label)
			cells = append(cells, l.Cells...)
			for len(cells) < 1+len(b.Header) {
				cells = append(cells, "")
			}
			writeRow(tw, cells)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		for _, line := range strings.SplitAfter(buf.String(), "\n") {
			if line == "" {
				continue
			}
			if _, err := io.WriteString(w, strings.TrimRight(line, " \n")+"\n"); err != nil {
				return err
			}
		}
	}
	return nil
}
