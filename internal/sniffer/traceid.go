package sniffer

import (
	"bytes"
	"errors"
	"fmt"
	"text/template"
)

// TraceID composes the deterministic identifier per spec Section 2:
//
//	trace_id = "<source_table>-<pk>"
//
// Same source row always yields same trace_id; idempotency relies on this.
func TraceID(sourceTable, pk string) string {
	return sourceTable + "-" + pk
}

// DstTemplate renders the destination path for a source row. The exact
// pattern mirrors NiFi's 1:1 mapping (verified Week 4 — see spec OQ1).
// Shadow=true appends ".imgsync_shadow_v1" to avoid colliding with NiFi
// production output (operational safety, NOT for cross-system reconcile).
type DstTemplate struct {
	Pattern string // text/template body, fields = source row columns
	Shadow  bool
}

const ShadowSuffix = ".imgsync_shadow_v1"

func (t DstTemplate) Render(fields map[string]string) (string, error) {
	if t.Pattern == "" {
		return "", errors.New("dst template: empty pattern")
	}
	tmpl, err := template.New("dst").Option("missingkey=error").Parse(t.Pattern)
	if err != nil {
		return "", fmt.Errorf("parse pattern: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, fields); err != nil {
		return "", fmt.Errorf("render: %w", err)
	}
	out := buf.String()
	if t.Shadow {
		out += ShadowSuffix
	}
	return out, nil
}
