import { expect, test, type Page } from '@playwright/test';

const SHARE = '/?a=9492:1:fastest&b=9492:4:fastest';

/** Waits for the comparison, then checks every chart canvas has real pixels drawn on it. */
async function expectCharts(page: Page): Promise<void> {
  const canvases = page.locator('#charts .plot canvas');
  await expect(canvases).toHaveCount(7);
  await expect(page.locator('#status')).toBeHidden();
  const painted = await canvases.evaluateAll((els) =>
    (els as HTMLCanvasElement[]).map((c) => {
      const data = c.getContext('2d')!.getImageData(0, 0, c.width, c.height).data;
      for (let i = 3; i < data.length; i += 4) if (data[i] !== 0) return true;
      return false;
    }),
  );
  expect(painted).toEqual(Array(7).fill(true));
}

async function summaryText(page: Page): Promise<string> {
  return (await page.locator('#summary').innerText()).replace('Link copied', 'Copy link');
}

test('share link renders, hover fills the readout, copied link reopens the same comparison', async ({ page, context }) => {
  await page.goto(SHARE);
  await expectCharts(page);

  // Header summary: both drivers, lap times and the final gap.
  const summary = page.locator('#summary');
  await expect(summary).toContainText('VER');
  await expect(summary).toContainText('NOR');
  await expect(summary).toContainText('1:28.197');
  await expect(summary).toContainText('1:28.489');
  await expect(page.locator('#gap')).toHaveText('+0.292 s');
  expect(new URL(page.url()).search).toBe('?a=9492:1:fastest&b=9492:4:fastest');

  // Track map is drawn.
  const mapPainted = await page.locator('#map').evaluate((c) => {
    const cv = c as HTMLCanvasElement;
    return cv.getContext('2d')!.getImageData(0, 0, cv.width, cv.height).data.some((v) => v !== 0);
  });
  expect(mapPainted).toBe(true);

  // Hover: the readout shows both drivers' values and the delta at that distance.
  const readout = page.locator('#readout');
  await expect(readout).not.toContainText('km/h');
  const over = page.locator('#charts .u-over').nth(1);
  const box = (await over.boundingBox())!;
  await page.mouse.move(box.x + box.width * 0.4, box.y + box.height / 2);
  const cells = readout.locator('.cell');
  await expect(cells.filter({ hasText: 'Distance' }).locator('dd')).toHaveText(/^\d+ m$/);
  await expect(cells.filter({ hasText: 'Delta' }).locator('dd')).toHaveText(/^[+−]?\d\.\d{3} s$/);
  for (const name of ['Speed', 'RPM', 'Gear']) {
    const row = cells.filter({ hasText: name });
    await expect(row.locator('dd.va')).toHaveText(/^\d+$/);
    await expect(row.locator('dd.vb')).toHaveText(/^\d+$/);
  }
  await expect(cells.filter({ hasText: 'Throttle' }).locator('dd.va')).toHaveText(/^\d+%$/);
  await expect(cells.filter({ hasText: 'Brake' }).locator('dd.vb')).toHaveText(/^(on|off)$/);
  await expect(cells.filter({ hasText: 'DRS' }).locator('dd.va')).toHaveText(/^(open|shut)$/);

  // Every chart shows the shared crosshair.
  const cursors = await page.locator('#charts .u-cursor-x').evaluateAll((els) =>
    els.map((e) => !e.classList.contains('u-off') && getComputedStyle(e).display !== 'none'),
  );
  expect(cursors).toEqual(Array(7).fill(true));

  // Copy link -> open it in a new page -> same comparison.
  await page.locator('#copy').click();
  await expect(page.locator('#copy')).toHaveText('Link copied');
  const copied = await page.evaluate(() => navigator.clipboard.readText());
  expect(copied).toBe('http://127.0.0.1:8090/?a=9492:1:fastest&b=9492:4:fastest');

  const second = await context.newPage();
  await second.goto(copied);
  await expectCharts(second);
  expect(await summaryText(second)).toBe(await summaryText(page));
  for (const id of ['a-session', 'a-driver', 'a-lap', 'b-session', 'b-driver', 'b-lap']) {
    expect(await second.locator(`#${id}`).inputValue()).toBe(await page.locator(`#${id}`).inputValue());
  }
});

