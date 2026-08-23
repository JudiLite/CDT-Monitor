package com.judilite.cdtmonitor.widget;

import android.content.Context;
import android.content.SharedPreferences;

/** Centralizes persisted connection and per-widget selection settings. */
final class AppPreferences {
    private static final String FILE = "cdt_monitor_widget";
    private static final String SITE_URL = "site_url";
    private static final String API_KEY = "api_key";
    private static final String WIDGET_ACCOUNT_PREFIX = "widget_account_";

    private AppPreferences() {
    }

    private static SharedPreferences prefs(Context context) {
        return context.getSharedPreferences(FILE, Context.MODE_PRIVATE);
    }

    static String siteUrl(Context context) {
        return prefs(context).getString(SITE_URL, "");
    }

    static String apiKey(Context context) {
        String encrypted = prefs(context).getString(API_KEY, "");
        return SecretStore.decrypt(encrypted);
    }

    static boolean saveConnection(Context context, String siteUrl, String apiKey) {
        String encrypted = SecretStore.encrypt(apiKey);
        if (encrypted.isEmpty()) {
            return false;
        }
        return prefs(context).edit()
                .putString(SITE_URL, siteUrl)
                .putString(API_KEY, encrypted)
                .commit();
    }

    static boolean hasConnection(Context context) {
        return !siteUrl(context).isEmpty() && !apiKey(context).isEmpty();
    }

    static void saveWidgetAccount(Context context, int widgetId, long accountId) {
        prefs(context).edit().putLong(WIDGET_ACCOUNT_PREFIX + widgetId, accountId).apply();
    }

    static long widgetAccount(Context context, int widgetId) {
        return prefs(context).getLong(WIDGET_ACCOUNT_PREFIX + widgetId, -1L);
    }

    static void removeWidget(Context context, int widgetId) {
        prefs(context).edit().remove(WIDGET_ACCOUNT_PREFIX + widgetId).apply();
    }
}
