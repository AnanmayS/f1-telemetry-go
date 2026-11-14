import { expect, test, type Page } from '@playwright/test';
import type { Compare } from '../src/api';
import { cornerSections, sectorSections, turningPoints } from '../src/sections';

const SHARE = '/?a=9492:1:fastest&b=9492:4:fastest';
const QUERY = '?a=9492:1:fastest&b=9492:4:fastest';

async function ready(page: Page): Promise<void> {
  await expect(page.locator('#charts .plot canvas')).toHaveCount(7);
  await expect(page.locator('#status')).toBeHidden();
  await expect(page.locator('#map .sec').first()).toBeVisible();
}

/** The distance range the charts currently show. */
async function xRange(page: Page): Promise<[number, number]> {
  const c = page.locator('#charts');
  return [Number(await c.getAttribute('data-x-min')), Number(await c.getAttribute('data-x-max'))];
}

const search = (page: Page): string => new URL(page.url()).search;
const selectedRow = (page: Page) => page.locator('#sections [role=option][aria-selected=true]');

test('share link draws the track map in sections, with a matching list and legend', async ({ page }) => {
  await page.goto(SHARE);
  await ready(page);
  const count = await page.locator('#map .sec').count();
  expect(count).toBeGreaterThanOrEqual(6);
  expect(count).toBeLessThanOrEqual(20);
  await expect(page.locator('#map .lbl')).toHaveCount(count);
  await expect(page.locator('#sections [role=option]')).toHaveCount(count + 1); // plus "Whole lap"
  // Every section is drawn and coloured by its winner.
  const drawn = await page.locator('#map .sec .sec-line').evaluateAll((els) => els.every((e) => (e as SVGPathElement).getTotalLength() > 5));
  expect(drawn).toBe(true);
  expect(await page.locator('#map .sec.w-a').count()).toBeGreaterThan(0);
  expect(await page.locator('#map .sec.w-b').count()).toBeGreaterThan(0);
  await expect(page.locator('#map .start')).toHaveCount(1);
  await expect(page.locator('#map .arrow')).toHaveCount(1);
  await expect(page.locator('#legend')).toContainText('VER faster');
  await expect(page.locator('#legend')).toContainText('NOR faster');
  await expect(page.locator('#legend')).toContainText('even');
  // Whole lap to start with: lap-level numbers and the full distance range.
  await expect(selectedRow(page)).toContainText('Whole lap');
  await expect(page.locator('#detail')).toContainText('Lap time');
  await expect(page.locator('#detail')).toContainText('Biggest gain · VER');
  const [min, max] = await xRange(page);
  expect(min).toBe(0);
  expect(max).toBeGreaterThan(5700);
  // No "Turn N" naming: the sections are not the official corners.
  await expect(page.locator('#app')).not.toContainText(/Turn \d/);
});

