package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JudiLite/CDT-Monitor/internal/aliyun"
	"github.com/JudiLite/CDT-Monitor/internal/domain"
	"github.com/JudiLite/CDT-Monitor/internal/notify"
	"github.com/JudiLite/CDT-Monitor/internal/security"
	"github.com/JudiLite/CDT-Monitor/internal/store"
)

const (
	JobMonitorAccount  = "monitor_account"
	JobRefreshAccount  = "refresh_account"
	JobControlInstance = "control_instance"
	JobTestNotify      = "test_notification"
)

type Engine struct {
	store                 *store.Store
	provider              aliyun.Provider
	notify                *notify.Service
	logger                *slog.Logger
	owner                 string
	wake                  chan struct{}
	workers               int
	started               sync.Once
	accountLocks          sync.Map
	telegramMu            sync.Mutex
	telegramNext          int64
	telegramBotKey        string
	telegramBotCommandTry time.Time
	telegramChats         sync.Map
}

type telegramChatSession struct {
	Mode    string
	Step    string
	Field   string
	Draft   domain.Account
	Updated time.Time
}

type telegramReply struct {
	Text     string
	Keyboard notify.TelegramInlineKeyboard
}

var ErrMonitorBusy = errors.New("monitor scheduler lease is held by another process")

func New(st *store.Store, provider aliyun.Provider, notifier *notify.Service, logger *slog.Logger, workers int) *Engine {
	if workers < 1 {
		workers = 4
	}
	owner, _ := security.NewToken(12)
	return &Engine{store: st, provider: provider, notify: notifier, logger: logger, owner: owner, wake: make(chan struct{}, 1), workers: workers}
}

func (e *Engine) Start(ctx context.Context) {
	e.started.Do(func() {
		go e.scheduler(ctx)
		for index := 0; index < e.workers; index++ {
			go e.worker(ctx, index)
		}
		go e.notificationWorker(ctx)
		go e.telegramBotWorker(ctx)
	})
}

func (e *Engine) Enqueue(ctx context.Context, jobType string, accountID int64, payload, uniqueKey string) (domain.Job, error) {
	job, err := e.store.EnqueueJob(ctx, jobType, accountID, payload, uniqueKey, 3)
	if err == nil {
		e.signal()
	}
	return job, err
}

func (e *Engine) EnqueueRefreshAll(ctx context.Context) ([]domain.Job, error) {
	accounts, err := e.store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	minute := time.Now().UTC().Format("200601021504")
	jobs := make([]domain.Job, 0, len(accounts))
	for _, account := range accounts {
		job, enqueueErr := e.Enqueue(ctx, JobRefreshAccount, account.ID, `{}`, JobUniqueKey(JobRefreshAccount, account.ID, minute))
		if enqueueErr != nil {
			return jobs, enqueueErr
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (e *Engine) RunOnce(ctx context.Context) error {
	acquired, err := e.store.AcquireLease(ctx, "monitor", e.owner, 75*time.Second)
	if err != nil {
		return err
	}
	if !acquired {
		return ErrMonitorBusy
	}
	return e.enqueueMonitorCycle(ctx, time.Now())
}

func (e *Engine) scheduler(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	_ = e.RunOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := e.RunOnce(ctx); err != nil && !errors.Is(err, ErrMonitorBusy) {
				e.logger.Warn("scheduler cycle skipped", "error", err)
			}
			if err := e.enqueueDailyReportIfDue(ctx, now); err != nil {
				e.logger.Warn("daily report skipped", "error", err)
			}
			if now.Minute()%30 == 0 && now.Second() < 15 {
				_ = e.store.Prune(ctx)
			}
		}
	}
}

func (e *Engine) enqueueMonitorCycle(ctx context.Context, now time.Time) error {
	accounts, err := e.store.ListAccounts(ctx)
	if err != nil {
		return err
	}
	minute := now.UTC().Format("200601021504")
	for _, account := range accounts {
		uniqueKey := fmt.Sprintf("monitor:%d:%s", account.ID, minute)
		if _, err = e.Enqueue(ctx, JobMonitorAccount, account.ID, `{}`, uniqueKey); err != nil {
			return err
		}
	}
	return e.store.SetLastMonitorRun(ctx, now.UTC())
}

func (e *Engine) worker(ctx context.Context, index int) {
	ticker := time.NewTicker(800 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.wake:
		case <-ticker.C:
		}
		for {
			job, err := e.store.ClaimJob(ctx)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				e.logger.Error("claim job", "worker", index, "error", err)
				break
			}
			result, runErr := e.runJob(ctx, job)
			if runErr != nil {
				e.logger.Warn("job failed", "job_id", job.ID, "type", job.Type, "error", runErr)
				_ = e.store.FailJob(ctx, job, runErr)
				continue
			}
			_ = e.store.CompleteJob(ctx, job.ID, result)
		}
	}
}

func (e *Engine) runJob(ctx context.Context, job domain.Job) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 55*time.Second)
	defer cancel()
	switch job.Type {
	case JobMonitorAccount:
		return e.processAccount(ctx, job.AccountID, false)
	case JobRefreshAccount:
		return e.processAccount(ctx, job.AccountID, true)
	case JobControlInstance:
		var payload struct {
			Action string `json:"action"`
			Source string `json:"source"`
		}
		if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
			return "", err
		}
		return e.control(ctx, job.AccountID, payload.Action, payload.Source)
	case JobTestNotify:
		var payload struct {
			Channel string `json:"channel"`
		}
		if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
			return "", err
		}
		config, err := e.store.GetConfig(ctx)
		if err != nil {
			return "", err
		}
		event := newEvent("test", "通知通道测试", "CDT Monitor 的通知配置工作正常。", 0, map[string]string{"发送时间": time.Now().Format("2006-01-02 15:04:05")})
		return "notification sent", e.notify.Send(ctx, payload.Channel, event, config)
	default:
		return "", fmt.Errorf("unknown job type %q", job.Type)
	}
}

