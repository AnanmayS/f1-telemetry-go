import { expect, test } from '@playwright/test';

test('predictions: a run race shows chances, likely range and the real result', async ({ page }) => {
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));
  await page.goto('/predict.html?race=9496'); // 2024 Japanese Grand Prix
  const rows = page.locator('#result tbody tr');
  await expect(rows).toHaveCount(20);
  await expect(page.locator('#status')).toBeHidden();
  await expect(page.locator('#race')).toHaveValue('9496');

  // Win chances add up to about 100%.
  const wins = await rows.locator('td.prob:nth-of-type(2) .val').allInnerTexts();
  const total = wins.reduce((sum, t) => sum + (t === '–' ? 0 : t.startsWith('<') ? 0.5 : Number(t.replace(/[>%]/g, ''))), 0);
  expect(total).toBeGreaterThan(92);
  expect(total).toBeLessThan(108);

  // Verstappen won from pole and the page says so.
  await expect(rows.first()).toContainText('VER');
  await expect(rows.first().locator('.res')).toHaveText('P1');
  await expect(page.locator('#lede')).toContainText('won');
  await expect(page.locator('#about')).toContainText('Grid order alone');
  await expect(page.locator('.foot')).toContainText('not betting odds');
  expect(errors).toEqual([]);
});

test('predictions: switching race updates the table and the link; unknown race falls back to the newest', async ({ page }) => {
  await page.goto('/predict.html?race=1');
  const select = page.locator('#race');
  await expect(page.locator('#result tbody tr').first()).toBeVisible();
  const newest = await select.inputValue();
  expect(newest).not.toBe('1');

  await select.selectOption('9496');
  await expect(page).toHaveURL(/race=9496/);
  await expect(page.locator('#result tbody tr')).toHaveCount(20);
  await expect(page).toHaveTitle(/2024 Japanese Grand Prix/);
});

test('predictions: fits a phone screen without sideways page scroll, and the nav links both pages', async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 800 });
  await page.goto('/predict.html?race=9496');
  await expect(page.locator('#result tbody tr')).toHaveCount(20);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await page.locator('.nav a', { hasText: 'Compare laps' }).click();
  await expect(page).toHaveURL(/\/$/);
  await expect(page.locator('.nav a[aria-current="page"]')).toHaveText('Compare laps');
});
