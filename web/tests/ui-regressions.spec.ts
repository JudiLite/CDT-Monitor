import { expect, test } from '@playwright/test'

const config = {
  traffic_threshold: 95,
  enable_schedule_notification: false,
  shutdown_mode: 'KeepCharging',
  threshold_action: 'stop_and_notify',
  keep_alive: false,
  api_interval: 600,
  enable_billing: true,
  timezone: 'Asia/Shanghai',
  notifications: {
    email: { enabled: false, to: '', host: '', port: 465, username: '', password_configured: false, security: 'ssl' },
    telegram: { enabled: false, token_configured: false, chat_id: '', proxy_type: 'none', proxy_url: '', proxy_ip: '', proxy_port: '', proxy_user: '', proxy_password_configured: false },
    webhook: { enabled: false, url: '', method: 'GET', request_type: 'JSON', body: '' },
  },
  accounts: [{
    id: 1,
    access_key_id: 'LTAI5test',
    secret_configured: true,
    region_id: 'cn-hongkong',
    instance_id: 'i-test',
    max_traffic: 200,
    schedule_enabled: false,
    start_time: '08:00',
    stop_time: '23:30',
    remark: '香港测试节点',
    site_type: 'china',
  }],
}

test('login brand is positioned above the authentication card', async ({ page }, testInfo) => {
  await page.route('**/api/v1/system/init-status', (route) => route.fulfill({ json: { initialized: true } }))
  await page.route('**/api/v1/status', (route) => route.fulfill({ status: 401, json: { error: { code: 'unauthorized', message: '需要登录' } } }))
  await page.route('**/api/v1/config', (route) => route.fulfill({ status: 401, json: { error: { code: 'unauthorized', message: '需要登录' } } }))

  await page.goto('/')
  const brand = page.locator('.login-brand')
  const card = page.locator('.auth-card')
  await expect(page.getByText('阿里云 CDT 流量与实例自动化控制台')).toBeVisible()
  await expect(brand.locator('.brand-mark')).toBeVisible()
  const brandBox = await brand.boundingBox()
  const cardBox = await card.boundingBox()
  expect(brandBox).not.toBeNull()
  expect(cardBox).not.toBeNull()
  expect(brandBox!.y + brandBox!.height).toBeLessThanOrEqual(cardBox!.y)
  await page.screenshot({ path: testInfo.outputPath('login-brand-desktop.png'), fullPage: true })
})