func (e *Engine) processAccount(ctx context.Context, accountID int64, force bool) (string, error) {
	lock := e.accountLock(accountID)
	lock.Lock()
	defer lock.Unlock()
	config, err := e.store.GetConfig(ctx)
	if err != nil {
		return "", err
	}
	account, err := e.store.GetAccount(ctx, accountID)
	if err != nil {
		return "", err
	}
	secret, err := e.store.AccountSecret(ctx, accountID)
	if err != nil {
		return "", err
	}
	location, err := time.LoadLocation(config.Timezone)
	if err != nil {
		location = time.FixedZone("CST", 8*3600)
	}
	now := time.Now().In(location)

	actions := make([]string, 0, 2)
	statusChangedBySchedule := false
	if account.ScheduleEnabled {
		if dueWithin(now, account.StartTime, 10*time.Minute) {
			changed, runErr := e.executeScheduledAction(ctx, config, account, secret, "start", now)
			if runErr != nil {
				return "", runErr
			}
			if changed {
				actions = append(actions, "scheduled_start")
				account.InstanceStatus = domain.StatusStarting
				statusChangedBySchedule = true
			}
		}
		if dueWithin(now, account.StopTime, 10*time.Minute) {
			changed, runErr := e.executeScheduledAction(ctx, config, account, secret, "stop", now)
			if runErr != nil {
				return "", runErr
			}
			if changed {
				actions = append(actions, "scheduled_stop")
				account.InstanceStatus = domain.StatusStopping
				statusChangedBySchedule = true
			}
		}
	}

	interval := time.Duration(config.APIInterval) * time.Second
	if transient(account.InstanceStatus) {
		interval = time.Minute
	}
	due := force || account.UpdatedAt.IsZero() || time.Since(account.UpdatedAt) >= interval || now.Minute() == 0 || statusChangedBySchedule
	traffic, status := account.TrafficUsed, account.InstanceStatus
	if due {
		var trafficErr, statusErr error
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			traffic, trafficErr = e.provider.GetTraffic(ctx, account, secret)
		}()
		go func() {
			defer wait.Done()
			status, statusErr = e.provider.GetInstanceStatus(ctx, account, secret)
		}()
		wait.Wait()
		if trafficErr != nil {
			traffic = account.TrafficUsed
			_ = e.store.AddLog(ctx, "error", fmt.Sprintf("流量查询失败 [%s]: %v", masked(account.AccessKeyID), trafficErr))
		}
		if statusErr != nil || status == "" {
			status = account.InstanceStatus
			_ = e.store.AddLog(ctx, "error", fmt.Sprintf("实例状态查询失败 [%s]: %v", masked(account.AccessKeyID), statusErr))
		}
		updatedAt := time.Now().UTC()
		if trafficErr != nil && statusErr != nil {
			updatedAt = account.UpdatedAt
		}
		if statusChangedBySchedule {
			if slices.Contains(actions, "scheduled_start") {
				status = domain.StatusStarting
			} else if slices.Contains(actions, "scheduled_stop") {
				status = domain.StatusStopping
			}
		}
		if err = e.store.UpdateRuntime(ctx, account.ID, traffic, status, updatedAt); err != nil {
			return "", err
		}
		if trafficErr == nil {
			_ = e.store.AddTrafficStats(ctx, account.ID, traffic, now)
		}
	}

	percentage := usagePercent(traffic, account.MaxTraffic)
	overThreshold := percentage >= float64(config.TrafficThreshold)
	thresholdKey := fmt.Sprintf("threshold:%d:active", account.ID)
	if !overThreshold {
		_ = e.store.DeleteActionEvent(ctx, thresholdKey)
	}
	if overThreshold && due {
		key := thresholdKey
		recorded, recordErr := e.store.RecordActionEvent(ctx, key, account.ID, "threshold", "detected", fmt.Sprintf("%.2f%%", percentage))
		if recordErr != nil {
			return "", recordErr
		}
		if recorded {
			if config.ThresholdAction == "stop_and_notify" && status != domain.StatusStopped && status != domain.StatusStopping {
				if err = e.provider.ControlInstance(ctx, account, secret, "stop", config.ShutdownMode); err != nil {
					_ = e.store.DeleteActionEvent(ctx, key)
					return "", err
				}
				status = domain.StatusStopping
				_ = e.store.UpdateRuntime(ctx, account.ID, traffic, status, time.Now().UTC())
				actions = append(actions, "threshold_stop")
			}
			event := newEvent("threshold", "流量阈值告警", fmt.Sprintf("账号 %s 的流量使用率达到 %.2f%%。", masked(account.AccessKeyID), percentage), account.ID, map[string]string{
				"当前流量": fmt.Sprintf("%.2f GB", traffic), "设定阈值": fmt.Sprintf("%d%%", config.TrafficThreshold), "实例状态": status,
			})
			_ = e.store.AddOutbox(ctx, event, notify.EnabledChannels(config))
			_ = e.store.AddLog(ctx, "warning", event.Summary)
		}
	}

	if config.KeepAlive && !overThreshold && !statusChangedBySchedule && status == domain.StatusStopped && (!account.ScheduleEnabled || inTimeRange(now.Format("15:04"), account.StartTime, account.StopTime)) {
		key := fmt.Sprintf("keepalive:%d:%s", account.ID, now.Format("200601021504"))
		fresh, recordErr := e.store.RecordActionEvent(ctx, key, account.ID, "keepalive", "attempting", "")
		if recordErr != nil {
			return "", recordErr
		}
		if fresh {
			if err = e.provider.ControlInstance(ctx, account, secret, "start", config.ShutdownMode); err != nil {
				_ = e.store.DeleteActionEvent(ctx, key)
				return "", err
			}
			status = domain.StatusStarting
			_ = e.store.UpdateRuntime(ctx, account.ID, traffic, status, time.Now().UTC())
			_ = e.store.UpdateKeepAliveAt(ctx, account.ID, time.Now().UTC())
			actions = append(actions, "keepalive_start")
			event := newEvent("keepalive", "实例保活启动", "检测到实例在允许运行时段意外停止，已发送启动指令。", account.ID, map[string]string{"账号": masked(account.AccessKeyID), "实例": account.InstanceID})
			_ = e.store.AddOutbox(ctx, event, notify.EnabledChannels(config))
		}
	}

	if config.EnableBilling {
		var balance aliyun.BillingBalance
		balanceCached, _ := e.store.BillingCache(ctx, account.ID, "balance", "", 6*time.Hour, &balance)
		billCached := true
		if account.InstanceID != "" {
			var bill aliyun.BillingBill
			billCached, _ = e.store.BillingCache(ctx, account.ID, "instance_bill", now.Format("2006-01"), 6*time.Hour, &bill)
		}
		if force || now.Hour()%6 == 0 || !balanceCached || !billCached {
			if billingErr := e.refreshBilling(ctx, account, secret, now); billingErr != nil {
				_ = e.store.AddLog(ctx, "error", fmt.Sprintf("账单查询失败 [%s]: %v", masked(account.AccessKeyID), billingErr))
			}
		}
	}
	message := fmt.Sprintf("[%s] 流量 %.2fGB / %.2fGB (%.2f%%) · 状态 %s", masked(account.AccessKeyID), traffic, account.MaxTraffic, percentage, status)
	if len(actions) > 0 {
		message += " · 动作 " + strings.Join(actions, ",")
	}
	_ = e.store.AddLog(ctx, "heartbeat", message)
	return message, nil
}

func (e *Engine) executeScheduledAction(ctx context.Context, config domain.Config, account domain.Account, secret, action string, now time.Time) (bool, error) {
	key := fmt.Sprintf("schedule:%d:%s:%s", account.ID, now.Format("20060102"), action)
	fresh, err := e.store.RecordActionEvent(ctx, key, account.ID, "schedule_"+action, "attempting", "")
	if err != nil || !fresh {
		return false, err
	}
	if err = e.provider.ControlInstance(ctx, account, secret, action, config.ShutdownMode); err != nil {
		_ = e.store.DeleteActionEvent(ctx, key)
		return false, err
	}
	status := domain.StatusStarting
	if action == "stop" {
		status = domain.StatusStopping
	}
	_ = e.store.UpdateRuntime(ctx, account.ID, account.TrafficUsed, status, time.Now().UTC())
	_ = e.store.AddLog(ctx, "info", fmt.Sprintf("执行定时%s [%s]", map[string]string{"start": "开机", "stop": "关机"}[action], masked(account.AccessKeyID)))
	if config.EnableScheduleMail {
		event := newEvent("schedule", "定时任务已执行", fmt.Sprintf("实例定时%s指令已发送。", map[string]string{"start": "开机", "stop": "关机"}[action]), account.ID, map[string]string{"账号": masked(account.AccessKeyID), "实例": account.InstanceID})
		_ = e.store.AddOutbox(ctx, event, notify.EnabledChannels(config))
	}
	return true, nil
}

