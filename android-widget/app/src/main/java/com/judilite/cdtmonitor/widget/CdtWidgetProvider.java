package com.judilite.cdtmonitor.widget;

import android.app.AlarmManager;
import android.app.PendingIntent;
import android.appwidget.AppWidgetManager;
import android.appwidget.AppWidgetProvider;
import android.content.BroadcastReceiver.PendingResult;
import android.content.ComponentName;
import android.content.Context;
import android.content.Intent;
import android.os.SystemClock;
import android.widget.RemoteViews;

import java.text.ParseException;
import java.text.SimpleDateFormat;
import java.util.Date;
import java.util.Locale;
import java.util.TimeZone;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

public final class CdtWidgetProvider extends AppWidgetProvider {
    static final String ACTION_REFRESH = "com.judilite.cdtmonitor.widget.ACTION_REFRESH";
    private static final long REFRESH_INTERVAL_MS = 30L * 60L * 1000L;
    private static final ExecutorService EXECUTOR = Executors.newSingleThreadExecutor();

    @Override
    public void onUpdate(Context context, AppWidgetManager manager, int[] widgetIds) {
        updateWidgets(context, widgetIds);
        scheduleRefresh(context);
    }

    @Override
    public void onReceive(Context context, Intent intent) {
        String action = intent.getAction();
        if (ACTION_REFRESH.equals(action) || AppWidgetManager.ACTION_APPWIDGET_UPDATE.equals(action)) {
            int[] ids = intent.getIntArrayExtra(AppWidgetManager.EXTRA_APPWIDGET_IDS);
            if (ids == null || ids.length == 0) {
                ids = AppWidgetManager.getInstance(context).getAppWidgetIds(
                        new ComponentName(context, CdtWidgetProvider.class));
            }
            updateWidgets(context, ids, goAsync());
            if (AppWidgetManager.ACTION_APPWIDGET_UPDATE.equals(action)) {
                scheduleRefresh(context);
            }
            return;
        }
        super.onReceive(context, intent);
    }

    @Override
    public void onDeleted(Context context, int[] widgetIds) {
        for (int widgetId : widgetIds) {
            AppPreferences.removeWidget(context, widgetId);
        }
        super.onDeleted(context, widgetIds);
    }

    @Override
    public void onEnabled(Context context) {
        scheduleRefresh(context);
    }

    @Override
    public void onDisabled(Context context) {
        AlarmManager alarmManager = (AlarmManager) context.getSystemService(Context.ALARM_SERVICE);
        if (alarmManager != null) alarmManager.cancel(refreshIntent(context));
    }

    static void updateWidget(Context context, int widgetId) {
        updateWidgets(context, new int[]{widgetId});
    }

    static void updateAllWidgets(Context context) {
        int[] ids = AppWidgetManager.getInstance(context).getAppWidgetIds(
                new ComponentName(context, CdtWidgetProvider.class));
        updateWidgets(context, ids);
    }

    private static void updateWidgets(Context context, int[] widgetIds) {
        updateWidgets(context, widgetIds, null);
    }

    private static void updateWidgets(Context context, int[] widgetIds, PendingResult pendingResult) {
        if (widgetIds == null || widgetIds.length == 0) {
            if (pendingResult != null) pendingResult.finish();
            return;
        }
        Context appContext = context.getApplicationContext();
        renderAll(appContext, widgetIds, null);
        EXECUTOR.execute(() -> {
            try {
                CdtApi.Summary summary = CdtApi.fetchSummary(AppPreferences.siteUrl(appContext), AppPreferences.apiKey(appContext));
                renderAll(appContext, widgetIds, summary);
            } catch (Exception error) {
                renderAll(appContext, widgetIds, error);
            } finally {
                if (pendingResult != null) pendingResult.finish();
            }
        });
    }