test('clicking a section on the map or in the list selects it, updates the URL and zooms the charts', async ({ page }) => {
  await page.goto(SHARE);
  await ready(page);

  // On the track itself: the middle of section 3's line.
  const mid = await page.locator('#map .sec[data-section="3"] .sec-line').evaluate((e) => {
    const p = e as SVGPathElement;
    const at = p.getPointAtLength(p.getTotalLength() / 2);
    const r = p.ownerSVGElement!.getBoundingClientRect();
    return { x: r.left + at.x, y: r.top + at.y };
  });
  await page.mouse.move(mid.x, mid.y);
  await expect(page.locator('#sections [data-section="3"]')).toHaveClass(/hover/); // map hover highlights the row
  await page.mouse.click(mid.x, mid.y);
  await expect(page).toHaveURL(new RegExp(`\\${QUERY}&view=corners&s=3$`));
  await expect(selectedRow(page)).toHaveAttribute('data-section', '3');
  await expect(page.locator('#map .sec.sel')).toHaveAttribute('data-section', '3');
  await expect(page.locator('#detail h2')).toContainText('Section 3');
  await expect(page.locator('#detail')).toContainText('Time in section');
  await expect(page.locator('#detail')).toContainText('Entry speed');
  await expect(page.locator('#charts .chart h2').first()).toContainText('re-based to 0 at the section start');
  const three = await xRange(page);
  expect(three[0]).toBeGreaterThan(500);
  expect(three[1] - three[0]).toBeLessThan(2500);

  // A list row.
  await page.locator('#sections [data-section="6"]').hover();
  await expect(page.locator('#map .sec[data-section="6"]')).toHaveClass(/hover/); // row hover highlights the map
  await page.locator('#sections [data-section="6"]').click();
  await expect(page).toHaveURL(/&view=corners&s=6$/);
  await expect(page.locator('#detail h2')).toContainText('Section 6');
  const six = await xRange(page);
  expect(six[0]).toBeGreaterThan(three[0]);
  // The zoom matches the range printed in the detail header, plus a little padding.
  const [from, to] = (await page.locator('#detail h2 small').innerText()).match(/[\d,]+/g)!.map((v) => Number(v.replace(/,/g, '')));
  expect(six[0]).toBeLessThanOrEqual(from!);
  expect(six[0]).toBeGreaterThan(from! - 150);
  expect(six[1]).toBeGreaterThanOrEqual(to!);
  expect(six[1]).toBeLessThan(to! + 150);

  // A number label on the map, then prev / next.
  await page.locator('#map .lbl[data-section="2"]').click();
  await expect(page).toHaveURL(/&s=2$/);
  await page.locator('#next').click();
  await expect(page).toHaveURL(/&s=3$/);
  await page.locator('#prev').click();
  await page.locator('#prev').click();
  await expect(page).toHaveURL(/&s=1$/);
  await expect(page.locator('#prev')).toBeDisabled();

  // Whole lap resets everything.
  await page.locator('#whole').click();
  expect(search(page)).toBe(QUERY);
  await expect(selectedRow(page)).toContainText('Whole lap');
  await expect(page.locator('#map .sec.sel')).toHaveCount(0);
  await expect(page.locator('#detail h2')).toContainText('Whole lap');
  await expect(page.locator('#charts .chart h2').first()).not.toContainText('re-based');
  expect((await xRange(page))[0]).toBe(0);
  expect((await xRange(page))[1]).toBeGreaterThan(5700);

  // The "Whole lap" list entry does the same.
  await page.locator('#sections [data-section="4"]').click();
  await expect(page).toHaveURL(/&s=4$/);
  await page.locator('#sections [data-section="0"]').click();
  expect(search(page)).toBe(QUERY);
});

test('sectors mode splits the lap into the three official sectors', async ({ page }) => {
  await page.goto(SHARE);
  await ready(page);
  await page.locator('#mode-sectors').click();
  await expect(page.locator('#mode-sectors')).toHaveAttribute('aria-pressed', 'true');
  await expect(page.locator('#map .sec')).toHaveCount(3);
  await expect(page.locator('#sections [role=option]')).toHaveCount(4);
  await expect(page).toHaveURL(/&view=sectors$/);
  await page.locator('#sections [data-section="2"]').click();
  await expect(page).toHaveURL(/&view=sectors&s=2$/);
  await expect(page.locator('#detail h2')).toContainText('Sector 2');
  // VER's official S2 was 39.850 s; the telemetry cut lands within a sample of it.
  const time = await page.locator('#detail .stat').filter({ hasText: 'Time in sector' }).locator('dd.va').innerText();
  expect(Math.abs(parseFloat(time.replace(/^\D+/, '')) - 39.85)).toBeLessThan(0.2);
  const [min, max] = await xRange(page);
  expect(min).toBeGreaterThan(1500);
  expect(max).toBeLessThan(5200);

  await page.locator('#mode-corners').click();
  await expect(page.locator('#mode-corners')).toHaveAttribute('aria-pressed', 'true');
  expect(search(page)).toBe(QUERY);
  expect(await page.locator('#map .sec').count()).toBeGreaterThan(3);
});

test('sectors mode is disabled, with a reason, when a sector time is missing', async ({ page }) => {
  await page.route('**/api/compare*', async (route) => {
    const res = await route.fetch();
    const body = (await res.json()) as Compare;
    body.a.lap.sectors_ms = [null, 39850, 17570];
    await route.fulfill({ response: res, json: body });
  });
  await page.goto(`${SHARE}&view=sectors&s=2`);
  await ready(page);
  await expect(page.locator('#mode-sectors')).toBeDisabled();
  await expect(page.locator('#mode-note')).toContainText('Sectors are unavailable');
  await expect(page.locator('#mode-corners')).toHaveAttribute('aria-pressed', 'true');
  expect(await page.locator('#map .sec').count()).toBeGreaterThan(3);
});

