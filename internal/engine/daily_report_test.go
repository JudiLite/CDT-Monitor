package engine

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/JudiLite/CDT-Monitor/internal/domain"
	"github.com/JudiLite/CDT-Monitor/internal/notify"
	"github.com/JudiLite/CDT-Monitor/internal/store"
)

func TestDailyReportUsesYesterdayClosedDailySnapshot(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	config := domain.Config{
		TrafficThreshold:  95,
		EnableDailyReport: true,
		DailyReportTime:   "23:59",
		ShutdownMode:      "KeepCharging",
		ThresholdAction:   "notify_only",
		APIInterval:       300,
		Timezone:          "Asia/Shanghai",
		Accounts: []domain.Account{{
			AccessKeyID:     "LTAI123456789",
			AccessKeySecret: "secret",
			RegionID:        "cn-hongkong",
			InstanceID:      "i-test",
			MaxTraffic:      200,
			Remark:          "AliCloud-CDT",
			SiteType:        "china",
		}},
	}
	if err = st.SaveConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	config, err = st.GetConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	account := config.Accounts[0]
	location, _ := time.LoadLocation("Asia/Shanghai")

	if err = st.AddTrafficStats(ctx, account.ID, 10, time.Date(2026, 8, 29, 23, 59, 0, 0, location)); err != nil {
		t.Fatal(err)
	}
	if err = st.AddTrafficStats(ctx, account.ID, 12.5, time.Date(2026, 8, 30, 23, 59, 0, 0, location)); err != nil {
		t.Fatal(err)
	}
	if err = st.AddTrafficStats(ctx, account.ID, 99, time.Date(2026, 8, 31, 10, 0, 0, 0, location)); err != nil {
		t.Fatal(err)
	}
	if err = st.UpdateRuntime(ctx, account.ID, 99, domain.StatusRunning, time.Date(2026, 8, 31, 10, 0, 0, 0, location)); err != nil {
		t.Fatal(err)
	}

	engine := New(st, nil, notify.New(), slog.Default(), 1)
	text, err := engine.dailyReportText(ctx, config, time.Date(2026, 8, 31, 12, 0, 0, 0, location))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "昨日流量：+2.50 GB") {
		t.Fatalf("report should use yesterday delta only, got:\n%s", text)
	}
	if strings.Contains(text, "昨日流量：+86.50 GB") || strings.Contains(text, "昨日流量：+89.00 GB") {
		t.Fatalf("report included today's traffic:\n%s", text)
	}
}