    private static void renderAll(Context context, int[] widgetIds, Object state) {
        AppWidgetManager manager = AppWidgetManager.getInstance(context);
        for (int widgetId : widgetIds) {
            RemoteViews views = new RemoteViews(context.getPackageName(), R.layout.widget_instance);
            Intent open = new Intent(context, SettingsActivity.class);
            PendingIntent pendingIntent = PendingIntent.getActivity(context, widgetId, open,
                    PendingIntent.FLAG_UPDATE_CURRENT | PendingIntent.FLAG_IMMUTABLE);
            views.setOnClickPendingIntent(R.id.widget_root, pendingIntent);
            if (state == null) {
                views.setTextViewText(R.id.widget_name, "CDT Monitor");
                views.setTextViewText(R.id.widget_status, "读取中");
                views.setTextViewText(R.id.widget_usage, "正在更新…");
                views.setTextViewText(R.id.widget_updated, "点击打开设置");
                views.setProgressBar(R.id.widget_progress, 100, 0, false);
            } else if (state instanceof CdtApi.Summary) {
                CdtApi.Account account = ((CdtApi.Summary) state).find(AppPreferences.widgetAccount(context, widgetId));
                if (account == null) {
                    views.setTextViewText(R.id.widget_name, "实例不可用");
                    views.setTextViewText(R.id.widget_status, "未找到");
                    views.setTextViewText(R.id.widget_usage, "请重新添加小组件并选择实例");
                    views.setTextViewText(R.id.widget_updated, "API 未返回已保存的实例");
                    views.setProgressBar(R.id.widget_progress, 100, 0, false);
                } else {
                    views.setTextViewText(R.id.widget_name, account.displayName());
                    views.setTextViewText(R.id.widget_status, account.displayStatus());
                    views.setTextViewText(R.id.widget_usage, account.displayUsage());
                    views.setTextViewText(R.id.widget_updated, "更新 " + formatTimestamp(account.updatedAt));
                    int progress = (int) Math.max(0, Math.min(100, Math.round(account.percentage)));
                    views.setProgressBar(R.id.widget_progress, 100, progress, false);
                    int color = statusColor(account.status);
                    views.setTextColor(R.id.widget_status, color);
                }
            } else {
                String error = ((Exception) state).getMessage();
                views.setTextViewText(R.id.widget_name, "CDT Monitor");
                views.setTextViewText(R.id.widget_status, "更新失败");
                views.setTextViewText(R.id.widget_usage, error == null ? "请检查连接设置" : error);
                views.setTextViewText(R.id.widget_updated, "点击打开设置重试");
                views.setProgressBar(R.id.widget_progress, 100, 0, false);
                views.setTextColor(R.id.widget_status, statusColor("Unknown"));
            }
            manager.updateAppWidget(widgetId, views);
        }
    }

    private static int statusColor(String status) {
        if ("Running".equalsIgnoreCase(status)) return 0xff188a55;
        if ("Stopped".equalsIgnoreCase(status)) return 0xffb56600;
        return 0xff667482;
    }

    private static String formatTimestamp(String raw) {
        if (raw == null || raw.isEmpty()) return "未知";
        String[] patterns = {"yyyy-MM-dd'T'HH:mm:ss.SSSX", "yyyy-MM-dd'T'HH:mm:ssX", "yyyy-MM-dd HH:mm:ss"};
        for (String pattern : patterns) {
            try {
                SimpleDateFormat parser = new SimpleDateFormat(pattern, Locale.US);
                parser.setTimeZone(TimeZone.getTimeZone("UTC"));
                Date date = parser.parse(raw);
                if (date != null) return new SimpleDateFormat("MM-dd HH:mm", Locale.getDefault()).format(date);
            } catch (ParseException ignored) {
                // Try the next RFC3339 variant.
            }
        }
        return raw.length() > 16 ? raw.substring(0, 16).replace('T', ' ') : raw;
    }

    private static void scheduleRefresh(Context context) {
        AlarmManager alarmManager = (AlarmManager) context.getSystemService(Context.ALARM_SERVICE);
        if (alarmManager != null) {
            alarmManager.setInexactRepeating(
                    AlarmManager.ELAPSED_REALTIME,
                    SystemClock.elapsedRealtime() + REFRESH_INTERVAL_MS,
                    REFRESH_INTERVAL_MS,
                    refreshIntent(context));
        }
    }

    private static PendingIntent refreshIntent(Context context) {
        Intent intent = new Intent(context, CdtWidgetProvider.class).setAction(ACTION_REFRESH);
        return PendingIntent.getBroadcast(context, 0, intent,
                PendingIntent.FLAG_UPDATE_CURRENT | PendingIntent.FLAG_IMMUTABLE);
    }
}
