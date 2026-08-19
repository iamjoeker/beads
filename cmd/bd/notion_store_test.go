//go:build cgo

package main

import (
	"context"
	"path/filepath"
	"testing"
)

func TestGetNotionConfigReadsDBPathWhenStoreUnset(t *testing.T) {
	saveAndRestoreGlobals(t)
	tempDir := t.TempDir()
	testDBPath := filepath.Join(tempDir, "test.db")
	// Isolated database, not the branch-per-test shared one: getNotionConfig
	// below reopens the store from dbPath through metadata.json, which lands
	// on the shared database's main branch where the SetConfig calls above
	// are not visible (bd-2k4).
	testStore := newTestStoreIsolatedDB(t, testDBPath, "test")
	defer testStore.Close()

	ctx := context.Background()
	if err := testStore.SetConfig(ctx, "notion.token", "path-token"); err != nil {
		t.Fatalf("SetConfig(notion.token): %v", err)
	}
	if err := testStore.SetConfig(ctx, "notion.data_source_id", "path-ds"); err != nil {
		t.Fatalf("SetConfig(notion.data_source_id): %v", err)
	}

	store = nil
	dbPath = testDBPath
	t.Setenv("NOTION_TOKEN", "")
	t.Setenv("NOTION_DATA_SOURCE_ID", "")
	t.Setenv("NOTION_VIEW_URL", "")

	cfg := getNotionConfig()
	if cfg.DataSourceID != "path-ds" {
		t.Fatalf("config = %+v", cfg)
	}
}