test('a section link restores the selection, and Copy link includes it', async ({ page, context }) => {
  await page.goto(`${SHARE}&view=corners&s=5`);
  await ready(page);
  await expect(selectedRow(page)).toHaveAttribute('data-section', '5');
  await expect(page.locator('#map .sec.sel')).toHaveAttribute('data-section', '5');
  await expect(page.locator('#detail h2')).toContainText('Section 5');
  expect(search(page)).toBe(`${QUERY}&view=corners&s=5`);
  const range = await xRange(page);
  expect(range[0]).toBeGreaterThan(0);

  await page.locator('#copy').click();
  await expect(page.locator('#copy')).toHaveText('Link copied');
  const copied = await page.evaluate(() => navigator.clipboard.readText());
  expect(copied).toBe(`http://127.0.0.1:8090/${QUERY}&view=corners&s=5`);

  const sectors = await context.newPage();
  await sectors.goto(`${SHARE}&view=sectors&s=3`);
  await ready(sectors);
  await expect(sectors.locator('#detail h2')).toContainText('Sector 3');
  await expect(sectors.locator('#mode-sectors')).toHaveAttribute('aria-pressed', 'true');

  // A section number that does not exist falls back to the whole lap.
  await sectors.goto(`${SHARE}&view=corners&s=99`);
  await ready(sectors);
  await expect(sectors.locator('#detail h2')).toContainText('Whole lap');
  expect(search(sectors)).toBe(QUERY);

  // Picking different laps starts again from the whole lap.
  await page.locator('#change').click();
  await page.locator('#b-driver').selectOption('11');
  await expect(page).toHaveURL(/b=9492:11:fastest$/);
  await ready(page);
  await expect(page.locator('#detail h2')).toContainText('Whole lap');
});

test('the section list works from the keyboard', async ({ page }) => {
  await page.goto(SHARE);
  await ready(page);
  const whole = page.locator('#sections [data-section="0"]');
  await whole.focus();
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('ArrowDown');
  await expect(page.locator('#sections [data-section="2"]')).toBeFocused();
  await expect(page.locator('#map .sec[data-section="2"]')).toHaveClass(/hover/);
  expect(search(page)).toBe(QUERY); // moving is not selecting
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(/&view=corners&s=2$/);
  await expect(selectedRow(page)).toHaveAttribute('data-section', '2');
  await expect(page.locator('#sections [data-section="2"]')).toBeFocused();
  await page.keyboard.press('ArrowUp');
  await page.keyboard.press(' ');
  await expect(page).toHaveURL(/&s=1$/);
  await page.keyboard.press('Home');
  await expect(whole).toBeFocused();
  await page.keyboard.press('Enter');
  expect(search(page)).toBe(QUERY);
  // Only one option is in the tab order.
  expect(await page.locator('#sections [role=option][tabindex="0"]').count()).toBe(1);
});

test('sorting by biggest difference reorders the list but keeps the section numbers', async ({ page }) => {
  await page.goto(SHARE);
  await ready(page);
  const gains = (): Promise<number[]> =>
    page.locator('#sections [role=option]:not(.whole) .val b').evaluateAll((els) => els.map((e) => Math.abs(parseFloat(e.textContent!.replace('−', '-')))));
  const numbers = (): Promise<string[]> => page.locator('#sections [role=option]:not(.whole) .num').allInnerTexts();
  const inOrder = await numbers();
  expect(inOrder).toEqual(inOrder.map((_, i) => String(i + 1)));
  await page.locator('#sort').click();
  await expect(page.locator('#sort')).toHaveAttribute('aria-pressed', 'true');
  const sorted = await gains();
  expect(sorted).toEqual([...sorted].sort((x, y) => y - x));
  expect([...(await numbers())].sort((x, y) => Number(x) - Number(y))).toEqual(inOrder);
  await page.locator('#sort').click();
  expect(await numbers()).toEqual(inOrder);
});