test('dashboard remains contained across supported viewport widths', async ({ page }, testInfo) => {
  await page.route('**/api/v1/system/init-status', (route) => route.fulfill({ json: { initialized: true } }))
  await page.route('**/api/v1/status', (route) => route.fulfill({ json: {
    accounts: [{
      id: 1,
      account: 'LTAI5te***',
      remark: '香港测试节点',
      region: 'cn-hongkong',
      region_name: '中国香港',
      flow_total: 200,
      flow_used: 12.5,
      percentage: 6.25,
      threshold: 95,
      over_threshold: false,
      instance_status: 'Running',
      last_updated: new Date().toISOString(),
      stale: false,
      monthly_cost: 23.456,
      balance: 123.45,
      currency: 'CNY',
    }],
    system_last_run: new Date().toISOString(),
  } }))
  await page.route('**/api/v1/config', (route) => route.fulfill({ json: config }))

  for (const viewport of [
    { width: 320, height: 720 },
    { width: 390, height: 844 },
    { width: 768, height: 1024 },
    { width: 1440, height: 900 },
  ]) {
    await page.setViewportSize(viewport)
    await page.goto('/')
    const title = page.getByRole('heading', { name: '资源控制台' })
    await expect(title).toBeVisible()
    if (viewport.width <= 640) {
      await page.waitForTimeout(700)
      const closedTitleBox = await title.boundingBox()
      const closedOverviewBox = await page.locator('.overview-head').boundingBox()
      expect(closedTitleBox).not.toBeNull()
      expect(closedOverviewBox).not.toBeNull()
      await page.getByRole('button', { name: '菜单' }).click()
      const actions = page.locator('.topbar-actions.open')
      const overview = page.locator('.overview-head')
      await expect(actions).toBeVisible()
      await page.waitForTimeout(300)
      const actionsBox = await actions.boundingBox()
      const titleBox = await title.boundingBox()
      const overviewBox = await overview.boundingBox()
      expect(actionsBox).not.toBeNull()
      expect(titleBox).not.toBeNull()
      expect(overviewBox).not.toBeNull()
      expect(Math.abs(titleBox!.y - closedTitleBox!.y)).toBeLessThanOrEqual(1)
      expect(Math.abs(overviewBox!.y - closedOverviewBox!.y)).toBeLessThanOrEqual(1)
      expect(actionsBox!.width).toBeGreaterThan(actionsBox!.height * 2)
      const overlapsTitle = actionsBox!.x < titleBox!.x + titleBox!.width
        && actionsBox!.x + actionsBox!.width > titleBox!.x
        && actionsBox!.y < titleBox!.y + titleBox!.height
        && actionsBox!.y + actionsBox!.height > titleBox!.y
      const layerState = await page.evaluate(() => {
        const menu = document.querySelector<HTMLElement>('.topbar-actions.open')
        const topbar = document.querySelector<HTMLElement>('.topbar')
        const overview = document.querySelector<HTMLElement>('.overview-head')
        return {
          menuZ: Number.parseInt(getComputedStyle(menu!).zIndex, 10) || 0,
          topbarZ: Number.parseInt(getComputedStyle(topbar!).zIndex, 10) || 0,
          overviewZ: Number.parseInt(getComputedStyle(overview!).zIndex, 10) || 0,
        }
      })
      expect(layerState.menuZ).toBeGreaterThan(0)
      expect(layerState.topbarZ).toBeGreaterThan(layerState.overviewZ)
      if (overlapsTitle) {
        const overlapCenter = {
          x: (Math.max(actionsBox!.x, titleBox!.x) + Math.min(actionsBox!.x + actionsBox!.width, titleBox!.x + titleBox!.width)) / 2,
          y: (Math.max(actionsBox!.y, titleBox!.y) + Math.min(actionsBox!.y + actionsBox!.height, titleBox!.y + titleBox!.height)) / 2,
        }
        const menuOwnsOverlap = await page.evaluate(({ x, y }) => Boolean(document.elementFromPoint(x, y)?.closest('.topbar-actions')), overlapCenter)
        expect(menuOwnsOverlap, `${viewport.width}px title paints above the menu`).toBe(true)
      }
      await page.screenshot({ path: testInfo.outputPath(`dashboard-menu-${viewport.width}.png`), fullPage: true })
      await page.getByRole('button', { name: '菜单' }).click()
    }
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
    expect(overflow, `${viewport.width}px viewport overflow`).toBeLessThanOrEqual(1)
    await page.screenshot({ path: testInfo.outputPath(`dashboard-${viewport.width}.png`), fullPage: true })
  }
})

