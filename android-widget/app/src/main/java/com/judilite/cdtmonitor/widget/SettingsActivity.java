package com.judilite.cdtmonitor.widget;

import android.app.Activity;
import android.os.Bundle;
import android.widget.Button;
import android.widget.EditText;
import android.widget.TextView;

import java.io.IOException;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

public final class SettingsActivity extends Activity {
    private final ExecutorService executor = Executors.newSingleThreadExecutor();
    private EditText siteUrl;
    private EditText apiKey;
    private TextView message;
    private Button saveButton;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_settings);
        siteUrl = findViewById(R.id.site_url);
        apiKey = findViewById(R.id.api_key);
        message = findViewById(R.id.settings_message);
        saveButton = findViewById(R.id.save_settings);
        siteUrl.setText(AppPreferences.siteUrl(this));
        saveButton.setOnClickListener(view -> saveAndTest());
    }

    private void saveAndTest() {
        String url = siteUrl.getText().toString().trim();
        String token = apiKey.getText().toString().trim();
        if (CdtApi.normalizeBaseUrl(url).isEmpty()) {
            message.setText("请输入有效的 http:// 或 https:// 站点地址");
            return;
        }
        if (token.isEmpty()) {
            message.setText("请输入 API Key");
            return;
        }
        setBusy(true);
        message.setText("正在测试连接…");
        executor.execute(() -> {
            try {
                CdtApi.Summary summary = CdtApi.fetchSummary(url, token);
                if (!AppPreferences.saveConnection(this, CdtApi.normalizeBaseUrl(url), token)) {
                    throw new IOException("Android Keystore 无法保存 API Key，请检查设备安全设置");
                }
                CdtWidgetProvider.updateAllWidgets(this);
                runOnUiThread(() -> {
                    setBusy(false);
                    apiKey.setText("");
                    message.setText("连接成功，发现 " + summary.accounts.size() + " 个实例。现在可以从桌面添加 CDT 小组件。");
                });
            } catch (Exception error) {
                runOnUiThread(() -> {
                    setBusy(false);
                    message.setText(error.getMessage() == null ? "连接失败，请检查站点地址和 API Key" : error.getMessage());
                });
            }
        });
    }

    private void setBusy(boolean busy) {
        saveButton.setEnabled(!busy);
        siteUrl.setEnabled(!busy);
        apiKey.setEnabled(!busy);
    }

    @Override
    protected void onDestroy() {
        executor.shutdownNow();
        super.onDestroy();
    }
}
