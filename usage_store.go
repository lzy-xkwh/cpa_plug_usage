package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
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
	if path := strings.TrimSpace(cfg.UsageDBPath); path != "" {
		return path
	}
	if path := strings.TrimSpace(os.Getenv("API_BALANCE_USAGE_DB")); path != "" {
		return path
	}
	return "api-balance-usage.db"
}

func openUsageStore(cfg config) error {
	path := filepath.Clean(usageDBPath(cfg))
	serverUsageStore.mu.Lock()
	defer serverUsageStore.mu.Unlock()
	if serverUsageStore.db != nil && serverUsageStore.path == path {
		return nil
	}
	if serverUsageStore.db != nil {
		_ = serverUsageStore.db.Close()
		serverUsageStore.db = nil
		serverUsageStore.path = ""
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return fmt.Errorf("创建 SQLite 目录失败: %w", err)
		}
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("SQLite 路径不能是符号链接: %s", path)
	}
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	// The plugin has one serialized writer. A single connection avoids pool
	// contention and makes WAL/busy-timeout behavior predictable.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`
PRAGMA busy_timeout = 5000;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA foreign_keys = ON;
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
  fingerprint TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT 'manual'
);
CREATE INDEX IF NOT EXISTS idx_usage_snapshots_day ON usage_snapshots(day);
CREATE INDEX IF NOT EXISTS idx_usage_snapshots_account ON usage_snapshots(account_key, observed_at);
`); err != nil {
		_ = db.Close()
		return fmt.Errorf("初始化 SQLite 表失败: %w", err)
	}
	if err := ensureUsageFingerprintColumn(db); err != nil {
		_ = db.Close()
		return err
	}
	if err := os.Chmod(path, 0600); err != nil && !os.IsNotExist(err) {
		_ = db.Close()
		return fmt.Errorf("设置 SQLite 文件权限失败: %w", err)
	}
	serverUsageStore.db, serverUsageStore.path = db, path
	if err := pruneUsageStoreLocked(db, cfg.UsageRetentionDays, time.Now()); err != nil {
		_ = db.Close()
		serverUsageStore.db = nil
		serverUsageStore.path = ""
		return err
	}
	return nil
}

func ensureUsageFingerprintColumn(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(usage_snapshots)`)
	if err != nil {
		return fmt.Errorf("读取 SQLite 表结构失败: %w", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("解析 SQLite 表结构失败: %w", err)
		}
		if name == "fingerprint" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取 SQLite 表结构失败: %w", err)
	}
	if !found {
		if _, err := db.Exec(`ALTER TABLE usage_snapshots ADD COLUMN fingerprint TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("升级 SQLite 表结构失败: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_usage_snapshots_dedup ON usage_snapshots(account_key, day, kind, currency, window, fingerprint)`); err != nil {
		return fmt.Errorf("创建 SQLite 去重索引失败: %w", err)
	}
	return nil
}

func closeUsageStore() {
	serverUsageStore.mu.Lock()
	defer serverUsageStore.mu.Unlock()
	if serverUsageStore.db != nil {
		_ = serverUsageStore.db.Close()
		serverUsageStore.db = nil
		serverUsageStore.path = ""
	}
}

func finiteUsageNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validQuotaWindow(window providerQuotaWindow) bool {
	return strings.TrimSpace(window.Window) != "" && finiteUsageNumber(window.Remaining)
}

func validCashResult(result providerBalanceResult) bool {
	return (result.HasBalance && finiteUsageNumber(result.Balance)) ||
		(result.HasUsed && finiteUsageNumber(result.Used)) ||
		(result.HasLimit && finiteUsageNumber(result.Limit))
}

func usageFingerprint(provider, accountKey, day, kind, currency, window string, result providerBalanceResult, quota *providerQuotaWindow) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00", provider, accountKey, day, kind, currency, window)
	if quota != nil {
		fmt.Fprintf(&builder, "remaining=%t:%g\x00fraction=%g\x00reset=%s", finiteUsageNumber(quota.Remaining), quota.Remaining, quota.RemainingFraction, quota.ResetTime)
	} else {
		fmt.Fprintf(&builder, "balance=%t:%g\x00used=%t:%g\x00limit=%t:%g", result.HasBalance, result.Balance, result.HasUsed, result.Used, result.HasLimit, result.Limit)
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

func snapshotExistsTx(tx *sql.Tx, accountKey, day, kind, currency, window, fingerprint string) (bool, error) {
	var one int
	err := tx.QueryRow(`SELECT 1 FROM usage_snapshots WHERE account_key = ? AND day = ? AND kind = ? AND currency = ? AND window = ? AND fingerprint = ? LIMIT 1`, accountKey, day, kind, currency, window, fingerprint).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("检查重复 SQLite 快照失败: %w", err)
	}
	return true, nil
}

