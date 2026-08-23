package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/JudiLite/CDT-Monitor/internal/domain"
	"github.com/JudiLite/CDT-Monitor/internal/security"
)

const minAPIIntervalSeconds = 30

var sensitiveSettings = map[string]bool{
	"notify_password":      true,
	"notify_tg_token":      true,
	"notify_tg_proxy_pass": true,
	"notify_wh_headers":    true,
	"notify_wh_secret":     true,
}

func boolSetting(settings map[string]string, key string, fallback bool) bool {
	value, ok := settings[key]
	if !ok {
		return fallback
	}
	return value == "1" || strings.EqualFold(value, "true")
}

func intSetting(settings map[string]string, key string, fallback int) int {
	value, ok := settings[key]
	if !ok {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func (s *Store) getSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	settings := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err = rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		if sensitiveSettings[key] {
			value, err = s.Decrypt(value)
			if err != nil {
				return nil, fmt.Errorf("decrypt setting %s: %w", key, err)
			}
		}
		settings[key] = value
	}
	return settings, rows.Err()
}

func (s *Store) GetConfig(ctx context.Context) (domain.Config, error) {
	settings, err := s.getSettings(ctx)
	if err != nil {
		return domain.Config{}, err
	}
	accounts, err := s.ListAccounts(ctx)
	if err != nil {
		return domain.Config{}, err
	}
	if accounts == nil {
		accounts = []domain.Account{}
	}
	apiInterval := intSetting(settings, "api_interval", 600)
	if apiInterval < minAPIIntervalSeconds {
		apiInterval = minAPIIntervalSeconds
	}
	config := domain.Config{
		TrafficThreshold:   intSetting(settings, "traffic_threshold", 95),
		EnableScheduleMail: boolSetting(settings, "enable_schedule_email", false),
		EnableDailyReport:  boolSetting(settings, "enable_daily_report", true),
		DailyReportTime:    valueOr(settings, "daily_report_time", "23:59"),
		ShutdownMode:       valueOr(settings, "shutdown_mode", "KeepCharging"),
		ThresholdAction:    valueOr(settings, "threshold_action", "stop_and_notify"),
		KeepAlive:          boolSetting(settings, "keep_alive", false),
		APIInterval:        apiInterval,
		EnableBilling:      boolSetting(settings, "enable_billing", false),
		Timezone:           valueOr(settings, "timezone", "Asia/Shanghai"),
		Accounts:           accounts,
		Notifications: domain.NotificationConfig{
			Email: domain.EmailConfig{
				Enabled:            boolSetting(settings, "notify_email_enabled", true),
				To:                 valueOr(settings, "notify_email", ""),
				Host:               valueOr(settings, "notify_host", ""),
				Port:               intSetting(settings, "notify_port", 465),
				Username:           valueOr(settings, "notify_username", ""),
				Password:           valueOr(settings, "notify_password", ""),
				PasswordConfigured: settings["notify_password"] != "",
				Security:           valueOr(settings, "notify_secure", "ssl"),
			},
			Telegram: domain.TelegramConfig{
				Enabled:         boolSetting(settings, "notify_tg_enabled", false),
				Token:           valueOr(settings, "notify_tg_token", ""),
				TokenConfigured: settings["notify_tg_token"] != "",
				ChatID:          valueOr(settings, "notify_tg_chat_id", ""),
				ProxyType:       valueOr(settings, "notify_tg_proxy_type", "none"),
				ProxyURL:        valueOr(settings, "notify_tg_proxy_url", ""),
				ProxyIP:         valueOr(settings, "notify_tg_proxy_ip", ""),
				ProxyPort:       valueOr(settings, "notify_tg_proxy_port", ""),
				ProxyUser:       valueOr(settings, "notify_tg_proxy_user", ""),
				ProxyPass:       valueOr(settings, "notify_tg_proxy_pass", ""),
				ProxyConfigured: settings["notify_tg_proxy_pass"] != "",
			},
			Webhook: domain.WebhookConfig{
				Enabled:          boolSetting(settings, "notify_wh_enabled", false),
				URL:              valueOr(settings, "notify_wh_url", ""),
				Method:           valueOr(settings, "notify_wh_method", "GET"),
				Type:             valueOr(settings, "notify_wh_request_type", "JSON"),
				Headers:          valueOr(settings, "notify_wh_headers", ""),
				Body:             valueOr(settings, "notify_wh_body", ""),
				Provider:         valueOr(settings, "notify_wh_provider", "generic"),
				Secret:           valueOr(settings, "notify_wh_secret", ""),
				SecretConfigured: settings["notify_wh_secret"] != "",
			},
		},
	}
	return config, nil
}

