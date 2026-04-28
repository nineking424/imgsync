package sniffer_test

import (
	"testing"

	"github.com/nineking424/imgsync/internal/sniffer"
)

func TestTraceID(t *testing.T) {
	tests := []struct {
		table string
		pk    string
		want  string
	}{
		{"images", "12345", "images-12345"},
		{"documents", "uuid-abc-def", "documents-uuid-abc-def"},
		{"main-db.images", "1", "main-db.images-1"},
	}
	for _, tt := range tests {
		got := sniffer.TraceID(tt.table, tt.pk)
		if got != tt.want {
			t.Errorf("TraceID(%q,%q)=%q want %q", tt.table, tt.pk, got, tt.want)
		}
	}
}

func TestDstPath_ShadowSuffixApplied(t *testing.T) {
	tmpl := sniffer.DstTemplate{
		Pattern: "/incoming/{{.FilePath}}",
		Shadow:  true,
	}
	got, err := tmpl.Render(map[string]string{"FilePath": "2026/04/img.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	want := "/incoming/2026/04/img.jpg" + sniffer.ShadowSuffix
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestDstPath_ShadowOff(t *testing.T) {
	tmpl := sniffer.DstTemplate{
		Pattern: "/incoming/{{.FilePath}}",
		Shadow:  false,
	}
	got, _ := tmpl.Render(map[string]string{"FilePath": "a/b.jpg"})
	if got != "/incoming/a/b.jpg" {
		t.Errorf("got %q", got)
	}
}

func TestDstPath_MissingKey(t *testing.T) {
	tmpl := sniffer.DstTemplate{Pattern: "/x/{{.Missing}}"}
	_, err := tmpl.Render(map[string]string{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDstPath_EmptyPatternRejected(t *testing.T) {
	tmpl := sniffer.DstTemplate{Pattern: ""}
	_, err := tmpl.Render(map[string]string{"FilePath": "x"})
	if err == nil {
		t.Fatal("expected error for empty pattern")
	}
}
