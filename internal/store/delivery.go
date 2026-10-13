package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/skrashevich/botmux/internal/models"
	"time"
)

// QueuePolicy bounds unread data by backpressure, never by dropping it.
type QueuePolicy struct {
	MaxUpdates       int
	MaxBytes         int64
	WarnUpdates      int
	WarnAge          time.Duration
	ReceiptRetention time.Duration
}

func DefaultQueuePolicy() QueuePolicy {
	return QueuePolicy{100000, 256 << 20, 10000, time.Hour, 30 * 24 * time.Hour}
}

var ErrQueueFull = errors.New("queue capacity reached; delivery must be retried")

func (s *Store) SetQueuePolicy(p QueuePolicy) error {
	if p.MaxUpdates <= 0 || p.MaxBytes <= 0 || p.WarnUpdates <= 0 || p.WarnAge <= 0 || p.ReceiptRetention <= 0 {
		return fmt.Errorf("queue policy values must be positive")
	}
	s.policyMu.Lock()
	s.queuePolicy = p
	s.policyMu.Unlock()
	return nil
}
func (s *Store) QueuePolicy() QueuePolicy {
	s.policyMu.RLock()
	defer s.policyMu.RUnlock()
	return s.queuePolicy
}

// DeliveryJob separates planning, remote sending, and committing local results.
type DeliveryJob struct {
	ID        int64           `json:"id"`
	BotID     int64           `json:"bot_id"`
	EventKey  string          `json:"event_key"`
	Operation string          `json:"operation"`
	Payload   json.RawMessage `json:"payload"`
	Result    json.RawMessage `json:"result"`
	Attempts  int             `json:"attempts"`
	LastError string          `json:"last_error"`
	CreatedAt int64           `json:"created_at"`
}

