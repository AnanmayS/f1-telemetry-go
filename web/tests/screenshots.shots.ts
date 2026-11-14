// Not a test of behaviour: writes reference screenshots to web/screenshots/.
// Run with: npm run shots
import { expect, test } from '@playwright/test';

const SHARE = '/?a=9492:1:fastest&b=9492:4:fastest';
const SECTION = `${SHARE}&view=corners&s=6`;
interface Shot {
  name: string;
  width: number;
  height: number;
  scheme: 'dark' | 'light';
  url: string;
}
const shots: Shot[] = [
  { name: 'desktop-dark', width: 1280, height: 900, scheme: 'dark', url: SHARE },
  { name: 'desktop-light', width: 1280, height: 900, scheme: 'light', url: SHARE },
  { name: 'desktop-section-dark', width: 1280, height: 900, scheme: 'dark', url: SECTION },
  { name: 'desktop-section-light', width: 1280, height: 900, scheme: 'light', url: SECTION },
  { name: 'mobile-section-dark', width: 390, height: 844, scheme: 'dark', url: SECTION },
];
// Extra views for checking a redesign by eye; written outside the repo.
const extra: Shot[] = [
  { name: 'extra-mobile-360-light', width: 360, height: 740, scheme: 'light', url: SHARE },
  { name: 'extra-sectors-dark', width: 1280, height: 900, scheme: 'dark', url: `${SHARE}&view=sectors&s=2` },
  { name: 'extra-teammates-dark', width: 1280, height: 900, scheme: 'dark', url: '/?a=9492:1:fastest&b=9492:11:fastest' },
  { name: 'extra-teammates-light', width: 1280, height: 900, scheme: 'light', url: '/?a=9492:1:fastest&b=9492:11:fastest' },
  { name: 'extra-monaco-dark', width: 1280, height: 900, scheme: 'dark', url: '/?a=9090:1:fastest&b=9090:14:fastest' },
  { name: 'extra-baku-light', width: 1280, height: 900, scheme: 'light', url: '/?a=9064:16:fastest&b=9064:1:fastest' },
  { name: 'extra-tablet-dark', width: 820, height: 1100, scheme: 'dark', url: SECTION },
];
if (process.env.SHOTS_EXTRA) shots.push(...extra);

for (const s of shots) {
  test(s.name, async ({ browser }) => {
    const page = await browser.newPage({ viewport: { width: s.width, height: s.height }, colorScheme: s.scheme, deviceScaleFactor: 2 });
    await page.goto(s.url);
    await expect(page.locator('#charts .plot canvas')).toHaveCount(7);
    await expect(page.locator('#status')).toBeHidden();
    await expect(page.locator('#map .sec').first()).toBeVisible();
    // Put the cursor on the speed chart so the readout and the map dot are filled in.
    const over = page.locator('#charts .u-over').nth(1);
    await over.scrollIntoViewIfNeeded();
    const box = (await over.boundingBox())!;
    await page.mouse.move(box.x + box.width * 0.4, box.y + box.height / 2);
    await page.evaluate(() => window.scrollTo(0, 0));
    const dir = s.name.startsWith('extra-') ? (process.env.SHOTS_EXTRA ?? '.') : 'screenshots';
    await page.screenshot({ path: `${dir}/${s.name}.png`, fullPage: true });
    await page.close();
  });
}
