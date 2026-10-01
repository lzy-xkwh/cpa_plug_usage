package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveProviderSnapshotSkipsInvalidAndSameDayDuplicates(t *testing.T) {
	closeUsageStore()
	oldConfig := currentConfig()
	defer func() {
		closeUsageStore()
		configMu.Lock()
		runtimeConfig = oldConfig
		configMu.Unlock()
	}()

	cfg := oldConfig
	cfg.Enabled = false
	cfg.UsageDBPath = filepath.Join(t.TempDir(), "usage.db")
	cfg.UsageRetentionDays = 400
	configMu.Lock()
	runtimeConfig = cfg
	configMu.Unlock()

	at := time.Date(2026, time.January, 10, 9, 0, 0, 0, time.Local)
	valid := providerBalanceResult{OK: true, HasBalance: true, Balance: 10, Currency: "USD"}
	if err := saveProviderSnapshot("deepseek", "account-a", valid, "manual", at); err != nil {
		t.Fatal(err)
	}
	if err := saveProviderSnapshot("deepseek", "account-a", valid, "manual", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	invalid := providerBalanceResult{OK: true, Description: "没有可用的数字余额"}
	if err := saveProviderSnapshot("deepseek", "account-a", invalid, "manual", at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	if err := openUsageStore(cfg); err != nil {
		t.Fatal(err)
	}
	serverUsageStore.mu.Lock()
	var count int
	err := serverUsageStore.db.QueryRow(`SELECT COUNT(*) FROM usage_snapshots`).Scan(&count)
	serverUsageStore.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("snapshot count after duplicate/invalid writes = %d, want 1", count)
	}

	changed := valid
	changed.Balance = 9
	if err := saveProviderSnapshot("deepseek", "account-a", changed, "manual", at.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	serverUsageStore.mu.Lock()
	err = serverUsageStore.db.QueryRow(`SELECT COUNT(*) FROM usage_snapshots`).Scan(&count)
	serverUsageStore.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("snapshot count after changed value = %d, want 2", count)
	}
}

func TestUsageHistoryWhereUsesAllFilters(t *testing.T) {
	where, args := usageHistoryWhere(usageHistoryRequest{
		From: "2026-09-01", To: "2026-09-07", Key: "account-a",
		Provider: "relay", Source: "scheduled", Currency: "CNY",
	})
	if !strings.Contains(where, "account_key = ?") || !strings.Contains(where, "provider = ?") || !strings.Contains(where, "source = ?") {
		t.Fatalf("where clause missing filters: %s", where)
	}
	if len(args) != 6 || args[2] != "account-a" || args[3] != "relay" || args[4] != "scheduled" || args[5] != "CNY" {
		t.Fatalf("unexpected filter args: %#v", args)
	}
}

func TestUsageHistoryWhereSupportsAllDateDeleteRange(t *testing.T) {
	where, args := usageHistoryWhere(usageHistoryRequest{From: "1970-01-01", To: "2026-09-28"})
	if !strings.HasPrefix(where, "day >= ? AND day <= ?") {
		t.Fatalf("unexpected date clause: %s", where)
	}
	if len(args) != 2 || args[0] != "1970-01-01" || args[1] != "2026-09-28" {
		t.Fatalf("unexpected date args: %#v", args)
	}
}
func TestUsageStoreRetentionZeroKeepsHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retention.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE usage_snapshots (day TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO usage_snapshots(day) VALUES (?)`, "2020-01-01"); err != nil {
		t.Fatal(err)
	}
	if err := pruneUsageStoreLocked(db, 0, time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_snapshots`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("retention=0 deleted history, count=%d", count)
	}
}
func TestUsageStoreMigratesFingerprintColumn(t *testing.T) {
	closeUsageStore()
	oldConfig := currentConfig()
	defer func() {
		closeUsageStore()
		configMu.Lock()
		runtimeConfig = oldConfig
		configMu.Unlock()
	}()

	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE usage_snapshots (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  observed_at TEXT NOT NULL,
  day TEXT NOT NULL,
  account_key TEXT NOT NULL,
  provider TEXT NOT NULL,
  currency TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT 'balance',
  window TEXT NOT NULL DEFAULT '',
  balance REAL,
  used REAL,
  limit_value REAL,
  remaining REAL,
  remaining_fraction REAL,
  reset_time TEXT NOT NULL DEFAULT '',
  has_balance INTEGER NOT NULL DEFAULT 0,
  has_used INTEGER NOT NULL DEFAULT 0,
  has_limit INTEGER NOT NULL DEFAULT 0,
  source TEXT NOT NULL DEFAULT 'manual'
)`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := oldConfig
	cfg.Enabled = false
	cfg.UsageDBPath = path
	cfg.UsageRetentionDays = 400
	configMu.Lock()
	runtimeConfig = cfg
	configMu.Unlock()
	if err := openUsageStore(cfg); err != nil {
		t.Fatal(err)
	}
	serverUsageStore.mu.Lock()
	var column string
	err = serverUsageStore.db.QueryRow(`SELECT name FROM pragma_table_info('usage_snapshots') WHERE name = 'fingerprint'`).Scan(&column)
	serverUsageStore.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if column != "fingerprint" {
		t.Fatalf("migrated column = %q, want fingerprint", column)
	}
}