func saveProviderSnapshot(provider, accountKey string, result providerBalanceResult, source string, observedAt time.Time) error {
	if !result.OK {
		return nil
	}
	if len(result.QuotaWindows) == 0 && !validCashResult(result) {
		// Successful HTTP responses without a usable numeric value are not
		// observations and must not create endless identical database rows.
		return nil
	}
	if err := openUsageStore(currentConfig()); err != nil {
		return err
	}
	serverUsageStore.mu.Lock()
	defer serverUsageStore.mu.Unlock()
	if serverUsageStore.db == nil {
		return fmt.Errorf("SQLite 尚未打开")
	}
	if source == "" {
		source = "manual"
	}
	day := observedAt.Local().Format("2006-01-02")
	observedAtText := observedAt.UTC().Format(time.RFC3339Nano)
	tx, err := serverUsageStore.db.Begin()
	if err != nil {
		return fmt.Errorf("开始 SQLite 事务失败: %w", err)
	}
	rollback := func(cause error) error {
		_ = tx.Rollback()
		return cause
	}
	currency := strings.TrimSpace(result.Currency)
	if currency == "" && len(result.QuotaWindows) == 0 {
		currency = "CNY"
	}
	if len(result.QuotaWindows) > 0 {
		for _, window := range result.QuotaWindows {
			if !validQuotaWindow(window) {
				continue
			}
			windowName := strings.TrimSpace(window.Window)
			fingerprint := usageFingerprint(provider, accountKey, day, "quota", "积分", windowName, result, &window)
			duplicate, err := snapshotExistsTx(tx, accountKey, day, "quota", "积分", windowName, fingerprint)
			if err != nil {
				return rollback(err)
			}
			if duplicate {
				continue
			}
			var fraction any
			if finiteUsageNumber(window.RemainingFraction) {
				fraction = window.RemainingFraction
			}
			if _, err := tx.Exec(`INSERT INTO usage_snapshots
(observed_at,day,account_key,provider,currency,kind,window,remaining,remaining_fraction,reset_time,fingerprint,source)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, observedAtText, day, accountKey, provider, "积分", "quota", windowName, window.Remaining, fraction, window.ResetTime, fingerprint, source); err != nil {
				return rollback(fmt.Errorf("写入配额快照失败: %w", err))
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("提交 SQLite 配额快照失败: %w", err)
		}
		return pruneUsageStoreLocked(serverUsageStore.db, currentConfig().UsageRetentionDays, observedAt)
	}
	var balance, used, limit any
	if result.HasBalance && finiteUsageNumber(result.Balance) {
		balance = result.Balance
	}
	if result.HasUsed && finiteUsageNumber(result.Used) {
		used = result.Used
	}
	if result.HasLimit && finiteUsageNumber(result.Limit) {
		limit = result.Limit
	}
	fingerprint := usageFingerprint(provider, accountKey, day, "balance", currency, "", result, nil)
	duplicate, err := snapshotExistsTx(tx, accountKey, day, "balance", currency, "", fingerprint)
	if err != nil {
		return rollback(err)
	}
	if !duplicate {
		if _, err := tx.Exec(`INSERT INTO usage_snapshots
(observed_at,day,account_key,provider,currency,kind,balance,used,limit_value,has_balance,has_used,has_limit,fingerprint,source)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, observedAtText, day, accountKey, provider, currency, "balance", balance, used, limit, boolInt(result.HasBalance && balance != nil), boolInt(result.HasUsed && used != nil), boolInt(result.HasLimit && limit != nil), fingerprint, source); err != nil {
			return rollback(fmt.Errorf("写入余额快照失败: %w", err))
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 SQLite 余额快照失败: %w", err)
	}
	return pruneUsageStoreLocked(serverUsageStore.db, currentConfig().UsageRetentionDays, observedAt)
}

func pruneUsageStoreLocked(db *sql.DB, retentionDays int, now time.Time) error {
	if retentionDays <= 0 {
		return nil
	}
	cutoff := now.Local().AddDate(0, 0, -retentionDays).Format("2006-01-02")
	if _, err := db.Exec(`DELETE FROM usage_snapshots WHERE day < ?`, cutoff); err != nil {
		return fmt.Errorf("清理过期 SQLite 快照失败: %w", err)
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

type usageHistoryRequest struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Key      string `json:"key"`
	Provider string `json:"provider"`
	Source   string `json:"source"`
	Currency string `json:"currency"`
}

func normalizedHistoryRange(req usageHistoryRequest) (string, string) {
	from, to := strings.TrimSpace(req.From), strings.TrimSpace(req.To)
	now := time.Now()
	if from == "" {
		from = now.AddDate(0, 0, -30).Format("2006-01-02")
	}
	if to == "" {
		to = now.Format("2006-01-02")
	}
	return from, to
}

func usageHistoryWhere(req usageHistoryRequest) (string, []any) {
	from, to := normalizedHistoryRange(req)
	where := "day >= ? AND day <= ?"
	args := []any{from, to}
	if key := strings.TrimSpace(req.Key); key != "" {
		where += " AND account_key = ?"
		args = append(args, key)
	}
	if provider := strings.TrimSpace(req.Provider); provider != "" {
		where += " AND provider = ?"
		args = append(args, provider)
	}
	if source := strings.TrimSpace(req.Source); source != "" {
		where += " AND source = ?"
		args = append(args, source)
	}
	if currency := strings.ToUpper(strings.TrimSpace(req.Currency)); currency != "" {
		switch currency {
		case "CNY":
			where += " AND (currency = ? OR (currency = '' AND kind = 'balance'))"
			args = append(args, currency)
		case "积分":
			where += " AND kind = 'quota'"
		default:
			where += " AND currency = ?"
			args = append(args, currency)
		}
	}
	return where, args
}

func clearUsageHistory(req usageHistoryRequest) error {
	if err := openUsageStore(currentConfig()); err != nil {
		return err
	}
	serverUsageStore.mu.Lock()
	defer serverUsageStore.mu.Unlock()
	if serverUsageStore.db == nil {
		return fmt.Errorf("SQLite 尚未打开")
	}
	where, args := usageHistoryWhere(req)
	if _, err := serverUsageStore.db.Exec("DELETE FROM usage_snapshots WHERE "+where, args...); err != nil {
		return fmt.Errorf("清空 SQLite 历史失败: %w", err)
	}
	return nil
}

func usageHistoryResponse(req usageHistoryRequest) (map[string]any, error) {
	if err := openUsageStore(currentConfig()); err != nil {
		return nil, err
	}
	serverUsageStore.mu.Lock()
	defer serverUsageStore.mu.Unlock()
	if serverUsageStore.db == nil {
		return nil, fmt.Errorf("SQLite 尚未打开")
	}
	from, to := normalizedHistoryRange(req)
	where, args := usageHistoryWhere(req)
	query := `SELECT observed_at,day,account_key,provider,currency,kind,window,balance,used,limit_value,remaining,remaining_fraction,reset_time,has_balance,has_used,has_limit,source FROM usage_snapshots WHERE ` + where
	query += " ORDER BY observed_at DESC, id DESC LIMIT 5000"
	rows, err := serverUsageStore.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("读取用量历史失败: %w", err)
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var observedAt, day, accountKey, provider, currency, kind, window, resetTime, source string
		var balance, used, limitValue, remaining, fraction sql.NullFloat64
		var hasBalance, hasUsed, hasLimit int
		if err := rows.Scan(&observedAt, &day, &accountKey, &provider, &currency, &kind, &window, &balance, &used, &limitValue, &remaining, &fraction, &resetTime, &hasBalance, &hasUsed, &hasLimit, &source); err != nil {
			return nil, fmt.Errorf("解析用量历史失败: %w", err)
		}
		item := map[string]any{"observed_at": observedAt, "day": day, "account_key": accountKey, "provider": provider, "currency": currency, "kind": kind, "window": window, "reset_time": resetTime, "source": source, "has_balance": hasBalance != 0, "has_used": hasUsed != 0, "has_limit": hasLimit != 0}
		if balance.Valid {
			item["balance"] = balance.Float64
		}
		if used.Valid {
			item["used"] = used.Float64
		}
		if limitValue.Valid {
			item["limit"] = limitValue.Float64
		}
		if remaining.Valid {
			item["remaining"] = remaining.Float64
		}
		if fraction.Valid {
			item["remaining_fraction"] = fraction.Float64
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"from": from, "to": to, "items": items}, nil
}

func marshalUsageHistory(req usageHistoryRequest) ([]byte, error) {
	result, err := usageHistoryResponse(req)
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}
