package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/JudiLite/CDT-Monitor/internal/domain"
	"github.com/JudiLite/CDT-Monitor/internal/security"
)

func (s *Store) AddLog(ctx context.Context, logType, message string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO logs(type,message,created_at) VALUES(?,?,unixepoch())`, logType, message)
	return err
}

func (s *Store) ListLogs(ctx context.Context, tab string, limit int) ([]domain.LogEntry, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	types := []string{"info", "warning", "error", "audit"}
	if tab == "heartbeat" {
		types = []string{"heartbeat"}
	}
	placeholders := "?"
	args := []any{types[0]}
	for _, value := range types[1:] {
		placeholders += ",?"
		args = append(args, value)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT id,type,message,created_at FROM logs WHERE type IN (`+placeholders+`) ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]domain.LogEntry, 0)
	for rows.Next() {
		var entry domain.LogEntry
		var created int64
		if err = rows.Scan(&entry.ID, &entry.Type, &entry.Message, &created); err != nil {
			return nil, err
		}
		entry.CreatedAt = time.Unix(created, 0).UTC()
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (s *Store) ClearLogs(ctx context.Context, tab string) error {
	if tab == "heartbeat" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM logs WHERE type='heartbeat'`)
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM logs WHERE type!='heartbeat'`)
	return err
}

func (s *Store) Prune(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
DELETE FROM logs WHERE (type='heartbeat' AND created_at<unixepoch()-259200) OR (type!='heartbeat' AND created_at<unixepoch()-2592000);
DELETE FROM traffic_hourly WHERE recorded_at<unixepoch()-172800;
DELETE FROM traffic_daily WHERE recorded_at<unixepoch()-5184000;
DELETE FROM billing_cache WHERE updated_at<unixepoch()-7776000;
DELETE FROM sessions WHERE expires_at<unixepoch();
DELETE FROM login_attempts WHERE attempt_time<unixepoch()-86400;
DELETE FROM jobs WHERE status IN ('completed','failed') AND updated_at<unixepoch()-604800;
DELETE FROM notification_outbox WHERE status IN ('sent','failed') AND updated_at<unixepoch()-2592000;
`)
	return err
}

func (s *Store) AddTrafficStats(ctx context.Context, accountID int64, traffic float64, now time.Time) error {
	hour := now.Truncate(time.Hour).Unix()
	year, month, day := now.Date()
	daily := time.Date(year, month, day, 0, 0, 0, 0, now.Location()).Unix()
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO traffic_hourly(account_id,traffic,recorded_at) VALUES(?,?,?) ON CONFLICT(account_id,recorded_at) DO UPDATE SET traffic=excluded.traffic`, accountID, traffic, hour); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO traffic_daily(account_id,traffic,recorded_at) VALUES(?,?,?) ON CONFLICT(account_id,recorded_at) DO UPDATE SET traffic=excluded.traffic`, accountID, traffic, daily)
		return err
	})
}

func (s *Store) History(ctx context.Context, accountID int64) (domain.History, error) {
	history := domain.History{Hourly: []domain.TrafficPoint{}, Daily: []domain.TrafficPoint{}}
	for query, target := range map[string]*[]domain.TrafficPoint{
		`SELECT traffic,recorded_at FROM traffic_hourly WHERE account_id=? ORDER BY recorded_at DESC LIMIT 25`: &history.Hourly,
		`SELECT traffic,recorded_at FROM traffic_daily WHERE account_id=? ORDER BY recorded_at DESC LIMIT 31`:  &history.Daily,
	} {
		rows, err := s.db.QueryContext(ctx, query, accountID)
		if err != nil {
			return history, err
		}
		var reverse []domain.TrafficPoint
		for rows.Next() {
			var point domain.TrafficPoint
			var at int64
			if err = rows.Scan(&point.Traffic, &at); err != nil {
				rows.Close()
				return history, err
			}
			point.At = time.Unix(at, 0).UTC()
			reverse = append(reverse, point)
		}
		rows.Close()
		for index := len(reverse) - 1; index >= 0; index-- {
			*target = append(*target, reverse[index])
		}
	}
	return history, nil
}

func (s *Store) PreviousDailyTraffic(ctx context.Context, accountID int64, before time.Time) (float64, bool, error) {
	var traffic float64
	err := s.db.QueryRowContext(ctx, `SELECT traffic FROM traffic_daily WHERE account_id=? AND recorded_at<? ORDER BY recorded_at DESC LIMIT 1`, accountID, before.Unix()).Scan(&traffic)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return traffic, true, nil
}