func (e *Engine) control(ctx context.Context, accountID int64, action, source string) (string, error) {
	lock := e.accountLock(accountID)
	lock.Lock()
	defer lock.Unlock()
	account, err := e.store.GetAccount(ctx, accountID)
	if err != nil {
		return "", err
	}
	config, err := e.store.GetConfig(ctx)
	if err != nil {
		return "", err
	}
	action = strings.ToLower(action)
	if action != "start" && action != "stop" {
		return "", errors.New("action must be start or stop")
	}
	if transient(account.InstanceStatus) {
		return "", fmt.Errorf("instance is currently %s", account.InstanceStatus)
	}
	if config.KeepAlive && action == "stop" {
		return "", errors.New("manual shutdown is disabled while keep-alive is enabled")
	}
	secret, err := e.store.AccountSecret(ctx, accountID)
	if err != nil {
		return "", err
	}
	if err = e.provider.ControlInstance(ctx, account, secret, action, config.ShutdownMode); err != nil {
		return "", err
	}
	status := domain.StatusStarting
	if action == "stop" {
		status = domain.StatusStopping
	}
	if err = e.store.UpdateRuntime(ctx, account.ID, account.TrafficUsed, status, time.Now().UTC()); err != nil {
		return "", err
	}
	message := fmt.Sprintf("%s控制实例 [%s]：%s", source, masked(account.AccessKeyID), action)
	_ = e.store.AddLog(ctx, "audit", message)
	return message, nil
}

func (e *Engine) accountLock(accountID int64) *sync.Mutex {
	value, _ := e.accountLocks.LoadOrStore(accountID, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func (e *Engine) refreshBilling(ctx context.Context, account domain.Account, secret string, now time.Time) error {
	cycle := now.Format("2006-01")
	setBillingError := func(err error) {
		_ = e.store.SetBillingCache(ctx, account.ID, "error", "", map[string]string{"message": err.Error()})
	}
	var balance aliyun.BillingBalance
	cached, _ := e.store.BillingCache(ctx, account.ID, "balance", "", 6*time.Hour, &balance)
	if !cached {
		value, err := e.provider.GetAccountBalance(ctx, account, secret)
		if err != nil {
			setBillingError(err)
			return err
		}
		balance = value
		_ = e.store.SetBillingCache(ctx, account.ID, "balance", "", balance)
	}
	if account.InstanceID != "" {
		var bill aliyun.BillingBill
		cached, _ = e.store.BillingCache(ctx, account.ID, "instance_bill", cycle, 6*time.Hour, &bill)
		if !cached {
			value, err := e.provider.GetInstanceBill(ctx, account, secret, cycle)
			if err != nil {
				setBillingError(err)
				return err
			}
			_ = e.store.SetBillingCache(ctx, account.ID, "instance_bill", cycle, value)
		}
	}
	_ = e.store.SetBillingCache(ctx, account.ID, "error", "", map[string]string{"message": ""})
	return nil
}

func (e *Engine) notificationWorker(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for {
				item, err := e.store.ClaimOutbox(ctx)
				if errors.Is(err, sql.ErrNoRows) {
					break
				}
				if err != nil {
					e.logger.Error("claim notification", "error", err)
					break
				}
				var event domain.NotificationEvent
				if err = json.Unmarshal([]byte(item.Payload), &event); err == nil {
					var config domain.Config
					config, err = e.store.GetConfig(ctx)
					if err == nil {
						sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
						err = e.notify.Send(sendCtx, item.Channel, event, config)
						cancel()
					}
				}
				if err != nil {
					_ = e.store.FailOutbox(ctx, item, err)
					continue
				}
				_ = e.store.CompleteOutbox(ctx, item.ID)
			}
		}
	}
}

func (e *Engine) enqueueDailyReportIfDue(ctx context.Context, tick time.Time) error {
	config, err := e.store.GetConfig(ctx)
	if err != nil {
		return err
	}
	if !config.EnableDailyReport {
		return nil
	}
	location, err := time.LoadLocation(config.Timezone)
	if err != nil {
		location = time.FixedZone("CST", 8*3600)
	}
	now := tick.In(location)
	if !dueWithin(now, config.DailyReportTime, 10*time.Minute) {
		return nil
	}
	key := "daily_report:" + now.Format("20060102")
	fresh, err := e.store.RecordActionEvent(ctx, key, 0, "daily_report", "queued", "")
	if err != nil || !fresh {
		return err
	}
	event, err := e.dailyReportEvent(ctx, config, now)
	if err != nil {
		_ = e.store.DeleteActionEvent(ctx, key)
		return err
	}
	if err = e.store.AddOutbox(ctx, event, notify.EnabledChannels(config)); err != nil {
		_ = e.store.DeleteActionEvent(ctx, key)
		return err
	}
	_ = e.store.AddLog(ctx, "info", "每日流量报告已生成")
	return nil
}

func (e *Engine) dailyReportEvent(ctx context.Context, config domain.Config, now time.Time) (domain.NotificationEvent, error) {
	text, fields, summary, err := e.buildDailyReport(ctx, config, now)
	if err != nil {
		return domain.NotificationEvent{}, err
	}
	event := newEvent("daily_report", "每日流量报告", summary, 0, fields)
	event.Text = text
	return event, nil
}

func (e *Engine) dailyReportText(ctx context.Context, config domain.Config, now time.Time) (string, error) {
	text, _, _, err := e.buildDailyReport(ctx, config, now)
	return text, err
}

func (e *Engine) buildDailyReport(ctx context.Context, config domain.Config, now time.Time) (string, map[string]string, string, error) {
	summaries, lastRun, err := e.Summary(ctx)
	if err != nil {
		return "", nil, "", err
	}
	fields := make(map[string]string, len(summaries)+4)
	totalUsed, totalLimit, totalToday := 0.0, 0.0, 0.0
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	yesterdayStart := dayStart.AddDate(0, 0, -1)
	type reportRow struct {
		Account domain.AccountSummary
		Today   float64
	}
	rows := make([]reportRow, 0, len(summaries))
	for _, account := range summaries {
		yesterdayEnd, ok, err := e.store.PreviousDailyTraffic(ctx, account.ID, dayStart)
		if err != nil {
			return "", nil, "", err
		}
		previousEnd, previousOK, err := e.store.PreviousDailyTraffic(ctx, account.ID, yesterdayStart)
		if err != nil {
			return "", nil, "", err
		}
		yesterday := 0.0
		if ok && previousOK {
			if yesterdayEnd >= previousEnd {
				yesterday = yesterdayEnd - previousEnd
			} else {
				yesterday = yesterdayEnd
			}
		}
		totalUsed += account.FlowUsed
		totalLimit += account.FlowTotal
		totalToday += yesterday
		name := reportCardTitle(account)
		fields[fmt.Sprintf("#%d %s", account.ID, name)] = fmt.Sprintf("昨日 +%.2f GB / 已用 %.2f GB / 剩余 %.2f GB / %.2f%% / %s", yesterday, account.FlowUsed, maxFloat(account.FlowTotal-account.FlowUsed, 0), account.Percentage, account.InstanceStatus)
		rows = append(rows, reportRow{Account: account, Today: yesterday})
	}
	percent := usagePercent(totalUsed, totalLimit)
	fields["昨日总增量"] = fmt.Sprintf("%.2f GB", totalToday)
	fields["累计总流量"] = fmt.Sprintf("%.2f GB / %.2f GB (%.2f%%)", totalUsed, totalLimit, percent)
	if !lastRun.IsZero() {
		fields["最近同步"] = lastRun.In(now.Location()).Format("2006-01-02 15:04:05")
	}
	summary := fmt.Sprintf("%s CDT 流量日报：昨日 +%.2f GB，累计 %.2f / %.2f GB。", now.Format("2006-01-02"), totalToday, totalUsed, totalLimit)
	var builder strings.Builder
	builder.WriteString("📊 CDT 每日流量报告\n")
	builder.WriteString("时间：")
	builder.WriteString(now.Format("2006-01-02 15:04"))
	builder.WriteString(" (")
	builder.WriteString(config.Timezone)
	builder.WriteString(")\n\n")
	builder.WriteString(fmt.Sprintf("昨日总增量：+%.2f GB\n", totalToday))
	builder.WriteString(fmt.Sprintf("累计总流量：%.2f / %.2f GB (%.2f%%)\n", totalUsed, totalLimit, percent))
	if !lastRun.IsZero() {
		builder.WriteString("最近同步：")
		builder.WriteString(lastRun.In(now.Location()).Format("2006-01-02 15:04:05"))
		builder.WriteString("\n")
	}
	if len(rows) == 0 {
		builder.WriteString("\n暂无实例。")
		return builder.String(), fields, summary, nil
	}
	for index, item := range rows {
		account := item.Account
		name := reportCardTitle(account)
		remaining := account.FlowTotal - account.FlowUsed
		if remaining < 0 {
			remaining = 0
		}
		builder.WriteString("\n")
		builder.WriteString(name)
		builder.WriteString("\n")
		builder.WriteString(fmt.Sprintf("📦 实例：%s\n", appendStar(firstNonEmptyText(account.Remark, account.Account, strconv.FormatInt(account.ID, 10)))))
		builder.WriteString(fmt.Sprintf("🌐 公网IP：%s\n", appendStar("未获取")))
		builder.WriteString(fmt.Sprintf("📍 地域：%s\n", firstNonEmptyText(account.RegionName, account.Region)))
		builder.WriteString(fmt.Sprintf("📈 昨日流量：+%.2f GB\n", item.Today))
		builder.WriteString(fmt.Sprintf("🗂 已用：%.2f GB\n", account.FlowUsed))
		builder.WriteString(fmt.Sprintf("📁 剩余：%.2f GB\n", remaining))
		builder.WriteString(fmt.Sprintf("🔥 使用率：%.2f%% %s\n", account.Percentage, trafficHealthLabel(account)))
		builder.WriteString(fmt.Sprintf("🕒 状态：%s", statusLabel(account.InstanceStatus)))
		if index < len(rows)-1 {
			builder.WriteString("\n")
		}
	}
	return builder.String(), fields, summary, nil
}

