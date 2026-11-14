// Not a test of behaviour: writes reference screenshots to web/screenshots/.
// Run with: npm run shots
import { expect, test } from '@playwright/test';

const SHARE = '/?a=9492:1:fastest&b=9492:4:fastest';
const shots = [
  { name: 'desktop', width: 1280, height: 900 },
  { name: 'mobile', width: 390, height: 844 },
  { name: 'mobile-360', width: 360, height: 740 },
];

for (const s of shots) {
  for (const scheme of ['dark', 'light'] as const) {
    test(`${s.name} ${scheme}`, async ({ browser }) => {
      const page = await browser.newPage({ viewport: { width: s.width, height: s.height }, colorScheme: scheme, deviceScaleFactor: 2 });
      await page.goto(SHARE);
      await expect(page.locator('#charts .plot canvas')).toHaveCount(7);
      await expect(page.locator('#status')).toBeHidden();
      const over = page.locator('#charts .u-over').nth(1);
      if (s.width < 900) await over.scrollIntoViewIfNeeded();
      const box = (await over.boundingBox())!;
      await page.mouse.move(box.x + box.width * 0.4, box.y + box.height / 2);
      await page.screenshot({ path: `screenshots/${s.name}-${scheme}.png`, fullPage: s.width >= 900 });
      await page.close();
    });
  }
}