func (s *Store) RecentDailyTraffic(ctx context.Context, accountID int64, limit int) ([]domain.TrafficPoint, error) {
	if limit < 1 {
		limit = 2
	}
	rows, err := s.db.QueryContext(ctx, `SELECT traffic,recorded_at FROM traffic_daily WHERE account_id=? ORDER BY recorded_at DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := make([]domain.TrafficPoint, 0, limit)
	for rows.Next() {
		var point domain.TrafficPoint
		var at int64
		if err = rows.Scan(&point.Traffic, &at); err != nil {
			return nil, err
		}
		point.At = time.Unix(at, 0).UTC()
		points = append(points, point)
	}
	return points, rows.Err()
}

func (s *Store) LastMonitorRun(ctx context.Context) (time.Time, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='last_monitor_run'`).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	unix, _ := strconv.ParseInt(value, 10, 64)
	return time.Unix(unix, 0).UTC(), nil
}

func (s *Store) SetLastMonitorRun(ctx context.Context, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('last_monitor_run',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, strconv.FormatInt(at.Unix(), 10))
	return err
}

func (s *Store) EnqueueJob(ctx context.Context, jobType string, accountID int64, payload, uniqueKey string, maxAttempts int) (domain.Job, error) {
	id, err := security.NewToken(18)
	if err != nil {
		return domain.Job{}, err
	}
	job := domain.Job{ID: id, Type: jobType, AccountID: accountID, Payload: payload, Status: "queued", MaxAttempts: maxAttempts, AvailableAt: time.Now().UTC(), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	_, err = s.db.ExecContext(ctx, `INSERT INTO jobs(id,type,account_id,payload,unique_key,status,max_attempts,available_at,created_at,updated_at) VALUES(?,?,?,?,?,'queued',?,?,?,?)`,
		job.ID, job.Type, job.AccountID, job.Payload, nullableString(uniqueKey), job.MaxAttempts, job.AvailableAt.Unix(), job.CreatedAt.Unix(), job.UpdatedAt.Unix())
	if err != nil && uniqueKey != "" {
		var existingID string
		lookupErr := s.db.QueryRowContext(ctx, `SELECT id FROM jobs WHERE unique_key=?`, uniqueKey).Scan(&existingID)
		if lookupErr == nil {
			return s.GetJob(ctx, existingID)
		}
	}
	return job, err
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Store) GetJob(ctx context.Context, id string) (domain.Job, error) {
	var job domain.Job
	var available, created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id,type,account_id,payload,status,result,error,attempts,max_attempts,available_at,created_at,updated_at FROM jobs WHERE id=?`, id).
		Scan(&job.ID, &job.Type, &job.AccountID, &job.Payload, &job.Status, &job.Result, &job.Error, &job.Attempts, &job.MaxAttempts, &available, &created, &updated)
	job.AvailableAt, job.CreatedAt, job.UpdatedAt = time.Unix(available, 0).UTC(), time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC()
	return job, err
}

func (s *Store) ClaimJob(ctx context.Context) (domain.Job, error) {
	var job domain.Job
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		var available, created, updated int64
		err := tx.QueryRowContext(ctx, `SELECT id,type,account_id,payload,status,result,error,attempts,max_attempts,available_at,created_at,updated_at FROM jobs WHERE status='queued' AND available_at<=unixepoch() ORDER BY created_at LIMIT 1`).
			Scan(&job.ID, &job.Type, &job.AccountID, &job.Payload, &job.Status, &job.Result, &job.Error, &job.Attempts, &job.MaxAttempts, &available, &created, &updated)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE jobs SET status='running',locked_at=unixepoch(),attempts=attempts+1,updated_at=unixepoch() WHERE id=? AND status='queued'`, job.ID)
		if err != nil {
			return err
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return sql.ErrNoRows
		}
		job.Status, job.Attempts = "running", job.Attempts+1
		job.AvailableAt, job.CreatedAt, job.UpdatedAt = time.Unix(available, 0).UTC(), time.Unix(created, 0).UTC(), time.Now().UTC()
		return nil
	})
	return job, err
}

func (s *Store) CompleteJob(ctx context.Context, id, result string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='completed',result=?,error='',unique_key=CASE WHEN type='monitor_account' THEN unique_key ELSE NULL END,updated_at=unixepoch() WHERE id=?`, result, id)
	return err
}

func (s *Store) FailJob(ctx context.Context, job domain.Job, jobErr error) error {
	status := "failed"
	available := time.Now().UTC()
	if job.Attempts < job.MaxAttempts {
		status = "queued"
		delay := time.Duration(1<<min(job.Attempts, 6)) * time.Second
		available = available.Add(delay)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET status=?,error=?,available_at=?,unique_key=CASE WHEN ?='failed' THEN NULL ELSE unique_key END,updated_at=unixepoch() WHERE id=?`, status, jobErr.Error(), available.Unix(), status, job.ID)
	return err
}