func (e *Engine) telegramBotWorker(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := e.pollTelegramBot(ctx); err != nil && !errors.Is(err, context.Canceled) {
			e.logger.Warn("telegram bot polling skipped", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (e *Engine) pollTelegramBot(ctx context.Context) error {
	config, err := e.store.GetConfig(ctx)
	if err != nil {
		return err
	}
	tg := config.Notifications.Telegram
	if !tg.Enabled || tg.Token == "" || tg.ChatID == "" {
		return nil
	}
	acquired, err := e.store.AcquireLease(ctx, "telegram_bot", e.owner, 45*time.Second)
	if err != nil || !acquired {
		return err
	}
	e.telegramMu.Lock()
	offset := e.telegramNext
	e.telegramMu.Unlock()
	if err = e.ensureTelegramCommands(ctx, tg); err != nil {
		e.logger.Warn("telegram bot commands skipped", "error", err)
	}
	pollCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	updates, err := e.notify.PollTelegramUpdates(pollCtx, tg, offset)
	cancel()
	if err != nil {
		return err
	}
	for _, update := range updates {
		if update.UpdateID >= offset {
			offset = update.UpdateID + 1
		}
		if update.ChatID != tg.ChatID {
			continue
		}
		if update.CallbackID != "" {
			_ = e.notify.AnswerCallbackQuery(ctx, tg, update.CallbackID, "")
			response := e.handleTelegramCallback(ctx, config, update.ChatID, update.CallbackData)
			if response.Text != "" {
				if err := e.notify.EditTelegramMessage(ctx, tg, update.ChatID, update.MessageID, response.Text, response.Keyboard); err != nil {
					if !strings.Contains(err.Error(), "message is not modified") {
						_ = e.notify.SendTelegramMessage(ctx, tg, update.ChatID, response.Text, response.Keyboard)
					}
				}
			}
			continue
		}
		if response := e.handleTelegramSession(ctx, config, update.ChatID, update.Text); response.Text != "" {
			_ = e.notify.SendTelegramMessage(ctx, tg, update.ChatID, response.Text, response.Keyboard)
			continue
		}
		parts := strings.Fields(update.Text)
		if len(parts) == 0 {
			continue
		}
		command := strings.ToLower(parts[0])
		if at := strings.Index(command, "@"); at >= 0 {
			command = command[:at]
		}
		response := e.handleTelegramCommandReply(ctx, config, update.ChatID, command, parts[1:])
		if response.Text != "" {
			_ = e.notify.SendTelegramMessage(ctx, tg, update.ChatID, response.Text, response.Keyboard)
		}
	}
	e.telegramMu.Lock()
	if offset > e.telegramNext {
		e.telegramNext = offset
	}
	e.telegramMu.Unlock()
	return nil
}

func (e *Engine) handleTelegramCommand(ctx context.Context, config domain.Config, command string, args []string) string {
	location, locErr := time.LoadLocation(config.Timezone)
	if locErr != nil {
		location = time.FixedZone("CST", 8*3600)
	}
	switch command {
	case "/report", "/daily", "/today":
		text, err := e.dailyReportText(ctx, config, time.Now().In(location))
		if err != nil {
			return "生成每日流量报告失败：" + err.Error()
		}
		return text
	case "/status":
		text, err := e.telegramStatusText(ctx, config)
		if err != nil {
			return "获取状态失败：" + err.Error()
		}
		return text
	case "/refresh":
		if len(args) == 0 {
			jobs, err := e.EnqueueRefreshAll(ctx)
			if err != nil {
				return "刷新任务创建失败：" + err.Error()
			}
			if len(jobs) == 0 {
				return "暂无可刷新的实例。"
			}
			return fmt.Sprintf("已创建全部实例刷新任务，共 %d 个。", len(jobs))
		}
		accountID, err := parseTelegramAccountID(args[0])
		if err != nil {
			return "用法：/refresh <实例ID>"
		}
		job, err := e.Enqueue(ctx, JobRefreshAccount, accountID, `{}`, JobUniqueKey(JobRefreshAccount, accountID, time.Now().UTC().Format("200601021504")))
		if err != nil {
			return "刷新任务创建失败：" + err.Error()
		}
		return fmt.Sprintf("已创建实例 #%d 刷新任务：%s", accountID, job.ID)
	case "/startvm", "/boot":
		return e.enqueueTelegramControl(ctx, config, args, "start")
	case "/stopvm", "/shutdown":
		return e.enqueueTelegramControl(ctx, config, args, "stop")
	case "/start", "/help":
		return telegramHelpText()
	default:
		if strings.HasPrefix(command, "/") {
			return "未知命令。\n\n" + telegramHelpText()
		}
		return ""
	}
}

func (e *Engine) ensureTelegramCommands(ctx context.Context, config domain.TelegramConfig) error {
	key := strings.Join([]string{config.Token, config.ProxyType, config.ProxyURL, config.ProxyIP, config.ProxyPort}, "|")
	e.telegramMu.Lock()
	if e.telegramBotKey == key {
		e.telegramMu.Unlock()
		return nil
	}
	if !e.telegramBotCommandTry.IsZero() && time.Since(e.telegramBotCommandTry) < time.Hour {
		e.telegramMu.Unlock()
		return nil
	}
	e.telegramBotCommandTry = time.Now()
	e.telegramMu.Unlock()
	commands := []notify.TelegramBotCommand{
		{Command: "start", Description: "打开控制菜单"},
		{Command: "status", Description: "查看实例与流量状态"},
		{Command: "report", Description: "获取今日流量报告"},
		{Command: "refresh", Description: "刷新全部或指定实例"},
		{Command: "accounts", Description: "查看实例列表"},
		{Command: "settings", Description: "查看和修改面板设置"},
		{Command: "add", Description: "在 Bot 中添加实例"},
		{Command: "cancel", Description: "取消当前流程"},
		{Command: "help", Description: "查看帮助"},
	}
	if err := e.notify.SetTelegramCommands(ctx, config, commands); err != nil {
		return err
	}
	e.telegramMu.Lock()
	e.telegramBotKey = key
	e.telegramBotCommandTry = time.Time{}
	e.telegramMu.Unlock()
	return nil
}

func (e *Engine) handleTelegramCommandReply(ctx context.Context, config domain.Config, chatID, command string, args []string) telegramReply {
	location, locErr := time.LoadLocation(config.Timezone)
	if locErr != nil {
		location = time.FixedZone("CST", 8*3600)
	}
	switch command {
	case "/report", "/daily", "/today":
		text, err := e.dailyReportText(ctx, config, time.Now().In(location))
		if err != nil {
			return telegramReply{Text: "生成每日流量报告失败：" + err.Error()}
		}
		return telegramReply{Text: text, Keyboard: telegramReportKeyboard()}
	case "/status":
		text, err := e.telegramStatusPretty(ctx, config)
		if err != nil {
			return telegramReply{Text: "获取状态失败：" + err.Error()}
		}
		return telegramReply{Text: text, Keyboard: telegramMainKeyboard()}
	case "/refresh":
		if len(args) == 0 {
			jobs, err := e.EnqueueRefreshAll(ctx)
			if err != nil {
				return telegramReply{Text: "刷新任务创建失败：" + err.Error()}
			}
			if len(jobs) == 0 {
				return telegramReply{Text: "暂无可刷新的实例。", Keyboard: telegramMainKeyboard()}
			}
			return telegramReply{Text: fmt.Sprintf("已创建全部实例刷新任务，共 %d 个。", len(jobs)), Keyboard: telegramMainKeyboard()}
		}
		accountID, err := parseTelegramAccountID(args[0])
		if err != nil {
			return telegramReply{Text: "用法：/refresh <实例ID>"}
		}
		job, err := e.Enqueue(ctx, JobRefreshAccount, accountID, `{}`, JobUniqueKey(JobRefreshAccount, accountID, time.Now().UTC().Format("200601021504")))
		if err != nil {
			return telegramReply{Text: "刷新任务创建失败：" + err.Error()}
		}
		return telegramReply{Text: fmt.Sprintf("已创建实例 #%d 刷新任务：%s", accountID, job.ID)}
	case "/startvm", "/boot":
		return telegramReply{Text: e.enqueueTelegramControl(ctx, config, args, "start")}
	case "/stopvm", "/shutdown":
		return telegramReply{Text: e.enqueueTelegramControl(ctx, config, args, "stop")}
	case "/accounts":
		return e.telegramAccountsReply(ctx)
	case "/settings":
		return telegramReply{Text: telegramSettingsText(config), Keyboard: telegramSettingsKeyboard(config)}
	case "/add":
		return e.startTelegramAddAccount(chatID)
	case "/cancel":
		e.telegramChats.Delete(chatID)
		return telegramReply{Text: "已取消当前操作。", Keyboard: telegramMainKeyboard()}
	case "/start":
		return telegramReply{Text: "CDT Monitor 控制菜单", Keyboard: telegramMainKeyboard()}
	case "/help":
		return telegramReply{Text: telegramHelpText(), Keyboard: telegramMainKeyboard()}
	default:
		if strings.HasPrefix(command, "/") {
			return telegramReply{Text: "未知命令。\n\n" + telegramHelpText(), Keyboard: telegramMainKeyboard()}
		}
		return telegramReply{}
	}
}

func (e *Engine) handleTelegramCallback(ctx context.Context, config domain.Config, chatID, data string) telegramReply {
	switch data {
	case "menu":
		return telegramReply{Text: "CDT Monitor 控制菜单", Keyboard: telegramMainKeyboard()}
	case "status":
		text, err := e.telegramStatusPretty(ctx, config)
		if err != nil {
			return telegramReply{Text: "获取状态失败：" + err.Error()}
		}
		return telegramReply{Text: text, Keyboard: telegramMainKeyboard()}
	case "report":
		location, locErr := time.LoadLocation(config.Timezone)
		if locErr != nil {
			location = time.FixedZone("CST", 8*3600)
		}
		text, err := e.dailyReportText(ctx, config, time.Now().In(location))
		if err != nil {
			return telegramReply{Text: "生成每日流量报告失败：" + err.Error()}
		}
		return telegramReply{Text: text, Keyboard: telegramReportKeyboard()}
	case "refresh_all":
		jobs, err := e.EnqueueRefreshAll(ctx)
		if err != nil {
			return telegramReply{Text: "刷新任务创建失败：" + err.Error()}
		}
		return telegramReply{Text: fmt.Sprintf("已创建全部实例刷新任务，共 %d 个。", len(jobs)), Keyboard: telegramMainKeyboard()}
	case "accounts":
		return e.telegramAccountsReply(ctx)
	case "add_account":
		return e.startTelegramAddAccount(chatID)
	case "settings":
		return telegramReply{Text: telegramSettingsText(config), Keyboard: telegramSettingsKeyboard(config)}
	case "set:daily:on":
		config.EnableDailyReport = true
		return e.saveTelegramConfig(ctx, config, "每日流量报告已开启。")
	case "set:daily:off":
		config.EnableDailyReport = false
		return e.saveTelegramConfig(ctx, config, "每日流量报告已关闭。")
	case "set:keepalive:on":
		config.KeepAlive = true
		return e.saveTelegramConfig(ctx, config, "抢占式实例保活已开启。")
	case "set:keepalive:off":
		config.KeepAlive = false
		return e.saveTelegramConfig(ctx, config, "抢占式实例保活已关闭。")
	case "set:billing:on":
		config.EnableBilling = true
		return e.saveTelegramConfig(ctx, config, "账单与余额已开启。")
	case "set:billing:off":
		config.EnableBilling = false
		return e.saveTelegramConfig(ctx, config, "账单与余额已关闭。")
	case "field:daily_time":
		e.telegramChats.Store(chatID, telegramChatSession{Mode: "setting", Field: "daily_time", Updated: time.Now()})
		return telegramReply{Text: "请输入日报时间，格式 HH:MM，例如 23:59。\n发送 /cancel 可取消。"}
	case "field:threshold":
		e.telegramChats.Store(chatID, telegramChatSession{Mode: "setting", Field: "threshold", Updated: time.Now()})
		return telegramReply{Text: "请输入告警阈值 1-100，例如 95。\n发送 /cancel 可取消。"}
	case "field:api_interval":
		e.telegramChats.Store(chatID, telegramChatSession{Mode: "setting", Field: "api_interval", Updated: time.Now()})
		return telegramReply{Text: "请输入 API 刷新间隔秒数，范围 30-86400，例如 300。\n发送 /cancel 可取消。"}
	case "field:timezone":
		e.telegramChats.Store(chatID, telegramChatSession{Mode: "setting", Field: "timezone", Updated: time.Now()})
		return telegramReply{Text: "请输入系统时区，例如 Asia/Shanghai。\n发送 /cancel 可取消。"}
	}
	if strings.HasPrefix(data, "account:") {
		id, err := parseTelegramAccountID(strings.TrimPrefix(data, "account:"))
		if err != nil {
			return telegramReply{Text: "实例 ID 无效。"}
		}
		return e.telegramAccountDetail(ctx, id)
	}
	if strings.HasPrefix(data, "refresh:") {
		id, err := parseTelegramAccountID(strings.TrimPrefix(data, "refresh:"))
		if err != nil {
			return telegramReply{Text: "实例 ID 无效。"}
		}
		job, err := e.Enqueue(ctx, JobRefreshAccount, id, `{}`, JobUniqueKey(JobRefreshAccount, id, time.Now().UTC().Format("200601021504")))
		if err != nil {
			return telegramReply{Text: "刷新任务创建失败：" + err.Error()}
		}
		return telegramReply{Text: fmt.Sprintf("已创建实例 #%d 刷新任务：%s", id, job.ID), Keyboard: telegramAccountKeyboard(id)}
	}
	if strings.HasPrefix(data, "start:") || strings.HasPrefix(data, "stop:") {
		action := "start"
		idText := strings.TrimPrefix(data, "start:")
		if strings.HasPrefix(data, "stop:") {
			action = "stop"
			idText = strings.TrimPrefix(data, "stop:")
		}
		return telegramReply{Text: e.enqueueTelegramControl(ctx, config, []string{idText}, action), Keyboard: telegramAccountKeyboardFromText(idText)}
	}
	return telegramReply{Text: "未知按钮。", Keyboard: telegramMainKeyboard()}
}

func (e *Engine) handleTelegramSession(ctx context.Context, config domain.Config, chatID, text string) telegramReply {
	text = strings.TrimSpace(text)
	if strings.EqualFold(text, "/cancel") {
		e.telegramChats.Delete(chatID)
		return telegramReply{Text: "已取消当前操作。", Keyboard: telegramMainKeyboard()}
	}
	value, ok := e.telegramChats.Load(chatID)
	if !ok {
		return telegramReply{}
	}
	session := value.(telegramChatSession)
	if time.Since(session.Updated) > 30*time.Minute {
		e.telegramChats.Delete(chatID)
		return telegramReply{Text: "上一次操作已超时，请重新开始。", Keyboard: telegramMainKeyboard()}
	}
	session.Updated = time.Now()
	if session.Mode == "setting" {
		return e.applyTelegramSetting(ctx, config, chatID, session.Field, text)
	}
	if session.Mode == "add_account" {
		return e.handleTelegramAddAccountStep(ctx, config, chatID, session, text)
	}
	return telegramReply{}
}

func (e *Engine) startTelegramAddAccount(chatID string) telegramReply {
	e.telegramChats.Store(chatID, telegramChatSession{Mode: "add_account", Step: "access_key_id", Draft: domain.Account{MaxTraffic: 200, SiteType: "china", StartTime: "09:00", StopTime: "23:00"}, Updated: time.Now()})
	return telegramReply{Text: "开始添加实例。\n请发送 AccessKey ID。\n发送 /cancel 可取消。"}
}

func (e *Engine) handleTelegramAddAccountStep(ctx context.Context, config domain.Config, chatID string, session telegramChatSession, text string) telegramReply {
	switch session.Step {
	case "access_key_id":
		session.Draft.AccessKeyID = text
		session.Step = "access_key_secret"
		e.telegramChats.Store(chatID, session)
		return telegramReply{Text: "请发送 AccessKey Secret。"}
	case "access_key_secret":
		session.Draft.AccessKeySecret = text
		session.Step = "region_id"
		e.telegramChats.Store(chatID, session)
		return telegramReply{Text: "请发送区域 ID，例如 cn-hongkong。"}
	case "region_id":
		session.Draft.RegionID = text
		session.Step = "instance_id"
		e.telegramChats.Store(chatID, session)
		return telegramReply{Text: "请发送实例 ID。"}
	case "instance_id":
		session.Draft.InstanceID = text
		session.Step = "max_traffic"
		e.telegramChats.Store(chatID, session)
		return telegramReply{Text: "请发送月流量额度 GB，例如 200。"}
	case "max_traffic":
		traffic, err := strconv.ParseFloat(text, 64)
		if err != nil || traffic <= 0 {
			return telegramReply{Text: "流量额度需要是大于 0 的数字，例如 200。"}
		}
		session.Draft.MaxTraffic = traffic
		session.Step = "site_type"
		e.telegramChats.Store(chatID, session)
		return telegramReply{Text: "请选择站点类型：china 或 international。"}
	case "site_type":
		siteType := strings.ToLower(text)
		if siteType != "international" {
			siteType = "china"
		}
		session.Draft.SiteType = siteType
		session.Step = "remark"
		e.telegramChats.Store(chatID, session)
		return telegramReply{Text: "请发送备注名；不需要备注可发送 -。"}
	case "remark":
		if text != "-" {
			session.Draft.Remark = text
		}
		config.Accounts = append(config.Accounts, session.Draft)
		if err := e.store.SaveConfig(ctx, config); err != nil {
			return telegramReply{Text: "保存实例失败：" + err.Error()}
		}
		e.telegramChats.Delete(chatID)
		return telegramReply{Text: "实例已添加，稍后可刷新同步状态。", Keyboard: telegramMainKeyboard()}
	default:
		e.telegramChats.Delete(chatID)
		return telegramReply{Text: "添加流程状态异常，请重新开始。", Keyboard: telegramMainKeyboard()}
	}
}

func (e *Engine) applyTelegramSetting(ctx context.Context, config domain.Config, chatID, field, text string) telegramReply {
	switch field {
	case "daily_time":
		if _, err := time.Parse("15:04", text); err != nil {
			return telegramReply{Text: "时间格式不正确，请输入 HH:MM，例如 23:59。"}
		}
		config.DailyReportTime = text
	case "threshold":
		value, err := strconv.Atoi(text)
		if err != nil || value < 1 || value > 100 {
			return telegramReply{Text: "告警阈值需要是 1-100 的整数。"}
		}
		config.TrafficThreshold = value
	case "api_interval":
		value, err := strconv.Atoi(text)
		if err != nil || value < 30 || value > 86400 {
			return telegramReply{Text: "API 刷新间隔需要是 30-86400 秒。"}
		}
		config.APIInterval = value
	case "timezone":
		if _, err := time.LoadLocation(text); err != nil {
			return telegramReply{Text: "时区无效，请输入类似 Asia/Shanghai 的 IANA 时区。"}
		}
		config.Timezone = text
	default:
		e.telegramChats.Delete(chatID)
		return telegramReply{Text: "未知设置项。", Keyboard: telegramSettingsKeyboard(config)}
	}
	if err := e.store.SaveConfig(ctx, config); err != nil {
		return telegramReply{Text: "保存设置失败：" + err.Error()}
	}
	e.telegramChats.Delete(chatID)
	return telegramReply{Text: "设置已保存。\n\n" + telegramSettingsText(config), Keyboard: telegramSettingsKeyboard(config)}
}

func (e *Engine) saveTelegramConfig(ctx context.Context, config domain.Config, text string) telegramReply {
	if err := e.store.SaveConfig(ctx, config); err != nil {
		return telegramReply{Text: "保存设置失败：" + err.Error()}
	}
	return telegramReply{Text: text + "\n\n" + telegramSettingsText(config), Keyboard: telegramSettingsKeyboard(config)}
}

func (e *Engine) telegramAccountsReply(ctx context.Context) telegramReply {
	summaries, _, err := e.Summary(ctx)
	if err != nil {
		return telegramReply{Text: "获取实例列表失败：" + err.Error()}
	}
	if len(summaries) == 0 {
		return telegramReply{Text: "暂无实例。", Keyboard: telegramMainKeyboard()}
	}
	var builder strings.Builder
	builder.WriteString("实例列表\n")
	for _, account := range summaries {
		name := firstNonEmptyText(account.Remark, account.Account, strconv.FormatInt(account.ID, 10))
		builder.WriteString(fmt.Sprintf("\n#%d %s\n%.2f / %.2f GB (%.2f%%) · %s", account.ID, name, account.FlowUsed, account.FlowTotal, account.Percentage, statusLabel(account.InstanceStatus)))
	}
	return telegramReply{Text: builder.String(), Keyboard: telegramAccountsKeyboard(summaries)}
}

func (e *Engine) telegramAccountDetail(ctx context.Context, id int64) telegramReply {
	summaries, _, err := e.Summary(ctx)
	if err != nil {
		return telegramReply{Text: "获取实例失败：" + err.Error()}
	}
	for _, account := range summaries {
		if account.ID == id {
			name := firstNonEmptyText(account.Remark, account.Account, strconv.FormatInt(account.ID, 10))
			text := fmt.Sprintf("#%d %s\n区域：%s\n流量：%.2f / %.2f GB (%.2f%%)\n状态：%s", account.ID, name, firstNonEmptyText(account.RegionName, account.Region), account.FlowUsed, account.FlowTotal, account.Percentage, statusLabel(account.InstanceStatus))
			return telegramReply{Text: text, Keyboard: telegramAccountKeyboard(id)}
		}
	}
	return telegramReply{Text: fmt.Sprintf("未找到实例 #%d。", id), Keyboard: telegramMainKeyboard()}
}

func (e *Engine) telegramStatusPretty(ctx context.Context, config domain.Config) (string, error) {
	summaries, lastRun, err := e.Summary(ctx)
	if err != nil {
		return "", err
	}
	running, warning, used, total := 0, 0, 0.0, 0.0
	for _, account := range summaries {
		if account.InstanceStatus == domain.StatusRunning {
			running++
		}
		if account.OverThreshold {
			warning++
		}
		used += account.FlowUsed
		total += account.FlowTotal
	}
	var builder strings.Builder
	builder.WriteString("CDT Monitor 状态\n")
	builder.WriteString(fmt.Sprintf("实例：%d 台，运行中：%d 台，告警：%d 项\n", len(summaries), running, warning))
	builder.WriteString(fmt.Sprintf("总流量：%.2f / %.2f GB (%.2f%%)", used, total, usagePercent(used, total)))
	if !lastRun.IsZero() {
		location, locErr := time.LoadLocation(config.Timezone)
		if locErr != nil {
			location = time.FixedZone("CST", 8*3600)
		}
		builder.WriteString("\n最近同步：")
		builder.WriteString(lastRun.In(location).Format("2006-01-02 15:04:05"))
	}
	for _, account := range summaries {
		name := firstNonEmptyText(account.Remark, account.Account, strconv.FormatInt(account.ID, 10))
		builder.WriteString(fmt.Sprintf("\n#%d %s：%.2f / %.2f GB (%.2f%%)，%s", account.ID, name, account.FlowUsed, account.FlowTotal, account.Percentage, statusLabel(account.InstanceStatus)))
	}
	return builder.String(), nil
}

func telegramMainKeyboard() notify.TelegramInlineKeyboard {
	return notify.TelegramInlineKeyboard{
		{{Text: "状态", CallbackData: "status"}, {Text: "日报", CallbackData: "report"}},
		{{Text: "刷新全部", CallbackData: "refresh_all"}, {Text: "实例列表", CallbackData: "accounts"}},
		{{Text: "添加实例", CallbackData: "add_account"}, {Text: "设置", CallbackData: "settings"}},
	}
}

func telegramReportKeyboard() notify.TelegramInlineKeyboard {
	return notify.TelegramInlineKeyboard{{{Text: "刷新日报", CallbackData: "report"}, {Text: "返回菜单", CallbackData: "menu"}}}
}

func telegramAccountsKeyboard(accounts []domain.AccountSummary) notify.TelegramInlineKeyboard {
	keyboard := make(notify.TelegramInlineKeyboard, 0, len(accounts)+1)
	for _, account := range accounts {
		name := firstNonEmptyText(account.Remark, account.Account, strconv.FormatInt(account.ID, 10))
		keyboard = append(keyboard, []notify.TelegramInlineButton{{Text: fmt.Sprintf("#%d %s", account.ID, name), CallbackData: fmt.Sprintf("account:%d", account.ID)}})
	}
	keyboard = append(keyboard, []notify.TelegramInlineButton{{Text: "添加实例", CallbackData: "add_account"}, {Text: "返回菜单", CallbackData: "menu"}})
	return keyboard
}

func telegramAccountKeyboard(id int64) notify.TelegramInlineKeyboard {
	return notify.TelegramInlineKeyboard{
		{{Text: "刷新", CallbackData: fmt.Sprintf("refresh:%d", id)}, {Text: "开机", CallbackData: fmt.Sprintf("start:%d", id)}, {Text: "关机", CallbackData: fmt.Sprintf("stop:%d", id)}},
		{{Text: "实例列表", CallbackData: "accounts"}, {Text: "返回菜单", CallbackData: "menu"}},
	}
}

func telegramAccountKeyboardFromText(idText string) notify.TelegramInlineKeyboard {
	id, err := parseTelegramAccountID(idText)
	if err != nil {
		return telegramMainKeyboard()
	}
	return telegramAccountKeyboard(id)
}

func telegramSettingsKeyboard(config domain.Config) notify.TelegramInlineKeyboard {
	daily := "开启日报"
	dailyData := "set:daily:on"
	if config.EnableDailyReport {
		daily = "关闭日报"
		dailyData = "set:daily:off"
	}
	keepAlive := "开启保活"
	keepAliveData := "set:keepalive:on"
	if config.KeepAlive {
		keepAlive = "关闭保活"
		keepAliveData = "set:keepalive:off"
	}
	billing := "开启账单"
	billingData := "set:billing:on"
	if config.EnableBilling {
		billing = "关闭账单"
		billingData = "set:billing:off"
	}
	return notify.TelegramInlineKeyboard{
		{{Text: daily, CallbackData: dailyData}, {Text: "日报时间", CallbackData: "field:daily_time"}},
		{{Text: "告警阈值", CallbackData: "field:threshold"}, {Text: "刷新间隔", CallbackData: "field:api_interval"}},
		{{Text: keepAlive, CallbackData: keepAliveData}, {Text: billing, CallbackData: billingData}},
		{{Text: "系统时区", CallbackData: "field:timezone"}, {Text: "返回菜单", CallbackData: "menu"}},
	}
}

func telegramSettingsText(config domain.Config) string {
	return fmt.Sprintf("当前设置\n日报：%s，时间：%s\n时区：%s\n告警阈值：%d%%\nAPI 刷新间隔：%d 秒\n保活：%s\n账单与余额：%s",
		boolLabel(config.EnableDailyReport), config.DailyReportTime, config.Timezone, config.TrafficThreshold, config.APIInterval, boolLabel(config.KeepAlive), boolLabel(config.EnableBilling))
}

func boolLabel(value bool) string {
	if value {
		return "开启"
	}
	return "关闭"
}

func statusLabel(status string) string {
	switch status {
	case domain.StatusRunning:
		return "运行中"
	case domain.StatusStopped:
		return "已停止"
	case domain.StatusStarting:
		return "启动中"
	case domain.StatusStopping:
		return "停止中"
	case "Pending":
		return "等待中"
	default:
		return "未知"
	}
}

func trafficHealthLabel(account domain.AccountSummary) string {
	if account.OverThreshold {
		return "告警"
	}
	if account.Percentage >= 80 {
		return "注意"
	}
	return "正常"
}

func reportCardTitle(account domain.AccountSummary) string {
	return appendStar(firstNonEmptyText(account.Remark, account.Account, strconv.FormatInt(account.ID, 10)))
}

func appendStar(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasSuffix(value, "*") {
		return value
	}
	return value + "*"
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func (e *Engine) enqueueTelegramControl(ctx context.Context, config domain.Config, args []string, action string) string {
	if len(args) == 0 {
		if action == "start" {
			return "用法：/startvm <实例ID>"
		}
		return "用法：/stopvm <实例ID>"
	}
	accountID, err := parseTelegramAccountID(args[0])
	if err != nil {
		if action == "start" {
			return "用法：/startvm <实例ID>"
		}
		return "用法：/stopvm <实例ID>"
	}
	if config.KeepAlive && action == "stop" {
		return "保活启用时不能手动关机。"
	}
	if _, err = e.store.GetAccount(ctx, accountID); err != nil {
		return fmt.Sprintf("未找到实例 #%d。", accountID)
	}
	job, err := e.Enqueue(ctx, JobControlInstance, accountID, ParseControlPayload(action, "Telegram"), JobUniqueKey(JobControlInstance, accountID, action))
	if err != nil {
		return "控制任务创建失败：" + err.Error()
	}
	label := map[string]string{"start": "开机", "stop": "关机"}[action]
	return fmt.Sprintf("已创建实例 #%d %s任务：%s", accountID, label, job.ID)
}

func (e *Engine) telegramStatusText(ctx context.Context, config domain.Config) (string, error) {
	summaries, lastRun, err := e.Summary(ctx)
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	running, warning, used, total := 0, 0, 0.0, 0.0
	for _, account := range summaries {
		if account.InstanceStatus == domain.StatusRunning {
			running++
		}
		if account.OverThreshold {
			warning++
		}
		used += account.FlowUsed
		total += account.FlowTotal
	}
	builder.WriteString("[CDT Monitor] 实例状态\n")
	builder.WriteString(fmt.Sprintf("实例：%d 台，运行中：%d 台，告警：%d 项\n", len(summaries), running, warning))
	builder.WriteString(fmt.Sprintf("总流量：%.2f / %.2f GB (%.2f%%)", used, total, usagePercent(used, total)))
	if !lastRun.IsZero() {
		location, locErr := time.LoadLocation(config.Timezone)
		if locErr != nil {
			location = time.FixedZone("CST", 8*3600)
		}
		builder.WriteString("\n最近同步：")
		builder.WriteString(lastRun.In(location).Format("2006-01-02 15:04:05"))
	}
	for _, account := range summaries {
		name := firstNonEmptyText(account.Remark, account.Account, strconv.FormatInt(account.ID, 10))
		builder.WriteString("\n")
		builder.WriteString(fmt.Sprintf("#%d %s：%.2f / %.2f GB (%.2f%%)，%s", account.ID, name, account.FlowUsed, account.FlowTotal, account.Percentage, account.InstanceStatus))
	}
	return builder.String(), nil
}

func parseTelegramAccountID(value string) (int64, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "#")
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 1 {
		return 0, errors.New("invalid account id")
	}
	return id, nil
}

func telegramHelpText() string {
	return `CDT Monitor Bot 可用命令：
/start 打开内联按钮菜单
/status 查看实例与流量状态
/report 获取今日流量报告
/daily 获取今日流量报告
/today 获取今日流量报告
/refresh 刷新全部实例
/refresh <实例ID> 刷新指定实例
/startvm <实例ID> 开机
/stopvm <实例ID> 关机
/accounts 查看实例列表
/settings 查看和修改面板设置
/add 在 Bot 中添加实例
/cancel 取消当前添加或设置流程
/help 查看帮助`
}

func (e *Engine) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func dueWithin(now time.Time, hhmm string, window time.Duration) bool {
	parsed, err := time.Parse("15:04", hhmm)
	if err != nil {
		return false
	}
	target := time.Date(now.Year(), now.Month(), now.Day(), parsed.Hour(), parsed.Minute(), 0, 0, now.Location())
	delta := now.Sub(target)
	return delta >= 0 && delta <= window
}

func inTimeRange(current, start, end string) bool {
	if start == "" || end == "" {
		return false
	}
	if start < end {
		return current >= start && current < end
	}
	return current >= start || current < end
}

func transient(status string) bool {
	return status == domain.StatusStarting || status == domain.StatusStopping || status == "Pending" || status == domain.StatusUnknown
}

func usagePercent(traffic, maxTraffic float64) float64 {
	if maxTraffic <= 0 {
		return 0
	}
	return math.Round((traffic/maxTraffic)*10000) / 100
}

func masked(accessKeyID string) string {
	if len(accessKeyID) <= 7 {
		return accessKeyID + "***"
	}
	return accessKeyID[:7] + "***"
}

func firstNonEmptyText(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "CDT"
}

func newEvent(eventType, title, summary string, accountID int64, fields map[string]string) domain.NotificationEvent {
	id, _ := security.NewToken(18)
	return domain.NotificationEvent{ID: id, Type: eventType, Title: title, Summary: summary, AccountID: accountID, Fields: fields, CreatedAt: time.Now().UTC()}
}

func (e *Engine) Summary(ctx context.Context) ([]domain.AccountSummary, time.Time, error) {
	config, err := e.store.GetConfig(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	lastRun, err := e.store.LastMonitorRun(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	result := make([]domain.AccountSummary, 0, len(config.Accounts))
	for _, account := range config.Accounts {
		percentage := usagePercent(account.TrafficUsed, account.MaxTraffic)
		item := domain.AccountSummary{
			ID: account.ID, Account: masked(account.AccessKeyID), Remark: account.Remark, Region: account.RegionID, RegionName: RegionName(account.RegionID),
			FlowTotal: account.MaxTraffic, FlowUsed: math.Round(account.TrafficUsed*100) / 100, Percentage: percentage, Threshold: config.TrafficThreshold,
			OverThreshold: percentage >= float64(config.TrafficThreshold), InstanceStatus: account.InstanceStatus, LastUpdated: account.UpdatedAt,
			Stale: account.UpdatedAt.IsZero() || time.Since(account.UpdatedAt) > time.Duration(max(config.APIInterval*2, 180))*time.Second,
		}
		if config.EnableBilling {
			var billingError struct {
				Message string `json:"message"`
			}
			if ok, _ := e.store.BillingCache(ctx, account.ID, "error", "", 7*24*time.Hour, &billingError); ok {
				item.BillingError = strings.TrimSpace(billingError.Message)
			}
			var balance aliyun.BillingBalance
			if ok, _ := e.store.BillingCache(ctx, account.ID, "balance", "", 7*24*time.Hour, &balance); ok {
				item.Balance, item.Currency = &balance.Amount, balance.Currency
			}
			var bill aliyun.BillingBill
			if ok, _ := e.store.BillingCache(ctx, account.ID, "instance_bill", time.Now().Format("2006-01"), 7*24*time.Hour, &bill); ok {
				item.MonthlyCost = &bill.TotalCost
			}
		}
		result = append(result, item)
	}
	return result, lastRun, nil
}

func RegionName(region string) string {
	names := map[string]string{
		"cn-hongkong": "中国香港", "ap-southeast-1": "新加坡", "us-west-1": "美国（硅谷）", "us-east-1": "美国（弗吉尼亚）",
		"cn-hangzhou": "华东 1（杭州）", "cn-shanghai": "华东 2（上海）", "cn-qingdao": "华北 1（青岛）", "cn-beijing": "华北 2（北京）",
		"cn-zhangjiakou": "华北 3（张家口）", "cn-huhehaote": "华北 5（呼和浩特）", "cn-wulanchabu": "华北 6（乌兰察布）",
		"cn-shenzhen": "华南 1（深圳）", "cn-heyuan": "华南 2（河源）", "cn-guangzhou": "华南 3（广州）", "cn-chengdu": "西南 1（成都）", "ap-northeast-1": "日本（东京）",
	}
	if name := names[region]; name != "" {
		return name
	}
	return region
}

func ParseControlPayload(action, source string) string {
	payload, _ := json.Marshal(map[string]string{"action": strings.ToLower(action), "source": source})
	return string(payload)
}

func ParseNotifyPayload(channel string) string {
	payload, _ := json.Marshal(map[string]string{"channel": channel})
	return string(payload)
}

func JobUniqueKey(jobType string, accountID int64, suffix string) string {
	return jobType + ":" + strconv.FormatInt(accountID, 10) + ":" + suffix
}
