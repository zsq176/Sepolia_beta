package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

const (
	sqliteBusyTimeoutMs = 5000
	sqliteWriteRetries  = 6
)

type Trade struct {
	ID        int64   `json:"id"`
	Timestamp int64   `json:"timestamp"`
	Wallet    string  `json:"wallet_address,omitempty"`
	Pair      string  `json:"pair"`
	Action    string  `json:"action"`
	Amount    string  `json:"amount"`
	Price     float64 `json:"price"`
	TxHash    string  `json:"tx_hash"`
	Status    string  `json:"status"`
	Source    string  `json:"source,omitempty"`
}

type PricePoint struct {
	Time  int64   `json:"time"`
	Pair  string  `json:"pair"`
	Interval string `json:"interval,omitempty"`
	Open  float64 `json:"open"`
	High  float64 `json:"high"`
	Low   float64 `json:"low"`
	Close float64 `json:"close"`
}

type Position struct {
	Wallet string  `json:"wallet_address,omitempty"`
	Token  string  `json:"token"`
	Amount string  `json:"amount"`
	Value  float64 `json:"value_usd"`
}

type DecisionLog struct {
	ID           int64   `json:"id"`
	Timestamp    int64   `json:"timestamp"`
	InstrumentID string  `json:"instrument_id"`
	QualityLevel string  `json:"quality_level"`
	PoolPrice    float64 `json:"pool_price"`
	TargetPrice  float64 `json:"target_price"`
	Deviation    float64 `json:"deviation"`
	NotionalUSD  float64 `json:"notional_usd"`
	State        string  `json:"state"`
	Allowed      bool    `json:"allowed"`
	Reason       string  `json:"reason"`
}

type ExecutionLog struct {
	ID           int64  `json:"id"`
	Timestamp    int64  `json:"timestamp"`
	InstrumentID string `json:"instrument_id"`
	Wallet       string `json:"wallet_address"`
	QualityLevel string `json:"quality_level"`
	Action       string `json:"action"`
	AmountIn     string `json:"amount_in"`
	MinAmountOut string `json:"min_amount_out"`
	TxHash       string `json:"tx_hash"`
	Status       string `json:"status"`
	Reason       string `json:"reason"`
	GasUsed      int64  `json:"gas_used"`
}

type UserOpLog struct {
	ID        int64  `json:"id"`
	Timestamp int64  `json:"timestamp"`
	Wallet    string `json:"wallet_address"`
	Operation string `json:"operation"`
	Target    string `json:"target"`
	Detail    string `json:"detail"`
	Status    string `json:"status"`
	TxHash    string `json:"tx_hash,omitempty"`
}

func NewStore(dbPath string) (*Store, error) {
	normalized, err := normalizeDBPath(dbPath)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", normalized)
	if err != nil {
		return nil, err
	}
	// Keep writes serialized inside this process to avoid SQLITE_BUSY storms.
	// WAL + busy_timeout still allow concurrent readers while one writer works.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := configureSQLite(db); err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func normalizeDBPath(input string) (string, error) {
	dbPath := strings.TrimSpace(input)
	if dbPath == "" {
		dbPath = filepath.Join("data", "trading.db")
	}
	dir := filepath.Dir(dbPath)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create db dir %s: %w", dir, err)
		}
	}

	// One-time migration: move legacy root trading.db to data/trading.db
	// when user switches to new default path and target does not exist yet.
	clean := filepath.Clean(dbPath)
	if clean == filepath.Clean(filepath.Join("data", "trading.db")) {
		legacy := "trading.db"
		if _, err := os.Stat(clean); os.IsNotExist(err) {
			if _, err := os.Stat(legacy); err == nil {
				if err := copyFile(legacy, clean); err != nil {
					return "", fmt.Errorf("migrate legacy db: %w", err)
				}
			}
		}
	}
	return clean, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func configureSQLite(db *sql.DB) error {
	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
		fmt.Sprintf("PRAGMA busy_timeout=%d;", sqliteBusyTimeoutMs),
		"PRAGMA temp_store=MEMORY;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return err
		}
	}
	return nil
}