func (s *Store) AcquireLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO scheduler_leases(name,owner,expires_at,updated_at) VALUES(?,?,?,?) ON CONFLICT(name) DO UPDATE SET owner=excluded.owner,expires_at=excluded.expires_at,updated_at=excluded.updated_at WHERE scheduler_leases.expires_at<? OR scheduler_leases.owner=?`,
		name, owner, now.Add(ttl).Unix(), now.Unix(), now.Unix(), owner)
	if err != nil {
		return false, err
	}
	count, _ := result.RowsAffected()
	return count == 1, nil
}

func (s *Store) RecordActionEvent(ctx context.Context, key string, accountID int64, eventType, status, detail string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO action_events(event_key,account_id,type,status,detail,created_at,updated_at) VALUES(?,?,?,?,?,unixepoch(),unixepoch())`, key, accountID, eventType, status, detail)
	if err != nil {
		return false, err
	}
	count, _ := result.RowsAffected()
	return count == 1, nil
}

func (s *Store) DeleteActionEvent(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM action_events WHERE event_key=?`, key)
	return err
}

func (s *Store) AddOutbox(ctx context.Context, event domain.NotificationEvent, channels []string) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		for _, channel := range channels {
			id := event.ID + ":" + channel
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO notification_outbox(id,event_id,channel,payload,status,available_at,created_at,updated_at) VALUES(?,?,?,?,'queued',unixepoch(),unixepoch(),unixepoch())`, id, event.ID, channel, string(payload)); err != nil {
				return err
			}
		}
		return nil
	})
}

type OutboxItem struct {
	ID, Channel, Payload  string
	Attempts, MaxAttempts int
}

func (s *Store) ClaimOutbox(ctx context.Context) (OutboxItem, error) {
	var item OutboxItem
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT id,channel,payload,attempts,max_attempts FROM notification_outbox WHERE status='queued' AND available_at<=unixepoch() ORDER BY created_at LIMIT 1`).Scan(&item.ID, &item.Channel, &item.Payload, &item.Attempts, &item.MaxAttempts); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE notification_outbox SET status='sending',attempts=attempts+1,updated_at=unixepoch() WHERE id=? AND status='queued'`, item.ID)
		if err != nil {
			return err
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return sql.ErrNoRows
		}
		item.Attempts++
		return nil
	})
	return item, err
}

func (s *Store) CompleteOutbox(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notification_outbox SET status='sent',last_error='',updated_at=unixepoch() WHERE id=?`, id)
	return err
}

func (s *Store) FailOutbox(ctx context.Context, item OutboxItem, sendErr error) error {
	status := "failed"
	available := time.Now().UTC()
	if item.Attempts < item.MaxAttempts {
		status = "queued"
		available = available.Add(time.Duration(1<<min(item.Attempts, 7)) * time.Second)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE notification_outbox SET status=?,last_error=?,available_at=?,updated_at=unixepoch() WHERE id=?`, status, sendErr.Error(), available.Unix(), item.ID)
	return err
}

func (s *Store) BillingCache(ctx context.Context, accountID int64, cacheType, cycle string, maxAge time.Duration, target any) (bool, error) {
	var data string
	var updated int64
	err := s.db.QueryRowContext(ctx, `SELECT data,updated_at FROM billing_cache WHERE account_id=? AND cache_type=? AND billing_cycle=?`, accountID, cacheType, cycle).Scan(&data, &updated)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && time.Since(time.Unix(updated, 0)) > maxAge) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(data), target)
}

func (s *Store) SetBillingCache(ctx context.Context, accountID int64, cacheType, cycle string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO billing_cache(account_id,cache_type,billing_cycle,data,updated_at) VALUES(?,?,?,?,unixepoch()) ON CONFLICT(account_id,cache_type,billing_cycle) DO UPDATE SET data=excluded.data,updated_at=excluded.updated_at`, accountID, cacheType, cycle, string(data))
	return err
}

func (s *Store) CountQueuedJobs(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE status IN ('queued','running')`).Scan(&count)
	return count, err
}

func (s *Store) DebugStats(ctx context.Context) (map[string]int, error) {
	result := map[string]int{}
	for _, table := range []string{"accounts", "logs", "jobs", "notification_outbox", "api_keys"} {
		var count int
		if err := s.db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&count); err != nil {
			return nil, err
		}
		result[table] = count
	}
	return result, nil
}
