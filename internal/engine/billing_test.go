package engine

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/JudiLite/CDT-Monitor/internal/aliyun"
	"github.com/JudiLite/CDT-Monitor/internal/domain"
	"github.com/JudiLite/CDT-Monitor/internal/notify"
	"github.com/JudiLite/CDT-Monitor/internal/store"
)

type billingTestProvider struct{}

func (billingTestProvider) GetTraffic(context.Context, domain.Account, string) (float64, error) {
	return 1.25, nil
}

func (billingTestProvider) GetInstanceStatus(context.Context, domain.Account, string) (string, error) {
	return domain.StatusRunning, nil
}

func (billingTestProvider) ControlInstance(context.Context, domain.Account, string, string, string) error {
	return nil
}

func (billingTestProvider) GetAccountBalance(context.Context, domain.Account, string) (aliyun.BillingBalance, error) {
	return aliyun.BillingBalance{Amount: 123.45, Currency: "CNY"}, nil
}

func (billingTestProvider) GetInstanceBill(context.Context, domain.Account, string, string) (aliyun.BillingBill, error) {
	return aliyun.BillingBill{TotalCost: 23.456}, nil
}

func TestProcessAccountFetchesMissingBillingCache(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	config := domain.Config{
		AdminPassword:    "Strong-Password-42!",
		TrafficThreshold: 95,
		ShutdownMode:     "KeepCharging",
		ThresholdAction:  "stop_and_notify",
		APIInterval:      600,
		EnableBilling:    true,
		Timezone:         "Asia/Shanghai",
		Accounts: []domain.Account{{
			AccessKeyID: "LTAItest", AccessKeySecret: "secret", RegionID: "cn-hongkong", InstanceID: "i-test", MaxTraffic: 200, SiteType: "china",
		}},
	}
	if err = st.Setup(ctx, config); err != nil {
		t.Fatal(err)
	}
	accounts, err := st.ListAccounts(ctx)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts=%v err=%v", accounts, err)
	}

	engine := New(st, billingTestProvider{}, notify.New(), slog.Default(), 1)
	if _, err = engine.processAccount(ctx, accounts[0].ID, false); err != nil {
		t.Fatal(err)
	}

	var balance aliyun.BillingBalance
	if ok, err := st.BillingCache(ctx, accounts[0].ID, "balance", "", time.Hour, &balance); err != nil || !ok || balance.Amount != 123.45 {
		t.Fatalf("balance cache ok=%v value=%#v err=%v", ok, balance, err)
	}
	var bill aliyun.BillingBill
	if ok, err := st.BillingCache(ctx, accounts[0].ID, "instance_bill", time.Now().Format("2006-01"), time.Hour, &bill); err != nil || !ok || bill.TotalCost != 23.456 {
		t.Fatalf("bill cache ok=%v value=%#v err=%v", ok, bill, err)
	}
	summaries, _, err := engine.Summary(ctx)
	if err != nil || len(summaries) != 1 || summaries[0].Balance == nil || *summaries[0].Balance != 123.45 || summaries[0].MonthlyCost == nil || *summaries[0].MonthlyCost != 23.456 {
		t.Fatalf("summary=%#v err=%v", summaries, err)
	}
}
