package com.judilite.cdtmonitor.widget;

import android.net.Uri;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.Locale;

final class CdtApi {
    private CdtApi() {
    }

    static Summary fetchSummary(String siteUrl, String apiKey) throws IOException {
        String baseUrl = normalizeBaseUrl(siteUrl);
        if (baseUrl.isEmpty() || apiKey == null || apiKey.trim().isEmpty()) {
            throw new IOException("请先填写站点地址和 API Key");
        }
        HttpURLConnection connection = null;
        try {
            URL url = new URL(baseUrl + "/api/v1/widget/summary");
            connection = (HttpURLConnection) url.openConnection();
            connection.setRequestMethod("GET");
            connection.setConnectTimeout(5_000);
            connection.setReadTimeout(8_000);
            connection.setRequestProperty("Accept", "application/json");
            connection.setRequestProperty("Authorization", "Bearer " + apiKey.trim());
            int status = connection.getResponseCode();
            String body = readBody(status >= 400 ? connection.getErrorStream() : connection.getInputStream());
            if (status < 200 || status >= 300) {
                throw new IOException(errorMessage(body, status));
            }
            return Summary.fromJson(new JSONObject(body));
        } catch (org.json.JSONException e) {
            throw new IOException("服务返回的数据格式无效", e);
        } finally {
            if (connection != null) {
                connection.disconnect();
            }
        }
    }

    static String normalizeBaseUrl(String value) {
        if (value == null) {
            return "";
        }
        String candidate = value.trim();
        while (candidate.endsWith("/")) {
            candidate = candidate.substring(0, candidate.length() - 1);
        }
        Uri uri = Uri.parse(candidate);
        if (("http".equalsIgnoreCase(uri.getScheme()) || "https".equalsIgnoreCase(uri.getScheme()))
                && uri.getHost() != null
                && uri.getUserInfo() == null
                && uri.getQuery() == null
                && uri.getFragment() == null) {
            return candidate;
        }
        return "";
    }

    private static String readBody(InputStream stream) throws IOException {
        if (stream == null) {
            return "";
        }
        try (BufferedReader reader = new BufferedReader(new InputStreamReader(stream, StandardCharsets.UTF_8))) {
            StringBuilder result = new StringBuilder();
            String line;
            while ((line = reader.readLine()) != null) {
                result.append(line);
            }
            return result.toString();
        }
    }

    private static String errorMessage(String body, int status) {
        try {
            JSONObject error = new JSONObject(body).optJSONObject("error");
            if (error != null && !error.optString("message").isEmpty()) {
                return error.optString("message");
            }
        } catch (Exception ignored) {
            // Keep the HTTP status when the server returns a non-JSON error page.
        }
        if (status == 401) {
            return "API Key 无效或已撤销";
        }
        if (status == 403) {
            return "API Key 缺少 widget:read 权限";
        }
        return "请求失败（HTTP " + status + "）";
    }

    static final class Summary {
        final List<Account> accounts;
        final String updatedAt;

        private Summary(List<Account> accounts, String updatedAt) {
            this.accounts = accounts;
            this.updatedAt = updatedAt;
        }

        static Summary fromJson(JSONObject root) throws org.json.JSONException {
            JSONArray rawAccounts = root.optJSONArray("accounts");
            if (rawAccounts == null) {
                throw new org.json.JSONException("accounts missing");
            }
            List<Account> accounts = new ArrayList<>();
            for (int index = 0; index < rawAccounts.length(); index++) {
                JSONObject raw = rawAccounts.getJSONObject(index);
                accounts.add(new Account(
                        raw.optLong("id", -1L),
                        raw.optString("name", "CDT 实例"),
                        raw.optString("status", "Unknown"),
                        raw.optDouble("used", 0D),
                        raw.optDouble("total", 0D),
                        raw.optDouble("percentage", 0D),
                        raw.optString("updated_at", "")));
            }
            return new Summary(accounts, root.optString("updated_at", ""));
        }

        Account find(long accountId) {
            for (Account account : accounts) {
                if (account.id == accountId) {
                    return account;
                }
            }
            return null;
        }
    }

    static final class Account {
        final long id;
        final String name;
        final String status;
        final double used;
        final double total;
        final double percentage;
        final String updatedAt;

        private Account(long id, String name, String status, double used, double total, double percentage, String updatedAt) {
            this.id = id;
            this.name = name;
            this.status = status;
            this.used = used;
            this.total = total;
            this.percentage = percentage;
            this.updatedAt = updatedAt;
        }

        String displayName() {
            return name.trim().isEmpty() ? "CDT 实例 #" + id : name;
        }

        String displayStatus() {
            if ("Running".equalsIgnoreCase(status)) return "运行中";
            if ("Stopped".equalsIgnoreCase(status)) return "已停止";
            if ("Starting".equalsIgnoreCase(status)) return "启动中";
            if ("Stopping".equalsIgnoreCase(status)) return "停止中";
            return "未知";
        }

        String displayUsage() {
            return String.format(Locale.getDefault(), "%.2f / %.2f GB · %.1f%%", used, total, percentage);
        }
    }
}