func (s *Store) migrateDelivery() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS route_deliveries(id INTEGER PRIMARY KEY AUTOINCREMENT,bot_id INTEGER NOT NULL,event_key TEXT NOT NULL,operation TEXT NOT NULL,payload TEXT NOT NULL,result TEXT NOT NULL DEFAULT 'null',attempts INTEGER NOT NULL DEFAULT 0,last_error TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,next_attempt INTEGER NOT NULL DEFAULT 0,completed_at INTEGER NOT NULL DEFAULT 0,lease_owner TEXT NOT NULL DEFAULT '',lease_until INTEGER NOT NULL DEFAULT 0,UNIQUE(bot_id,event_key,operation));
 CREATE INDEX IF NOT EXISTS route_delivery_due ON route_deliveries(completed_at,next_attempt,lease_until);
 CREATE TABLE IF NOT EXISTS handled_update_ranges(bot_id INTEGER NOT NULL,start_id INTEGER NOT NULL,end_id INTEGER NOT NULL,PRIMARY KEY(bot_id,start_id));`)
	if err != nil {
		return err
	}
	for _, col := range []struct{ table, name, definition string }{
		{"pending_updates", "created_at", "INTEGER NOT NULL DEFAULT 0"},
		{"pending_updates", "size_bytes", "INTEGER NOT NULL DEFAULT 0"},
		{"update_consumers", "last_poll_at", "INTEGER NOT NULL DEFAULT 0"},
		{"handled_updates", "handled_at", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var n int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?", col.table, col.name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := s.db.Exec("ALTER TABLE " + col.table + " ADD COLUMN " + col.name + " " + col.definition); err != nil {
				return err
			}
		}
	}
	_, err = s.db.Exec(`UPDATE pending_updates SET created_at=unixepoch('now')*1000 WHERE created_at=0;
 UPDATE pending_updates SET size_bytes=length(CAST(data AS BLOB)) WHERE size_bytes=0;
 UPDATE handled_updates SET handled_at=unixepoch('now')*1000 WHERE handled_at=0;`)
	return err
}

func enqueueDelivery(tx *sql.Tx, j DeliveryJob, now int64) error {
	_, err := tx.Exec(`INSERT INTO route_deliveries(bot_id,event_key,operation,payload,created_at) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, j.BotID, j.EventKey, j.Operation, string(j.Payload), now)
	return err
}
func (s *Store) ScheduleRouting(j DeliveryJob, updateID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists, n int
	var size int64
	if err := tx.QueryRow("SELECT COUNT(*) FROM route_deliveries WHERE bot_id=? AND event_key=? AND operation=?", j.BotID, j.EventKey, j.Operation).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if err := tx.QueryRow("SELECT COUNT(*),COALESCE(SUM(length(CAST(payload AS BLOB))),0) FROM route_deliveries WHERE bot_id=? AND completed_at=0", j.BotID).Scan(&n, &size); err != nil {
			return err
		}
		policy := s.QueuePolicy()
		if n >= policy.MaxUpdates || size+int64(len(j.Payload)) > policy.MaxBytes {
			return ErrQueueFull
		}
	}
	now := time.Now().UnixMilli()
	if err := enqueueDelivery(tx, j, now); err != nil {
		return err
	}
	if updateID > 0 {
		if _, err := tx.Exec("INSERT INTO handled_updates(bot_id,update_id,handled_at) VALUES(?,?,?) ON CONFLICT DO NOTHING", j.BotID, updateID, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ExpandDelivery makes a persisted decision and its child actions atomic.
func (s *Store) ExpandDelivery(id int64, owner string, jobs []DeliveryJob) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM route_deliveries WHERE id=? AND lease_owner=? AND completed_at=0", id, owner).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("delivery lease lost")
	}
	now := time.Now().UnixMilli()
	for _, j := range jobs {
		if err := enqueueDelivery(tx, j, now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("UPDATE route_deliveries SET completed_at=?,lease_owner='',lease_until=0,last_error='',payload='{}',result='null' WHERE id=?", now, id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ClaimDelivery(owner string, now time.Time, lease time.Duration) (*DeliveryJob, error) {
	return s.ClaimDeliveryBefore(owner, now, now, lease)
}
func (s *Store) ClaimDeliveryBefore(owner string, now, dueBefore time.Time, lease time.Duration) (*DeliveryJob, error) {
	// UPDATE RETURNING claims exactly one row atomically, including across managers.
	row := s.db.QueryRow(`UPDATE route_deliveries SET lease_owner=?,lease_until=? WHERE id=(SELECT d.id FROM route_deliveries d JOIN bots b ON b.id=d.bot_id WHERE d.completed_at=0 AND d.next_attempt<=? AND d.lease_until<=? AND b.disabled=0 ORDER BY d.id LIMIT 1) RETURNING id,bot_id,event_key,operation,payload,result,attempts,last_error,created_at`, owner, now.Add(lease).UnixMilli(), dueBefore.UnixMilli(), now.UnixMilli())
	var j DeliveryJob
	err := row.Scan(&j.ID, &j.BotID, &j.EventKey, &j.Operation, jsonScan(&j.Payload), jsonScan(&j.Result), &j.Attempts, &j.LastError, &j.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &j, err
}
func (s *Store) SaveDeliveryResult(id int64, owner string, result json.RawMessage) error {
	res, err := s.db.Exec("UPDATE route_deliveries SET result=? WHERE id=? AND lease_owner=? AND completed_at=0", jsonText(result), id, owner)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return fmt.Errorf("delivery lease lost")
	}
	return err
}
func (s *Store) CompleteDelivery(id int64, owner string) error {
	res, err := s.db.Exec("UPDATE route_deliveries SET completed_at=?,lease_owner='',lease_until=0,last_error='',payload='{}',result='null' WHERE id=? AND lease_owner=? AND completed_at=0", time.Now().UnixMilli(), id, owner)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return fmt.Errorf("delivery lease lost")
	}
	return err
}
func (s *Store) RetryDelivery(id int64, owner string, next time.Time, reason string) error {
	_, err := s.db.Exec("UPDATE route_deliveries SET attempts=attempts+1,next_attempt=?,last_error=?,lease_owner='',lease_until=0 WHERE id=? AND lease_owner=? AND completed_at=0", next.UnixMilli(), reason, id, owner)
	return err
}
func (s *Store) ReleaseDeliveryLeases(owner string) error {
	_, err := s.db.Exec("UPDATE route_deliveries SET lease_owner='',lease_until=0 WHERE lease_owner=?", owner)
	return err
}

type QueueHealth struct {
	BotID         int64  `json:"bot_id"`
	Pending       int    `json:"pending_updates"`
	Bytes         int64  `json:"pending_bytes"`
	OldestAt      int64  `json:"oldest_update_at"`
	LastPollAt    int64  `json:"last_poll_at"`
	RoutePending  int    `json:"pending_routes"`
	RouteBytes    int64  `json:"pending_route_bytes"`
	RouteOldestAt int64  `json:"oldest_route_at"`
	RouteFailed   int    `json:"failed_routes"`
	Warning       string `json:"warning"`
}

func (s *Store) QueueHealth(botID int64, now time.Time) (QueueHealth, error) {
	h := QueueHealth{BotID: botID}
	if err := s.db.QueryRow("SELECT COUNT(*),COALESCE(SUM(size_bytes),0),COALESCE(MIN(created_at),0) FROM pending_updates WHERE bot_id=?", botID).Scan(&h.Pending, &h.Bytes, &h.OldestAt); err != nil {
		return h, err
	}
	err := s.db.QueryRow("SELECT last_poll_at FROM update_consumers WHERE bot_id=?", botID).Scan(&h.LastPollAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return h, err
	}
	if err := s.db.QueryRow("SELECT COUNT(*),COALESCE(MIN(created_at),0),COALESCE(SUM(CASE WHEN attempts>0 THEN 1 ELSE 0 END),0),COALESCE(SUM(length(CAST(payload AS BLOB))),0) FROM route_deliveries WHERE bot_id=? AND completed_at=0", botID).Scan(&h.RoutePending, &h.RouteOldestAt, &h.RouteFailed, &h.RouteBytes); err != nil {
		return h, err
	}
	p := s.QueuePolicy()
	switch {
	case h.Pending >= p.MaxUpdates || h.Bytes >= p.MaxBytes || h.RoutePending >= p.MaxUpdates || h.RouteBytes >= p.MaxBytes:
		h.Warning = "capacity reached; intake is paused until pending work is consumed"
	case h.Pending >= p.WarnUpdates || h.RoutePending >= p.WarnUpdates:
		h.Warning = "large backlog"
	case h.OldestAt > 0 && now.Sub(time.UnixMilli(h.OldestAt)) >= p.WarnAge:
		h.Warning = "consumer stalled; unread updates are retained"
	case h.RouteOldestAt > 0 && now.Sub(time.UnixMilli(h.RouteOldestAt)) >= p.WarnAge:
		h.Warning = "route delivery stalled; pending jobs are retained"
	}
	return h, nil
}

// MaintainQueues removes only completed work. Old receipt IDs are compacted into
// ranges so cleanup does not re-enable duplicates after a restart.
func (s *Store) MaintainQueues(now time.Time) error {
	cutoff := now.Add(-s.QueuePolicy().ReceiptRetention).UnixMilli()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT h.bot_id,h.update_id FROM handled_updates h WHERE h.handled_at<? AND NOT EXISTS(SELECT 1 FROM pending_updates p WHERE p.bot_id=h.bot_id AND p.update_id=h.update_id) AND NOT EXISTS(SELECT 1 FROM route_deliveries d WHERE d.bot_id=h.bot_id AND d.event_key='tg:'||h.update_id AND d.completed_at=0) ORDER BY h.bot_id,h.update_id LIMIT 1000`, cutoff)
	if err != nil {
		return err
	}
	type receipt struct{ botID, id int64 }
	var receipts []receipt
	for rows.Next() {
		var r receipt
		if err := rows.Scan(&r.botID, &r.id); err != nil {
			rows.Close()
			return err
		}
		receipts = append(receipts, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range receipts {
		var start, end int64
		if err := tx.QueryRow("SELECT MIN(start_id),MAX(end_id) FROM (SELECT ? AS start_id,? AS end_id UNION ALL SELECT start_id,end_id FROM handled_update_ranges WHERE bot_id=? AND end_id>=? AND start_id<=?)", r.id, r.id, r.botID, r.id-1, r.id+1).Scan(&start, &end); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM handled_update_ranges WHERE bot_id=? AND end_id>=? AND start_id<=?", r.botID, r.id-1, r.id+1); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO handled_update_ranges(bot_id,start_id,end_id) VALUES(?,?,?)", r.botID, start, end); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM handled_updates WHERE bot_id=? AND update_id=?", r.botID, r.id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM route_deliveries WHERE completed_at>0 AND completed_at<?", cutoff); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CompleteRouteDelivery(id int64, owner string, m *models.RouteMapping) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM route_deliveries WHERE id=? AND lease_owner=? AND completed_at=0", id, owner).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("delivery lease lost")
	}
	if m != nil {
		if _, err := tx.Exec(`INSERT INTO route_mappings(route_id,source_bot_id,source_chat_id,source_msg_id,target_bot_id,target_chat_id,target_msg_id,created_at) SELECT ?,?,?,?,?,?,?,? WHERE NOT EXISTS(SELECT 1 FROM route_mappings WHERE route_id=? AND source_bot_id=? AND source_chat_id=? AND source_msg_id=? AND target_bot_id=? AND target_chat_id=? AND target_msg_id=?)`, m.RouteID, m.SourceBotID, m.SourceChatID, m.SourceMsgID, m.TargetBotID, m.TargetChatID, m.TargetMsgID, m.CreatedAt, m.RouteID, m.SourceBotID, m.SourceChatID, m.SourceMsgID, m.TargetBotID, m.TargetChatID, m.TargetMsgID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("UPDATE route_deliveries SET completed_at=?,lease_owner='',lease_until=0,last_error='',payload='{}',result='null' WHERE id=?", time.Now().UnixMilli(), id); err != nil {
		return err
	}
	return tx.Commit()
}
