package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type usageStore struct {
	mu   sync.Mutex
	db   *sql.DB
	path string
}

var serverUsageStore usageStore

func usageDBPath(cfg config) string {
	if path := strings.TrimSpace(cfg.UsageDBPath); path != "" { return path }
	if path := strings.TrimSpace(os.Getenv("API_BALANCE_USAGE_DB")); path != "" { return path }
	return "api-balance-usage.db"
}

func openUsageStore(cfg config) error {
	path := usageDBPath(cfg)
	serverUsageStore.mu.Lock()
	defer serverUsageStore.mu.Unlock()
	if serverUsageStore.db != nil && serverUsageStore.path == path { return nil }
	if serverUsageStore.db != nil { _ = serverUsageStore.db.Close(); serverUsageStore.db = nil }
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0750); err != nil { return fmt.Errorf("创建 SQLite 目录失败: %w", err) }
	}
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil { return fmt.Errorf("打开 SQLite 失败: %w", err) }
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS usage_snapshots (
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
);
CREATE INDEX IF NOT EXISTS idx_usage_snapshots_day ON usage_snapshots(day);
CREATE INDEX IF NOT EXISTS idx_usage_snapshots_account ON usage_snapshots(account_key, observed_at);
`); err != nil { _ = db.Close(); return fmt.Errorf("初始化 SQLite 表失败: %w", err) }
	serverUsageStore.db, serverUsageStore.path = db, path
	return nil
}

func closeUsageStore() {
	serverUsageStore.mu.Lock(); defer serverUsageStore.mu.Unlock()
	if serverUsageStore.db != nil { _ = serverUsageStore.db.Close(); serverUsageStore.db = nil }
}

func saveProviderSnapshot(provider, accountKey string, result providerBalanceResult, source string, observedAt time.Time) error {
	if err := openUsageStore(currentConfig()); err != nil { return err }
	serverUsageStore.mu.Lock(); defer serverUsageStore.mu.Unlock()
	if serverUsageStore.db == nil { return fmt.Errorf("SQLite 尚未打开") }
	if source == "" { source = "manual" }
	day := observedAt.Local().Format("2006-01-02")
	currency := strings.TrimSpace(result.Currency)
	if currency == "" && len(result.QuotaWindows) == 0 { currency = "CNY" }
	if len(result.QuotaWindows) > 0 {
		for _, window := range result.QuotaWindows {
			if _, err := serverUsageStore.db.Exec(`INSERT INTO usage_snapshots
(observed_at,day,account_key,provider,currency,kind,window,remaining,remaining_fraction,reset_time,source)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, observedAt.UTC().Format(time.RFC3339Nano), day, accountKey, provider, "积分", "quota", window.Window, window.Remaining, window.RemainingFraction, window.ResetTime, source); err != nil { return fmt.Errorf("写入配额快照失败: %w", err) }
		}
		return nil
	}
	var balance, used, limit any
	if result.HasBalance { balance = result.Balance }
	if result.HasUsed { used = result.Used }
	if result.HasLimit { limit = result.Limit }
	_, err := serverUsageStore.db.Exec(`INSERT INTO usage_snapshots
(observed_at,day,account_key,provider,currency,kind,balance,used,limit_value,has_balance,has_used,has_limit,source)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, observedAt.UTC().Format(time.RFC3339Nano), day, accountKey, provider, currency, "balance", balance, used, limit, boolInt(result.HasBalance), boolInt(result.HasUsed), boolInt(result.HasLimit), source)
	if err != nil { return fmt.Errorf("写入余额快照失败: %w", err) }
	return nil
}

func boolInt(value bool) int { if value { return 1 }; return 0 }

type usageHistoryRequest struct { From string `json:"from"`; To string `json:"to"`; Key string `json:"key"` }

func usageHistoryResponse(req usageHistoryRequest) (map[string]any, error) {
	if err := openUsageStore(currentConfig()); err != nil { return nil, err }
	serverUsageStore.mu.Lock(); defer serverUsageStore.mu.Unlock()
	if serverUsageStore.db == nil { return nil, fmt.Errorf("SQLite 尚未打开") }
	from, to := strings.TrimSpace(req.From), strings.TrimSpace(req.To)
	if from == "" { from = time.Now().AddDate(0, 0, -30).Format("2006-01-02") }
	if to == "" { to = time.Now().Format("2006-01-02") }
	query := `SELECT observed_at,day,account_key,provider,currency,kind,window,balance,used,limit_value,remaining,remaining_fraction,reset_time,has_balance,has_used,has_limit,source FROM usage_snapshots WHERE day >= ? AND day <= ?`
	args := []any{from, to}
	if req.Key != "" { query += " AND account_key = ?"; args = append(args, req.Key) }
	query += " ORDER BY observed_at DESC, id DESC"
	rows, err := serverUsageStore.db.Query(query, args...)
	if err != nil { return nil, fmt.Errorf("读取用量历史失败: %w", err) }
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var observedAt, day, accountKey, provider, currency, kind, window, resetTime, source string
		var balance, used, limitValue, remaining, fraction sql.NullFloat64
		var hasBalance, hasUsed, hasLimit int
		if err := rows.Scan(&observedAt, &day, &accountKey, &provider, &currency, &kind, &window, &balance, &used, &limitValue, &remaining, &fraction, &resetTime, &hasBalance, &hasUsed, &hasLimit, &source); err != nil { return nil, fmt.Errorf("解析用量历史失败: %w", err) }
		item := map[string]any{"observed_at": observedAt, "day": day, "account_key": accountKey, "provider": provider, "currency": currency, "kind": kind, "window": window, "reset_time": resetTime, "source": source, "has_balance": hasBalance != 0, "has_used": hasUsed != 0, "has_limit": hasLimit != 0}
		if balance.Valid { item["balance"] = balance.Float64 }; if used.Valid { item["used"] = used.Float64 }; if limitValue.Valid { item["limit"] = limitValue.Float64 }; if remaining.Valid { item["remaining"] = remaining.Float64 }; if fraction.Valid { item["remaining_fraction"] = fraction.Float64 }
		items = append(items, item)
	}
	if err := rows.Err(); err != nil { return nil, err }
	return map[string]any{"from": from, "to": to, "items": items}, nil
}

func marshalUsageHistory(req usageHistoryRequest) ([]byte, error) { result, err := usageHistoryResponse(req); if err != nil { return nil, err }; return json.Marshal(result) }