test('top refresh forces every configured instance and reports completion', async ({ page }, testInfo) => {
  const accounts = [
    {
      id: 1, account: 'LTAI5on***', remark: '香港节点', region: 'cn-hongkong', region_name: '中国香港', flow_total: 200, flow_used: 12.5,
      percentage: 6.25, threshold: 95, over_threshold: false, instance_status: 'Running', last_updated: new Date().toISOString(), stale: false,
    },
    {
      id: 2, account: 'LTAI5tw***', remark: '新加坡节点', region: 'ap-southeast-1', region_name: '新加坡', flow_total: 300, flow_used: 8.25,
      percentage: 2.75, threshold: 95, over_threshold: false, instance_status: 'Stopped', last_updated: new Date().toISOString(), stale: false,
    },
  ]
  let statusRequests = 0
  let refreshAllCalls = 0
  const jobPolls = new Map<string, number>()
  await page.route('**/api/v1/system/init-status', (route) => route.fulfill({ json: { initialized: true } }))
  await page.route('**/api/v1/status', (route) => { statusRequests += 1; return route.fulfill({ json: { accounts, system_last_run: new Date().toISOString() } }) })
  await page.route('**/api/v1/config', (route) => route.fulfill({ json: { ...config, accounts: [config.accounts[0], { ...config.accounts[0], id: 2, access_key_id: 'LTAI5test2', instance_id: 'i-test-2', remark: '新加坡节点', region_id: 'ap-southeast-1', site_type: 'international' }] } }))
  await page.route('**/api/v1/accounts/refresh', (route) => {
    refreshAllCalls += 1
    expect(route.request().method()).toBe('POST')
    return route.fulfill({ status: 202, json: { jobs: [{ id: 'refresh-1', status: 'pending' }, { id: 'refresh-2', status: 'pending' }] } })
  })
  await page.route('**/api/v1/jobs/**', (route) => {
    const id = route.request().url().split('/').pop() || ''
    const count = (jobPolls.get(id) || 0) + 1
    jobPolls.set(id, count)
    return route.fulfill({ json: { id, status: count > 1 ? 'completed' : 'running' } })
  })

  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto('/')
  await page.getByRole('button', { name: '菜单' }).click()
  await page.getByRole('button', { name: '强制刷新全部实例' }).click()
  await expect(page.getByRole('button', { name: '正在强制刷新全部实例' })).toBeDisabled()
  await expect(page.getByText('已强制刷新 2 个实例')).toBeVisible({ timeout: 10_000 })
  expect(refreshAllCalls).toBe(1)
  expect(statusRequests).toBeGreaterThanOrEqual(2)
  expect(jobPolls.get('refresh-1')).toBeGreaterThanOrEqual(2)
  expect(jobPolls.get('refresh-2')).toBeGreaterThanOrEqual(2)
  await page.screenshot({ path: testInfo.outputPath('dashboard-refresh-all-mobile.png'), fullPage: true })
})

test('about update check falls back to the browser GitHub request', async ({ page }) => {
  await page.route('**/api/v1/system/init-status', (route) => route.fulfill({ json: { initialized: true } }))
  await page.route('**/api/v1/status', (route) => route.fulfill({ json: { accounts: [], system_last_run: new Date().toISOString() } }))
  await page.route('**/api/v1/config', (route) => route.fulfill({ json: config }))
  await page.route('**/api/v1/system/info**', (route) => {
    const check = new URL(route.request().url()).searchParams.get('check') === '1'
    return route.fulfill({ json: check
      ? { version: 'v2.0.2', commit: 'test', built_at: new Date().toISOString(), repository: 'https://github.com/JudiLite/CDT-Monitor', release_url: 'https://github.com/JudiLite/CDT-Monitor/releases', check_error: '暂时无法检查 GitHub Release' }
      : { version: 'v2.0.2', commit: 'test', built_at: new Date().toISOString(), repository: 'https://github.com/JudiLite/CDT-Monitor', release_url: 'https://github.com/JudiLite/CDT-Monitor/releases' } })
  })
  await page.route('https://api.github.com/repos/JudiLite/CDT-Monitor/releases/latest', (route) => route.fulfill({ headers: { 'access-control-allow-origin': '*' }, json: { tag_name: 'v2.0.3' } }))

  await page.goto('/')
  await page.getByRole('button', { name: '设置', exact: true }).click()
  await page.getByRole('button', { name: '关于' }).click()
  await page.getByRole('button', { name: '检查更新' }).click()
  await expect(page.getByText('GitHub 最新版本：v2.0.3')).toBeVisible()
  await expect(page.getByText('已通过浏览器网络检查版本')).toBeVisible()
})