func migrate(db *sql.DB) error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS trades (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp INTEGER NOT NULL,
			pair TEXT NOT NULL,
			action TEXT NOT NULL,
			amount TEXT NOT NULL,
			price REAL NOT NULL,
			tx_hash TEXT,
			status TEXT DEFAULT 'pending'
		)`,
		`CREATE TABLE IF NOT EXISTS prices (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			time INTEGER NOT NULL,
			pair TEXT NOT NULL,
			open REAL NOT NULL,
			high REAL NOT NULL,
			low REAL NOT NULL,
			close REAL NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_prices_pair_time ON prices(pair, time)`,
		`CREATE TABLE IF NOT EXISTS market_klines (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pair TEXT NOT NULL,
			interval TEXT NOT NULL,
			bucket_ts INTEGER NOT NULL,
			open REAL NOT NULL,
			high REAL NOT NULL,
			low REAL NOT NULL,
			close REAL NOT NULL,
			UNIQUE(pair, interval, bucket_ts)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_market_klines_pair_interval_bucket ON market_klines(pair, interval, bucket_ts)`,
		`CREATE TABLE IF NOT EXISTS positions (
			token TEXT PRIMARY KEY,
			amount TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS account_trades (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp INTEGER NOT NULL,
			wallet_address TEXT NOT NULL,
			pair TEXT NOT NULL,
			action TEXT NOT NULL,
			amount TEXT NOT NULL,
			price REAL NOT NULL,
			tx_hash TEXT,
			status TEXT DEFAULT 'pending',
			source TEXT DEFAULT 'user'
		)`,
		`CREATE INDEX IF NOT EXISTS idx_account_trades_wallet_time ON account_trades(wallet_address, timestamp DESC)`,
		`CREATE TABLE IF NOT EXISTS account_positions (
			wallet_address TEXT NOT NULL,
			token TEXT NOT NULL,
			amount TEXT NOT NULL,
			value_usd REAL NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY(wallet_address, token)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_account_positions_wallet ON account_positions(wallet_address)`,
		`CREATE TABLE IF NOT EXISTS decision_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp INTEGER NOT NULL,
			instrument_id TEXT NOT NULL,
			quality_level TEXT NOT NULL,
			pool_price REAL NOT NULL,
			target_price REAL NOT NULL,
			deviation REAL NOT NULL,
			notional_usd REAL NOT NULL,
			state TEXT NOT NULL,
			allowed INTEGER NOT NULL,
			reason TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_decision_logs_inst_time ON decision_logs(instrument_id, timestamp DESC)`,
		`CREATE TABLE IF NOT EXISTS execution_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp INTEGER NOT NULL,
			instrument_id TEXT NOT NULL,
			wallet_address TEXT NOT NULL,
			quality_level TEXT NOT NULL,
			action TEXT NOT NULL,
			amount_in TEXT NOT NULL,
			min_amount_out TEXT NOT NULL,
			tx_hash TEXT,
			status TEXT NOT NULL,
			reason TEXT,
			gas_used INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_execution_logs_inst_time ON execution_logs(instrument_id, timestamp DESC)`,
		`CREATE TABLE IF NOT EXISTS param_changes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp INTEGER NOT NULL,
			who TEXT NOT NULL,
			param_key TEXT NOT NULL,
			old_value TEXT NOT NULL,
			new_value TEXT NOT NULL,
			reason TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS incidents (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp INTEGER NOT NULL,
			kind TEXT NOT NULL,
			status TEXT NOT NULL,
			details_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS user_op_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp INTEGER NOT NULL,
			wallet_address TEXT NOT NULL,
			operation TEXT NOT NULL,
			target TEXT NOT NULL,
			detail TEXT NOT NULL,
			status TEXT NOT NULL,
			tx_hash TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_user_op_logs_wallet_time ON user_op_logs(wallet_address, timestamp DESC)`,
	}
	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) InsertTrade(t *Trade) error {
	return s.withWriteTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			"INSERT INTO trades (timestamp, pair, action, amount, price, tx_hash, status) VALUES (?, ?, ?, ?, ?, ?, ?)",
			t.Timestamp, t.Pair, t.Action, t.Amount, t.Price, t.TxHash, t.Status,
		); err != nil {
			return err
		}
		if t.Wallet != "" {
			if _, err := tx.Exec(
				"INSERT INTO account_trades (timestamp, wallet_address, pair, action, amount, price, tx_hash, status, source) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
				t.Timestamp, t.Wallet, t.Pair, t.Action, t.Amount, t.Price, t.TxHash, t.Status, defaultIfEmpty(t.Source, "system"),
			); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) GetTrades(limit int) ([]Trade, error) {
	rows, err := s.db.Query("SELECT id, timestamp, pair, action, amount, price, COALESCE(tx_hash,''), COALESCE(status,'') FROM trades ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var trades []Trade
	for rows.Next() {
		var t Trade
		if err := rows.Scan(&t.ID, &t.Timestamp, &t.Pair, &t.Action, &t.Amount, &t.Price, &t.TxHash, &t.Status); err != nil {
			return nil, err
		}
		trades = append(trades, t)
	}
	return trades, nil
}

func (s *Store) GetTradesByWallet(wallet string, limit int) ([]Trade, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, wallet_address, pair, action, amount, price, COALESCE(tx_hash,''), COALESCE(status,''), COALESCE(source,'')
		 FROM account_trades WHERE wallet_address = ? ORDER BY id DESC LIMIT ?`,
		wallet, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var trades []Trade
	for rows.Next() {
		var t Trade
		if err := rows.Scan(&t.ID, &t.Timestamp, &t.Wallet, &t.Pair, &t.Action, &t.Amount, &t.Price, &t.TxHash, &t.Status, &t.Source); err != nil {
			return nil, err
		}
		trades = append(trades, t)
	}
	return trades, nil
}

func (s *Store) InsertPrice(p *PricePoint) error {
	_, err := s.execWrite(
		"INSERT INTO prices (time, pair, open, high, low, close) VALUES (?, ?, ?, ?, ?, ?)",
		p.Time, p.Pair, p.Open, p.High, p.Low, p.Close,
	)
	return err
}

func (s *Store) InsertKline(p *PricePoint) error {
	if p.Interval == "" {
		p.Interval = "1m"
	}
	_, err := s.execWrite(
		`INSERT INTO market_klines (pair, interval, bucket_ts, open, high, low, close)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(pair, interval, bucket_ts) DO UPDATE SET
		   open = excluded.open,
		   high = excluded.high,
		   low = excluded.low,
		   close = excluded.close`,
		p.Pair, p.Interval, p.Time, p.Open, p.High, p.Low, p.Close,
	)
	return err
}

func (s *Store) GetPrices(pair string, interval string, from time.Time, to time.Time, limit int) ([]PricePoint, error) {
	if interval == "" {
		interval = "1m"
	}
	if limit <= 0 || limit > 5000 {
		limit = 2000
	}
	baseInterval := interval
	if interval != "1m" && interval != "5m" && interval != "15m" {
		baseInterval = "1m"
	}
	rows, err := s.db.Query(
		`SELECT bucket_ts, pair, interval, open, high, low, close
		 FROM market_klines
		 WHERE pair = ? AND interval = ? AND bucket_ts >= ? AND bucket_ts <= ?
		 ORDER BY bucket_ts ASC LIMIT ?`,
		pair, baseInterval, from.Unix(), to.Unix(), limit*5,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var points []PricePoint
	for rows.Next() {
		var p PricePoint
		if err := rows.Scan(&p.Time, &p.Pair, &p.Interval, &p.Open, &p.High, &p.Low, &p.Close); err != nil {
			return nil, err
		}
		points = append(points, p)
	}
	if interval == "1m" || interval == "" || interval == baseInterval {
		return points, nil
	}
	return aggregateKlines(points, interval), nil
}

func aggregateKlines(points []PricePoint, interval string) []PricePoint {
	if len(points) == 0 {
		return points
	}
	var step int64 = 60
	switch interval {
	case "5m":
		step = 300
	case "15m":
		step = 900
	default:
		return points
	}
	out := make([]PricePoint, 0, len(points)/5+1)
	var cur *PricePoint
	for _, p := range points {
		bucket := (p.Time / step) * step
		if cur == nil || cur.Time != bucket {
			if cur != nil {
				out = append(out, *cur)
			}
			cp := PricePoint{
				Time:     bucket,
				Pair:     p.Pair,
				Interval: interval,
				Open:     p.Open,
				High:     p.High,
				Low:      p.Low,
				Close:    p.Close,
			}
			cur = &cp
			continue
		}
		if p.High > cur.High {
			cur.High = p.High
		}
		if p.Low < cur.Low {
			cur.Low = p.Low
		}
		cur.Close = p.Close
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

func (s *Store) UpsertPosition(token string, amount string) error {
	_, err := s.execWrite(
		"INSERT INTO positions (token, amount, updated_at) VALUES (?, ?, ?) ON CONFLICT(token) DO UPDATE SET amount = ?, updated_at = ?",
		token, amount, time.Now().Unix(), amount, time.Now().Unix(),
	)
	return err
}

func (s *Store) UpsertAccountPosition(wallet, token, amount string, valueUSD float64) error {
	_, err := s.execWrite(
		`INSERT INTO account_positions (wallet_address, token, amount, value_usd, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(wallet_address, token) DO UPDATE SET amount = ?, value_usd = ?, updated_at = ?`,
		wallet, token, amount, valueUSD, time.Now().Unix(), amount, valueUSD, time.Now().Unix(),
	)
	return err
}

func (s *Store) GetPositions() ([]Position, error) {
	rows, err := s.db.Query("SELECT token, amount FROM positions")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var positions []Position
	for rows.Next() {
		var p Position
		if err := rows.Scan(&p.Token, &p.Amount); err != nil {
			return nil, err
		}
		positions = append(positions, p)
	}
	return positions, nil
}

func (s *Store) GetPositionsByWallet(wallet string) ([]Position, error) {
	rows, err := s.db.Query(
		"SELECT wallet_address, token, amount, value_usd FROM account_positions WHERE wallet_address = ? ORDER BY token ASC",
		wallet,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var positions []Position
	for rows.Next() {
		var p Position
		if err := rows.Scan(&p.Wallet, &p.Token, &p.Amount, &p.Value); err != nil {
			return nil, err
		}
		positions = append(positions, p)
	}
	return positions, nil
}

func (s *Store) UpdateTradeStatus(txHash string, status string) error {
	_, err := s.execWrite("UPDATE trades SET status = ? WHERE tx_hash = ?", status, txHash)
	return err
}

func (s *Store) InsertDecisionLog(l *DecisionLog) error {
	allowed := 0
	if l.Allowed {
		allowed = 1
	}
	_, err := s.execWrite(
		`INSERT INTO decision_logs (timestamp, instrument_id, quality_level, pool_price, target_price, deviation, notional_usd, state, allowed, reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.Timestamp, l.InstrumentID, defaultIfEmpty(l.QualityLevel, "live"), l.PoolPrice, l.TargetPrice, l.Deviation, l.NotionalUSD, defaultIfEmpty(l.State, "Normal"), allowed, l.Reason,
	)
	return err
}

func (s *Store) GetDecisionLogs(instrumentID string, from, to time.Time, limit int) ([]DecisionLog, error) {
	q := `SELECT id, timestamp, instrument_id, quality_level, pool_price, target_price, deviation, notional_usd, state, allowed, COALESCE(reason,'')
		  FROM decision_logs WHERE timestamp >= ? AND timestamp <= ?`
	args := []any{from.Unix(), to.Unix()}
	if instrumentID != "" {
		q += " AND instrument_id = ?"
		args = append(args, instrumentID)
	}
	q += " ORDER BY timestamp ASC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DecisionLog, 0)
	for rows.Next() {
		var l DecisionLog
		var allowed int
		if err := rows.Scan(&l.ID, &l.Timestamp, &l.InstrumentID, &l.QualityLevel, &l.PoolPrice, &l.TargetPrice, &l.Deviation, &l.NotionalUSD, &l.State, &allowed, &l.Reason); err != nil {
			return nil, err
		}
		l.Allowed = allowed == 1
		out = append(out, l)
	}
	return out, nil
}

func (s *Store) InsertExecutionLog(l *ExecutionLog) error {
	_, err := s.execWrite(
		`INSERT INTO execution_logs (timestamp, instrument_id, wallet_address, quality_level, action, amount_in, min_amount_out, tx_hash, status, reason, gas_used)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.Timestamp, l.InstrumentID, l.Wallet, defaultIfEmpty(l.QualityLevel, "live"), l.Action, l.AmountIn, l.MinAmountOut, l.TxHash, l.Status, l.Reason, l.GasUsed,
	)
	return err
}

func (s *Store) InsertParamChange(who, key, oldValue, newValue, reason string) error {
	_, err := s.execWrite(
		`INSERT INTO param_changes (timestamp, who, param_key, old_value, new_value, reason) VALUES (?, ?, ?, ?, ?, ?)`,
		time.Now().Unix(), who, key, oldValue, newValue, reason,
	)
	return err
}

func (s *Store) InsertIncident(kind, status string, details map[string]any) error {
	raw, _ := json.Marshal(details)
	_, err := s.execWrite(
		`INSERT INTO incidents (timestamp, kind, status, details_json) VALUES (?, ?, ?, ?)`,
		time.Now().Unix(), kind, status, string(raw),
	)
	return err
}

func (s *Store) InsertUserOpLog(l *UserOpLog) error {
	if l == nil {
		return fmt.Errorf("nil op log")
	}
	_, err := s.execWrite(
		`INSERT INTO user_op_logs (timestamp, wallet_address, operation, target, detail, status, tx_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		l.Timestamp, strings.ToLower(l.Wallet), l.Operation, l.Target, l.Detail, l.Status, l.TxHash,
	)
	return err
}

func (s *Store) GetUserOpLogsByWallet(wallet string, limit int) ([]UserOpLog, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.Query(
		`SELECT id, timestamp, wallet_address, operation, target, detail, status, COALESCE(tx_hash,'')
		 FROM user_op_logs WHERE wallet_address = ? ORDER BY id DESC LIMIT ?`,
		strings.ToLower(wallet), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]UserOpLog, 0, limit)
	for rows.Next() {
		var l UserOpLog
		if err := rows.Scan(&l.ID, &l.Timestamp, &l.Wallet, &l.Operation, &l.Target, &l.Detail, &l.Status, &l.TxHash); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

func defaultIfEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func (s *Store) Ping() error {
	if s == nil || s.db == nil {
		return fmt.Errorf("db not initialized")
	}
	return s.db.Ping()
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) execWrite(query string, args ...any) (sql.Result, error) {
	var lastErr error
	backoff := 40 * time.Millisecond
	for i := 0; i < sqliteWriteRetries; i++ {
		res, err := s.db.Exec(query, args...)
		if err == nil {
			return res, nil
		}
		if !isBusyErr(err) {
			return nil, err
		}
		lastErr = err
		time.Sleep(backoff)
		if backoff < 500*time.Millisecond {
			backoff *= 2
		}
	}
	return nil, lastErr
}

func (s *Store) withWriteTx(fn func(tx *sql.Tx) error) error {
	var lastErr error
	backoff := 40 * time.Millisecond
	for i := 0; i < sqliteWriteRetries; i++ {
		tx, err := s.db.BeginTx(context.Background(), nil)
		if err != nil {
			if isBusyErr(err) {
				lastErr = err
				time.Sleep(backoff)
				if backoff < 500*time.Millisecond {
					backoff *= 2
				}
				continue
			}
			return err
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback()
			if isBusyErr(err) {
				lastErr = err
				time.Sleep(backoff)
				if backoff < 500*time.Millisecond {
					backoff *= 2
				}
				continue
			}
			return err
		}
		if err := tx.Commit(); err != nil {
			_ = tx.Rollback()
			if isBusyErr(err) {
				lastErr = err
				time.Sleep(backoff)
				if backoff < 500*time.Millisecond {
					backoff *= 2
				}
				continue
			}
			return err
		}
		return nil
	}
	return lastErr
}

func isBusyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "sqlite_busy")
}