test('changing the picker updates the URL; explicit laps and other sessions survive a reload', async ({ page }) => {
  await page.goto(SHARE);
  await expectCharts(page);

  // B from the 2023 qualifying at the same circuit, on a specific (non-fastest) lap.
  await page.locator('#b-year').selectOption('2023');
  await expect(page).toHaveURL(/b=9169:\d+:fastest$/);
  await expectCharts(page);
  const lap = await page
    .locator('#b-lap option:not([disabled])')
    .evaluateAll((o) => (o as HTMLOptionElement[]).map((x) => x.value).find((v) => v !== 'fastest')!);
  await page.locator('#b-lap').selectOption(lap);
  await expect(page).toHaveURL(new RegExp(`a=9492:1:fastest&b=9169:\\d+:${lap}$`));
  await expectCharts(page);

  const url = page.url();
  const before = await summaryText(page);
  await page.reload();
  await expectCharts(page);
  expect(page.url()).toBe(url);
  expect(await summaryText(page)).toBe(before);
});

test('no selection in the URL falls back to a default comparison', async ({ page }) => {
  await page.goto('/');
  await expectCharts(page);
  await expect(page).toHaveURL(/\?a=\d+:\d+:fastest&b=\d+:\d+:fastest$/);
  const [a, b] = [await page.locator('#a-driver').inputValue(), await page.locator('#b-driver').inputValue()];
  expect(a).not.toBe(b);
});

test('API errors are shown, never a blank screen', async ({ page }) => {
  await page.route('**/api/compare*', (route) =>
    route.fulfill({ status: 404, contentType: 'application/json', body: '{"error":"lap has no car data","code":"no_telemetry"}' }),
  );
  await page.goto(SHARE);
  await expect(page.locator('#status')).toHaveText('That lap has no telemetry. Pick another lap.');
  await expect(page.locator('#picker')).toBeVisible();
});

test('empty store shows the empty state', async ({ page }) => {
  await page.route('**/api/seasons', (route) => route.fulfill({ json: { seasons: [] } }));
  await page.route('**/api/sessions', (route) => route.fulfill({ json: { sessions: [] } }));
  await page.goto('/');
  await expect(page.locator('#status')).toContainText('No seasons have been ingested yet');
});

test.describe('mobile', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('no horizontal overflow, and touch scrubbing fills the readout', async ({ page }) => {
    await page.goto(SHARE);
    await expectCharts(page);
    const overflow = (): Promise<number> =>
      page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(await overflow()).toBeLessThanOrEqual(0);

    const over = page.locator('#charts .u-over').nth(1);
    await over.scrollIntoViewIfNeeded();
    const box = (await over.boundingBox())!;
    await page.touchscreen.tap(box.x + box.width / 2, box.y + box.height / 2);
    await expect(page.locator('#readout .cell').filter({ hasText: 'Speed' }).locator('dd.va')).toHaveText(/^\d+$/);
    expect(await overflow()).toBeLessThanOrEqual(0);
  });

  test('no horizontal overflow at 360 px', async ({ page }) => {
    await page.setViewportSize({ width: 360, height: 740 });
    await page.goto(SHARE);
    await expectCharts(page);
    expect(await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)).toBeLessThanOrEqual(0);
  });
});

test('team-mates share a colour, so lap B is drawn dashed', async ({ page }) => {
  await page.goto('/?a=9492:1:fastest&b=9492:11:fastest');
  await expectCharts(page);
  await expect(page.locator('html')).toHaveClass(/dash-b/);
  await page.goto(SHARE);
  await expectCharts(page);
  await expect(page.locator('html')).not.toHaveClass(/dash-b/);
});