test('dashboard billing, history precision and settings remain usable', async ({ page }, testInfo) => {
  await page.route('**/api/v1/system/init-status', (route) => route.fulfill({ json: { initialized: true } }))
  await page.route('**/api/v1/status', (route) => route.fulfill({ json: {
    accounts: [{
      id: 1,
      account: 'LTAI5te***',
      remark: '香港测试节点',
      region: 'cn-hongkong',
      region_name: '中国香港',
      flow_total: 200,
      flow_used: 12.5,
      percentage: 6.25,
      threshold: 95,
      over_threshold: false,
      instance_status: 'Running',
      last_updated: new Date().toISOString(),
      stale: false,
      monthly_cost: 23.456,
      balance: 123.45,
      currency: 'CNY',
    }],
    system_last_run: new Date().toISOString(),
  } }))
  await page.route('**/api/v1/config', (route) => route.fulfill({ json: config }))
  await page.route('**/api/v1/accounts/1/history', (route) => route.fulfill({ json: {
    hourly: [{ at: new Date().toISOString(), traffic: 1.23456 }],
    daily: [{ at: new Date().toISOString(), traffic: 9.87654 }],
  } }))
  const longToken = 'cdt_' + 'A1b2C3d4'.repeat(12)
  await page.route('**/api/v1/api-keys', (route) => {
    if (route.request().method() === 'POST') {
      return route.fulfill({ status: 201, json: { token: longToken } })
    }
    return route.fulfill({ json: {
      keys: [
        { id: 1, name: '旧版 Key', scopes: null, created_at: new Date().toISOString() },
        { id: 2, name: '已撤销 Key', scopes: ['widget:read'], created_at: new Date().toISOString(), revoked_at: new Date().toISOString() },
      ],
    } })
  })
  await page.route('**/api/v1/system/info**', (route) => route.fulfill({ json: { version: 'v2.0.1', commit: 'test', built_at: new Date().toISOString(), repository: 'https://github.com/JudiLite/CDT-Monitor', release_url: 'https://github.com/JudiLite/CDT-Monitor/releases', latest_version: 'v2.0.1' } }))
  await page.route('**/api/v1/admin/passkeys', (route) => route.fulfill({ json: { passkeys: [] } }))
  let logsCleared = false
  await page.route('**/api/v1/logs**', (route) => {
    if (route.request().method() === 'DELETE') {
      logsCleared = true
      return route.fulfill({ json: { success: true } })
    }
    return route.fulfill({ json: { logs: logsCleared ? null : [{ id: 1, type: 'audit', message: '这是一条用于验证移动端日志布局不会向右溢出的较长运行日志内容', created_at: new Date().toISOString() }] } })
  })

  await page.setViewportSize({ width: 1440, height: 900 })
  await page.goto('/')
  const billing = page.locator('.account-billing')
  await expect(billing.getByText('本月费用')).toBeVisible()
  await expect(billing.getByText('¥23.46')).toBeVisible()
  await expect(billing.getByText('¥123.45')).toBeVisible()
  await expect(page.locator('.billing-row')).toHaveCount(0)
  await expect(page.locator('.metric--blue .metric-icon')).toBeVisible()
  await expect(page.locator('.metric--green .metric-icon')).toBeVisible()
  await expect(page.locator('.metric--cyan .metric-icon')).toBeVisible()
  await expect(page.locator('.metric--amber .metric-icon')).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('dashboard-billing-desktop.png'), fullPage: true })

  await page.getByRole('button', { name: '查看历史流量' }).click()
  await expect(page.locator('.chart-modal')).toBeVisible()
  const latestSample = page.locator('.chart-area .recharts-line-dot').last()
  await expect(latestSample).toBeVisible()
  await latestSample.hover()
  await expect(page.getByText('1.235')).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('history-chart-desktop.png'), fullPage: true })
  await page.getByRole('button', { name: '30 天' }).click()
  const sparseBar = page.locator('.recharts-bar-rectangle .recharts-rectangle').first()
  await expect(sparseBar).toBeVisible()
  const sparseBarWidth = await sparseBar.evaluate((element) => (element as unknown as SVGGraphicsElement).getBBox().width)
  expect(sparseBarWidth).toBeLessThanOrEqual(26)
  await page.screenshot({ path: testInfo.outputPath('history-chart-daily-sparse.png'), fullPage: true })
  await page.getByRole('button', { name: '24 小时' }).click()
  await page.locator('.chart-modal').getByRole('button', { name: '关闭' }).click()

  await page.getByRole('button', { name: '设置' }).click()
  const settingsSection = page.locator('.settings-section')
  const generalSectionWidth = await settingsSection.evaluate((element) => element.clientWidth)
  const refreshSelectDesktop = page.getByRole('combobox', { name: 'API 刷新间隔' })
  await expect(refreshSelectDesktop).toBeVisible()
  await refreshSelectDesktop.click()
  await expect(page.getByRole('listbox')).toBeVisible()
  await expect(page.getByRole('option', { name: '1 小时' })).toBeVisible()
  await page.keyboard.press('Escape')

  await page.getByRole('button', { name: '实例', exact: true }).click()
  const accountSectionWidth = await settingsSection.evaluate((element) => element.clientWidth)
  expect(Math.abs(accountSectionWidth - generalSectionWidth)).toBeLessThanOrEqual(1)
  const regionSelect = page.getByRole('combobox', { name: '地域' })
  await regionSelect.click()
  await page.getByRole('combobox', { name: '地域' }).fill('zhangjiakou')
  const zhangjiakou = page.getByRole('option', { name: /华北 3（张家口）.*cn-zhangjiakou/ })
  await expect(zhangjiakou).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('settings-region-select-desktop.png'), fullPage: true })
  await zhangjiakou.click()
  await expect(page.getByRole('combobox', { name: '地域' })).toContainText('cn-zhangjiakou')

  await page.getByRole('button', { name: '通知', exact: true }).click()
  const notificationSectionWidth = await settingsSection.evaluate((element) => element.clientWidth)
  expect(Math.abs(notificationSectionWidth - generalSectionWidth)).toBeLessThanOrEqual(1)
  await page.getByRole('button', { name: 'Webhook' }).click()
  await page.getByTitle('插入 #TITLE#').click()
  await expect(page.getByLabel('Body 模板')).toHaveValue('#TITLE#')
  await page.screenshot({ path: testInfo.outputPath('settings-select-desktop.png'), fullPage: true })

  await page.getByRole('button', { name: '关于' }).click()
  const aboutSectionWidth = await settingsSection.evaluate((element) => element.clientWidth)
  expect(Math.abs(aboutSectionWidth - generalSectionWidth)).toBeLessThanOrEqual(1)
  await expect(page.locator('.about-links a')).toHaveCount(4)
  await page.screenshot({ path: testInfo.outputPath('settings-about-desktop.png'), fullPage: true })
  await page.locator('.settings-panel').getByRole('button', { name: '关闭' }).click()

  for (const viewport of [{ width: 320, height: 720 }, { width: 390, height: 844 }]) {
    await page.setViewportSize(viewport)
    await page.getByRole('button', { name: '查看历史流量' }).click()
    const mobileChart = page.locator('.chart-area')
    await expect(mobileChart).toBeVisible()
    const mobileTicks = page.locator('.chart-area .recharts-xAxis .recharts-cartesian-axis-tick text')
    await expect(mobileTicks).toHaveCount(5)
    const chartBox = await mobileChart.boundingBox()
    const tickBoxes = await mobileTicks.evaluateAll((ticks) => ticks.map((tick) => tick.getBoundingClientRect().toJSON()))
    expect(chartBox).not.toBeNull()
    expect(tickBoxes[0].x).toBeGreaterThanOrEqual(chartBox!.x)
    expect(tickBoxes[tickBoxes.length - 1].x + tickBoxes[tickBoxes.length - 1].width).toBeLessThanOrEqual(chartBox!.x + chartBox!.width)
    await page.screenshot({ path: testInfo.outputPath(`history-chart-${viewport.width}.png`), fullPage: true })
    await page.locator('.chart-modal').getByRole('button', { name: '关闭' }).click()
  }

  await page.setViewportSize({ width: 320, height: 720 })
  await page.getByRole('button', { name: '菜单' }).click()
  await page.getByRole('button', { name: '设置' }).click()
  const panel = page.locator('.settings-panel')
  await expect(panel).toBeVisible()
  const panelBox = await panel.boundingBox()
  expect(panelBox).not.toBeNull()
  expect(panelBox!.x).toBeGreaterThan(0)
  expect(panelBox!.y).toBeGreaterThan(0)
  expect(panelBox!.x + panelBox!.width).toBeLessThan(390)
  expect(panelBox!.y + panelBox!.height).toBeLessThan(844)
  expect(await panel.evaluate((element) => getComputedStyle(element).borderRadius)).not.toBe('0px')

  await page.getByRole('button', { name: '实例', exact: true }).click()
  await page.getByRole('combobox', { name: '地域' }).click()
  await page.getByRole('combobox', { name: '地域' }).fill('cn-')
  const mobileRegionMenu = page.getByRole('listbox')
  await expect(mobileRegionMenu).toBeVisible()
  const mobileRegionMenuBox = await mobileRegionMenu.boundingBox()
  expect(mobileRegionMenuBox).not.toBeNull()
  expect(mobileRegionMenuBox!.x).toBeGreaterThanOrEqual(0)
  expect(mobileRegionMenuBox!.x + mobileRegionMenuBox!.width).toBeLessThanOrEqual(390)
  await page.screenshot({ path: testInfo.outputPath('settings-region-select-mobile.png'), fullPage: true })
  await page.keyboard.press('Escape')

  await page.getByRole('button', { name: '日志' }).click()
  await expect(page.getByRole('heading', { name: '运行日志' })).toBeVisible()
  const contentOverflow = await page.locator('.settings-content').evaluate((element) => element.scrollWidth - element.clientWidth)
  expect(contentOverflow).toBeLessThanOrEqual(1)
  await page.getByRole('button', { name: '清空' }).click()
  await expect(page.getByText('暂无日志')).toBeVisible()
  await expect(page.getByRole('heading', { name: '运行日志' })).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('settings-logs-mobile.png'), fullPage: true })

  await page.getByRole('button', { name: 'API Key' }).click()
  await expect(page.getByRole('heading', { name: 'API Key' })).toBeVisible()
  await expect(page.getByText('旧版 Key')).toBeVisible()
  await expect(page.getByText('已撤销 Key')).toHaveCount(0)
  await expect(page.getByText('未配置权限')).toBeVisible()
  await page.getByRole('button', { name: '创建 Key' }).click()
  await expect(page.getByText('仅显示一次')).toBeVisible()
  await expect(page.locator('.token-reveal code')).toHaveText(longToken)
  const tokenOverflow = await page.locator('.token-reveal').evaluate((element) => element.scrollWidth - element.clientWidth)
  expect(tokenOverflow).toBeLessThanOrEqual(1)
  await page.screenshot({ path: testInfo.outputPath('settings-api-key-mobile.png'), fullPage: true })
  await page.getByRole('button', { name: '关于' }).click()
  await expect(page.getByRole('heading', { name: '关于 CDT Monitor' })).toBeVisible()
  await expect(page.locator('.about-version b')).toHaveText('v2.0.1')
  const updateButton = page.getByRole('button', { name: '检查更新' })
  await expect(updateButton).toBeVisible()
  const updateButtonStyle = await updateButton.evaluate((element) => ({
    whiteSpace: getComputedStyle(element).whiteSpace,
    width: element.getBoundingClientRect().width,
    height: element.getBoundingClientRect().height,
  }))
  expect(updateButtonStyle.whiteSpace).toBe('nowrap')
  expect(updateButtonStyle.width).toBeGreaterThan(updateButtonStyle.height * 2)
  await expect(page.getByRole('link', { name: /NodeSeek/ })).toBeVisible()
  const faviconSources = await page.locator('.about-link__favicon img').evaluateAll((images) => images.map((image) => image.getAttribute('src')))
  expect(faviconSources).toEqual([
    'https://a.favicon.im/github.com',
    'https://a.favicon.im/qninq.cn',
    'https://a.favicon.im/nodeseek.com',
    'https://a.favicon.im/linux.do',
  ])
  await expect.poll(() => page.locator('.about-link__favicon').evaluateAll((items) => items.every((item) => item.getAttribute('data-state') !== 'loading'))).toBe(true)
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
  expect(overflow).toBeLessThanOrEqual(1)
  await page.screenshot({ path: testInfo.outputPath('settings-about-mobile.png'), fullPage: true })
  await panel.getByRole('button', { name: '关闭' }).click()
  await page.getByRole('button', { name: '菜单' }).click()
  await page.getByRole('button', { name: '管理员' }).click()
  await expect(page.getByRole('heading', { name: '管理员设置' })).toBeVisible()
  await expect(page.getByRole('button', { name: '创建 Passkey' })).toBeDisabled()
  const adminOverflow = await page.locator('.admin-settings-panel').evaluate((element) => element.scrollWidth - element.clientWidth)
  expect(adminOverflow).toBeLessThanOrEqual(1)
  await page.screenshot({ path: testInfo.outputPath('admin-settings-mobile.png'), fullPage: true })
})