func valueOr(values map[string]string, key, fallback string) string {
	if value, ok := values[key]; ok && value != "" {
		return value
	}
	return fallback
}

func (s *Store) IsInitialized(ctx context.Context) (bool, error) {
	var password string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='admin_password'`).Scan(&password)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && password != "", err
}

func (s *Store) Setup(ctx context.Context, config domain.Config) error {
	initialized, err := s.IsInitialized(ctx)
	if err != nil {
		return err
	}
	if initialized {
		return errors.New("system is already initialized")
	}
	return s.saveConfig(ctx, config, true)
}

func (s *Store) SaveConfig(ctx context.Context, config domain.Config) error {
	return s.saveConfig(ctx, config, false)
}

func (s *Store) saveConfig(ctx context.Context, config domain.Config, setup bool) error {
	if config.TrafficThreshold < 1 || config.TrafficThreshold > 100 {
		return errors.New("traffic threshold must be between 1 and 100")
	}
	if config.ShutdownMode != "KeepCharging" && config.ShutdownMode != "StopCharging" {
		return errors.New("invalid shutdown mode")
	}
	if config.ThresholdAction != "stop_and_notify" && config.ThresholdAction != "notify_only" {
		return errors.New("invalid threshold action")
	}
	if config.APIInterval < minAPIIntervalSeconds || config.APIInterval > 86400 {
		return errors.New("api interval must be between 30 and 86400 seconds")
	}
	if config.DailyReportTime == "" {
		config.DailyReportTime = "23:59"
	}
	if _, err := time.Parse("15:04", config.DailyReportTime); err != nil {
		return errors.New("daily report time must use HH:MM format")
	}
	if config.Timezone == "" {
		config.Timezone = "Asia/Shanghai"
	}
	if setup && len(config.AdminPassword) < 10 {
		return errors.New("administrator password must be at least 10 characters")
	}

	return s.WithTx(ctx, func(tx *sql.Tx) error {
		if config.AdminPassword != "" {
			hash, err := hashOrKeepPassword(config.AdminPassword)
			if err != nil {
				return err
			}
			if err = putSettingTx(ctx, tx, "admin_password", hash); err != nil {
				return err
			}
		} else if setup {
			return errors.New("administrator password is required")
		}

		values := map[string]string{
			"traffic_threshold":      strconv.Itoa(config.TrafficThreshold),
			"enable_schedule_email":  strconv.FormatBool(config.EnableScheduleMail),
			"enable_daily_report":    strconv.FormatBool(config.EnableDailyReport),
			"daily_report_time":      config.DailyReportTime,
			"shutdown_mode":          config.ShutdownMode,
			"threshold_action":       config.ThresholdAction,
			"keep_alive":             strconv.FormatBool(config.KeepAlive),
			"api_interval":           strconv.Itoa(config.APIInterval),
			"enable_billing":         strconv.FormatBool(config.EnableBilling),
			"timezone":               config.Timezone,
			"notify_email_enabled":   strconv.FormatBool(config.Notifications.Email.Enabled),
			"notify_email":           config.Notifications.Email.To,
			"notify_host":            config.Notifications.Email.Host,
			"notify_port":            strconv.Itoa(config.Notifications.Email.Port),
			"notify_username":        config.Notifications.Email.Username,
			"notify_secure":          config.Notifications.Email.Security,
			"notify_tg_enabled":      strconv.FormatBool(config.Notifications.Telegram.Enabled),
			"notify_tg_chat_id":      config.Notifications.Telegram.ChatID,
			"notify_tg_proxy_type":   config.Notifications.Telegram.ProxyType,
			"notify_tg_proxy_url":    config.Notifications.Telegram.ProxyURL,
			"notify_tg_proxy_ip":     config.Notifications.Telegram.ProxyIP,
			"notify_tg_proxy_port":   config.Notifications.Telegram.ProxyPort,
			"notify_tg_proxy_user":   config.Notifications.Telegram.ProxyUser,
			"notify_wh_enabled":      strconv.FormatBool(config.Notifications.Webhook.Enabled),
			"notify_wh_url":          config.Notifications.Webhook.URL,
			"notify_wh_method":       config.Notifications.Webhook.Method,
			"notify_wh_request_type": config.Notifications.Webhook.Type,
			"notify_wh_body":         config.Notifications.Webhook.Body,
			"notify_wh_provider":     config.Notifications.Webhook.Provider,
		}
		for key, value := range values {
			if err := putSettingTx(ctx, tx, key, value); err != nil {
				return err
			}
		}
		for key, value := range map[string]string{
			"notify_password":      config.Notifications.Email.Password,
			"notify_tg_token":      config.Notifications.Telegram.Token,
			"notify_tg_proxy_pass": config.Notifications.Telegram.ProxyPass,
			"notify_wh_headers":    config.Notifications.Webhook.Headers,
			"notify_wh_secret":     config.Notifications.Webhook.Secret,
		} {
			if value == "" {
				continue
			}
			encrypted, err := s.Encrypt(value)
			if err != nil {
				return err
			}
			if err = putSettingTx(ctx, tx, key, encrypted); err != nil {
				return err
			}
		}

		if err := saveAccountsTx(ctx, tx, s, config.Accounts); err != nil {
			return err
		}
		return nil
	})
}

func hashOrKeepPassword(password string) (string, error) {
	if len(password) > 14 && strings.HasPrefix(password, "$argon2id$") {
		return password, nil
	}
	return hashPassword(password)
}

// Kept in this file to make password migration explicit at the storage boundary.
func hashPassword(password string) (string, error) {
	return security.HashPassword(password)
}

func putSettingTx(ctx context.Context, tx *sql.Tx, key, value string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func saveAccountsTx(ctx context.Context, tx *sql.Tx, s *Store, accounts []domain.Account) error {
	activeRows, err := tx.QueryContext(ctx, `SELECT id, access_key_id, region_id, instance_id, access_key_secret FROM accounts WHERE deleted_at=0`)
	if err != nil {
		return err
	}
	type existing struct {
		id, key, region, instance, secret string
	}
	byID := map[int64]existing{}
	byComposite := map[string]existing{}
	for activeRows.Next() {
		var id int64
		var row existing
		if err = activeRows.Scan(&id, &row.key, &row.region, &row.instance, &row.secret); err != nil {
			activeRows.Close()
			return err
		}
		row.id = fmt.Sprint(id)
		byID[id] = row
		byComposite[row.key+"|"+row.region+"|"+row.instance] = row
	}
	activeRows.Close()
	kept := make(map[int64]bool)
	for _, account := range accounts {
		if strings.TrimSpace(account.AccessKeyID) == "" || strings.TrimSpace(account.RegionID) == "" {
			return errors.New("account access_key_id and region_id are required")
		}
		row, found := byID[account.ID]
		if !found {
			row, found = byComposite[account.AccessKeyID+"|"+account.RegionID+"|"+account.InstanceID]
		}
		secret := account.AccessKeySecret
		if secret == "" && found {
			secret, err = s.Decrypt(row.secret)
			if err != nil {
				return err
			}
		}
		if secret == "" {
			return fmt.Errorf("account %s is missing access key secret", account.AccessKeyID)
		}
		encryptedSecret, err := s.Encrypt(secret)
		if err != nil {
			return err
		}
		siteType := account.SiteType
		if siteType != "international" {
			siteType = "china"
		}
		if found {
			id, _ := strconv.ParseInt(row.id, 10, 64)
			_, err = tx.ExecContext(ctx, `UPDATE accounts SET access_key_id=?, access_key_secret=?, region_id=?, instance_id=?, max_traffic=?, schedule_enabled=?, start_time=?, stop_time=?, remark=?, site_type=?, deleted_at=0 WHERE id=?`,
				account.AccessKeyID, encryptedSecret, account.RegionID, account.InstanceID, account.MaxTraffic, boolInt(account.ScheduleEnabled), account.StartTime, account.StopTime, account.Remark, siteType, id)
			if err != nil {
				return err
			}
			kept[id] = true
		} else {
			result, err := tx.ExecContext(ctx, `INSERT INTO accounts(access_key_id,access_key_secret,region_id,instance_id,max_traffic,schedule_enabled,start_time,stop_time,remark,site_type,instance_status) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
				account.AccessKeyID, encryptedSecret, account.RegionID, account.InstanceID, account.MaxTraffic, boolInt(account.ScheduleEnabled), account.StartTime, account.StopTime, account.Remark, siteType, domain.StatusUnknown)
			if err != nil {
				return err
			}
			id, err := result.LastInsertId()
			if err != nil {
				return err
			}
			kept[id] = true
		}
	}
	if len(accounts) == 0 {
		_, err = tx.ExecContext(ctx, `UPDATE accounts SET deleted_at=unixepoch() WHERE deleted_at=0`)
		return err
	}
	for id := range byID {
		if !kept[id] {
			if _, err = tx.ExecContext(ctx, `UPDATE accounts SET deleted_at=unixepoch() WHERE id=?`, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *Store) ListAccounts(ctx context.Context) ([]domain.Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,access_key_id,access_key_secret,region_id,instance_id,max_traffic,schedule_enabled,start_time,stop_time,traffic_used,instance_status,updated_at,last_keep_alive_at,remark,site_type FROM accounts WHERE deleted_at=0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []domain.Account
	for rows.Next() {
		var a domain.Account
		var secret string
		var schedule, updated, keepAlive int64
		if err = rows.Scan(&a.ID, &a.AccessKeyID, &secret, &a.RegionID, &a.InstanceID, &a.MaxTraffic, &schedule, &a.StartTime, &a.StopTime, &a.TrafficUsed, &a.InstanceStatus, &updated, &keepAlive, &a.Remark, &a.SiteType); err != nil {
			return nil, err
		}
		secret, err = s.Decrypt(secret)
		if err != nil {
			return nil, err
		}
		a.SecretConfigured = secret != ""
		a.ScheduleEnabled = schedule == 1
		if updated > 0 {
			a.UpdatedAt = time.Unix(updated, 0).UTC()
		}
		if keepAlive > 0 {
			a.LastKeepAliveAt = time.Unix(keepAlive, 0).UTC()
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

func (s *Store) GetAccount(ctx context.Context, id int64) (domain.Account, error) {
	accounts, err := s.ListAccounts(ctx)
	if err != nil {
		return domain.Account{}, err
	}
	for _, account := range accounts {
		if account.ID == id {
			return account, nil
		}
	}
	return domain.Account{}, sql.ErrNoRows
}

func (s *Store) AccountSecret(ctx context.Context, id int64) (string, error) {
	var encrypted string
	err := s.db.QueryRowContext(ctx, `SELECT access_key_secret FROM accounts WHERE id=? AND deleted_at=0`, id).Scan(&encrypted)
	if err != nil {
		return "", err
	}
	return s.Decrypt(encrypted)
}

func (s *Store) updateRuntime(ctx context.Context, id int64, traffic float64, status string, updatedAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET traffic_used=?,instance_status=?,updated_at=? WHERE id=? AND deleted_at=0`, traffic, status, updatedAt.Unix(), id)
	return err
}

func (s *Store) UpdateRuntime(ctx context.Context, id int64, traffic float64, status string, updatedAt time.Time) error {
	return s.updateRuntime(ctx, id, traffic, status, updatedAt)
}

func (s *Store) UpdateKeepAliveAt(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET last_keep_alive_at=? WHERE id=?`, at.Unix(), id)
	return err
}