test('hovering the track moves the chart crosshair and fills the readout', async ({ page }) => {
  await page.goto(SHARE);
  await ready(page);
  const at = await page.locator('#map .sec[data-section="4"] .sec-line').evaluate((e) => {
    const p = e as SVGPathElement;
    const q = p.getPointAtLength(p.getTotalLength() / 2);
    const r = p.ownerSVGElement!.getBoundingClientRect();
    return { x: r.left + q.x, y: r.top + q.y };
  });
  await page.mouse.move(at.x, at.y);
  await expect(page.locator('#readout .cell').filter({ hasText: 'Distance' }).locator('dd')).toHaveText(/^\d+ m$/);
  await expect(page.locator('#readout .cell').filter({ hasText: 'Speed' }).locator('dd.va')).toHaveText(/^\d+$/);
  await expect(page.locator('#map .dot')).toBeVisible();
  const cursors = await page.locator('#charts .u-cursor-x').evaluateAll((els) => els.map((e) => !e.classList.contains('u-off')));
  expect(cursors).toEqual(Array(7).fill(true));
});

test.describe('mobile', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('tapping a section on the map selects it, without horizontal overflow', async ({ page }) => {
    await page.goto(SHARE);
    await ready(page);
    const overflow = (): Promise<number> => page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(await overflow()).toBeLessThanOrEqual(0);
    const box = (await page.locator('#map .lbl[data-section="7"]').boundingBox())!;
    await page.touchscreen.tap(box.x + box.width / 2, box.y + box.height / 2);
    await expect(page).toHaveURL(/&s=7$/);
    await expect(page.locator('#detail h2')).toContainText('Section 7');
    // The selected chip is scrolled into view inside the strip, and the page still does not overflow.
    await expect(page.locator('#sections [data-section="7"]')).toBeInViewport({ ratio: 0.9 });
    expect(await overflow()).toBeLessThanOrEqual(0);
    for (const width of [360, 430]) {
      await page.setViewportSize({ width, height: 800 });
      // The charts resize from a ResizeObserver, a frame or two after the viewport changes.
      await expect.poll(overflow).toBeLessThanOrEqual(0);
    }
  });
});

// ---- Section maths, straight against the API (no browser) -------------------

const LAPS = [
  'a=9492:1:fastest&b=9492:4:fastest', // Suzuka
  'a=9090:1:fastest&b=9090:14:fastest', // Monaco
  'a=9064:16:fastest&b=9064:1:fastest', // Baku
];

for (const q of LAPS) {
  test(`sections tile the lap and their gains sum to the final delta (${q})`, async ({ request }) => {
    const res = await request.get(`/api/compare?${q}&channels=speed,x,y`);
    expect(res.ok()).toBe(true);
    const d = (await res.json()) as Compare;
    const last = d.distance.length - 1;
    const sectors = sectorSections(d);
    expect(sectors).not.toBeNull();
    expect(sectors).toHaveLength(3);
    const corners = cornerSections(d);
    expect(corners.length).toBeGreaterThanOrEqual(6);
    expect(corners.length).toBeLessThanOrEqual(20);
    expect(corners.some((s) => s.kind !== 'straight')).toBe(true);

    for (const secs of [corners, sectors!]) {
      expect(secs[0]!.i0).toBe(0);
      expect(secs[secs.length - 1]!.i1).toBe(last);
      secs.forEach((s, i) => {
        expect(s.n).toBe(i + 1);
        expect(s.i1).toBeGreaterThan(s.i0);
        if (i > 0) expect(s.i0).toBe(secs[i - 1]!.i1); // no gaps, no overlaps
        expect(s.anchor).toBeGreaterThanOrEqual(s.i0);
        expect(s.anchor).toBeLessThanOrEqual(s.i1);
        expect(s.name).not.toMatch(/Turn/);
      });
      const gain = secs.reduce((t, s) => t + s.gain!, 0);
      expect(gain).toBeCloseTo(d.delta[last]! - d.delta[0]!, 6);
      expect(gain).toBeCloseTo(d.final_delta_s!, 2);
      const timeA = secs.reduce((t, s) => t + s.time.a!, 0);
      expect(timeA).toBeCloseTo(d.a.lap.duration_ms! / 1000, 1);
    }
    // No slivers.
    for (const s of corners) expect(s.endM - s.startM).toBeGreaterThanOrEqual(100);
  });
}

test('turning points need the given prominence and ignore the ends', () => {
  const v = [300, 310, 200, 100, 180, 170, 185, 300, 290, 120, 250];
  expect(turningPoints(v, 25)).toEqual([
    { i: 1, type: 'max' },
    { i: 3, type: 'min' },
    { i: 7, type: 'max' },
    { i: 9, type: 'min' },
  ]);
  expect(turningPoints([100, 110, 105, 112, 108], 25)).toEqual([]);
});
