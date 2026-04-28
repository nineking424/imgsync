package cli_test

import (
	"testing"
	"time"

	"github.com/nineking424/imgsync/internal/cli"
)

func TestParseConfig_DefaultsApplied(t *testing.T) {
	t.Setenv("SNIFFER_SOURCE_ID", "main.images")
	t.Setenv("SNIFFER_SOURCE_DSN", "postgres://x/x")
	t.Setenv("SNIFFER_IMGSYNC_DSN", "postgres://y/y")
	t.Setenv("SNIFFER_TABLE", "images")
	t.Setenv("SNIFFER_PK_COLUMN", "id")
	t.Setenv("SNIFFER_TS_COLUMN", "updated_at")
	t.Setenv("SNIFFER_DST_PATTERN", "/in/{{.file_path}}")
	t.Setenv("SNIFFER_SRC_PATTERN", "src://images/{{.id}}")
	t.Setenv("SNIFFER_SRC_PROTOCOL", "fs")
	t.Setenv("SNIFFER_DST_PROTOCOL", "fs")

	cfg, err := cli.ParseSnifferConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IntervalSec != 60 {
		t.Errorf("default interval=%d", cfg.IntervalSec)
	}
	if cfg.BatchSize != 500 {
		t.Errorf("default batch=%d", cfg.BatchSize)
	}
	if cfg.BiasDuration != 5*time.Second {
		t.Errorf("default bias=%v", cfg.BiasDuration)
	}
	if cfg.Shadow != true {
		t.Errorf("default shadow should be true")
	}
}

func TestParseConfig_RequiredMissing(t *testing.T) {
	t.Setenv("SNIFFER_SOURCE_ID", "")
	_, err := cli.ParseSnifferConfig()
	if err == nil {
		t.Fatal("expected error")
	}
}

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SNIFFER_SOURCE_ID", "main.images")
	t.Setenv("SNIFFER_SOURCE_DSN", "postgres://x/x")
	t.Setenv("SNIFFER_IMGSYNC_DSN", "postgres://y/y")
	t.Setenv("SNIFFER_TABLE", "images")
	t.Setenv("SNIFFER_PK_COLUMN", "id")
	t.Setenv("SNIFFER_TS_COLUMN", "updated_at")
	t.Setenv("SNIFFER_DST_PATTERN", "/in/{{.file_path}}")
	t.Setenv("SNIFFER_SRC_PATTERN", "src://images/{{.id}}")
	t.Setenv("SNIFFER_SRC_PROTOCOL", "fs")
	t.Setenv("SNIFFER_DST_PROTOCOL", "fs")
}

func TestParseConfig_IntervalZeroRejected(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SNIFFER_INTERVAL_SEC", "0")
	_, err := cli.ParseSnifferConfig()
	if err == nil {
		t.Fatal("SNIFFER_INTERVAL_SEC=0 must be rejected (would panic time.NewTicker)")
	}
}

func TestParseConfig_ExtraColumnsTrimsEmpty(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SNIFFER_EXTRA_COLUMNS", " file_path , , category ,")
	cfg, err := cli.ParseSnifferConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ExtraColumns) != 2 || cfg.ExtraColumns[0] != "file_path" || cfg.ExtraColumns[1] != "category" {
		t.Fatalf("expected [file_path category], got %v", cfg.ExtraColumns)
	}
}
