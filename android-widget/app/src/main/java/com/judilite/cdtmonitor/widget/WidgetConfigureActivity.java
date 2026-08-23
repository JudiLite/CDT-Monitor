package com.judilite.cdtmonitor.widget;

import android.app.Activity;
import android.appwidget.AppWidgetManager;
import android.content.Intent;
import android.os.Bundle;
import android.widget.ArrayAdapter;
import android.widget.Button;
import android.widget.Spinner;
import android.widget.TextView;

import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

public final class WidgetConfigureActivity extends Activity {
    private final ExecutorService executor = Executors.newSingleThreadExecutor();
    private final List<CdtApi.Account> accounts = new ArrayList<>();
    private int appWidgetId = AppWidgetManager.INVALID_APPWIDGET_ID;
    private Spinner spinner;
    private Button createButton;
    private TextView hint;
    private boolean loaded;
    private boolean loading;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setResult(RESULT_CANCELED);
        setContentView(R.layout.activity_widget_configure);
        appWidgetId = getIntent().getIntExtra(AppWidgetManager.EXTRA_APPWIDGET_ID, appWidgetId);
        if (appWidgetId == AppWidgetManager.INVALID_APPWIDGET_ID) {
            finish();
            return;
        }
        spinner = findViewById(R.id.instance_spinner);
        createButton = findViewById(R.id.create_widget);
        hint = findViewById(R.id.configure_hint);
        createButton.setOnClickListener(view -> createWidget());
        findViewById(R.id.open_settings).setOnClickListener(view -> startActivity(new Intent(this, SettingsActivity.class)));
        loadSummary();
    }

    @Override
    protected void onResume() {
        super.onResume();
        if (!loaded && !loading && AppPreferences.hasConnection(this)) {
            loadSummary();
        }
    }

    private void loadSummary() {
        if (loaded || loading) return;
        if (!AppPreferences.hasConnection(this)) {
            hint.setText("请先保存站点地址和 API Key，再返回这里选择实例。");
            return;
        }
        loading = true;
        hint.setText("正在读取 widget summary…");
        executor.execute(() -> {
            try {
                CdtApi.Summary summary = CdtApi.fetchSummary(AppPreferences.siteUrl(this), AppPreferences.apiKey(this));
                runOnUiThread(() -> showAccounts(summary.accounts));
            } catch (Exception error) {
                runOnUiThread(() -> {
                    loading = false;
                    hint.setText(error.getMessage() == null ? "读取实例失败，请检查连接设置" : error.getMessage());
                });
            }
        });
    }

    private void showAccounts(List<CdtApi.Account> result) {
        accounts.clear();
        accounts.addAll(result);
        List<String> labels = new ArrayList<>();
        for (CdtApi.Account account : accounts) {
            labels.add(account.displayName() + " · " + account.displayStatus());
        }
        ArrayAdapter<String> adapter = new ArrayAdapter<>(this, android.R.layout.simple_spinner_item, labels);
        adapter.setDropDownViewResource(android.R.layout.simple_spinner_dropdown_item);
        spinner.setAdapter(adapter);
        loaded = true;
        loading = false;
        createButton.setEnabled(!accounts.isEmpty());
        hint.setText(accounts.isEmpty() ? "API 没有返回可用实例。" : "选择一个实例后添加小组件。");
    }

    private void createWidget() {
        if (spinner.getSelectedItemPosition() < 0 || spinner.getSelectedItemPosition() >= accounts.size()) return;
        CdtApi.Account account = accounts.get(spinner.getSelectedItemPosition());
        AppPreferences.saveWidgetAccount(this, appWidgetId, account.id);
        CdtWidgetProvider.updateWidget(this, appWidgetId);
        Intent result = new Intent();
        result.putExtra(AppWidgetManager.EXTRA_APPWIDGET_ID, appWidgetId);
        setResult(RESULT_OK, result);
        finish();
    }

    @Override
    protected void onDestroy() {
        executor.shutdownNow();
        super.onDestroy();
    }
}
