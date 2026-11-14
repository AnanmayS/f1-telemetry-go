// Stacked uPlot charts sharing one distance axis and one cursor.
import uPlot from 'uplot';
import 'uplot/dist/uPlot.min.css';
import type { Compare, Series } from './api';
import { cssVar, el, type Colours } from './state';

export interface Charts {
  destroy(): void;
}

interface Spec {
  title: string;
  unit: string;
  height: number;
  data: Series[];
  /** Fixed y range; omitted means fit the data. */
  range?: [number, number];
  stepped?: boolean;
  /** On/off channel drawn as two filled lanes, A above B, so overlapping laps stay readable. */
  lanes?: boolean;
  ticks?: (v: number) => string;
  splits?: number[];
}

const AXIS_W = 46;
const SYNC_KEY = 'ghostlap';
const stepped = uPlot.paths.stepped!({ align: 1 });

const lane = (s: Series, base: number): Series => s.map((v) => (v == null ? null : base + (v >= 0.5 ? 0.8 : 0)));

function specs(d: Compare, labelA: string, labelB: string): Spec[] {
  const ch = d.channels;
  const laneTicks = (v: number): string => (v > 1 ? labelA : labelB);
  return [
    { title: 'Delta', unit: `s · above zero: ${labelB} is behind ${labelA}`, height: 150, data: [d.delta] },
    { title: 'Speed', unit: 'km/h', height: 200, data: [ch.speed.a, ch.speed.b] },
    { title: 'Throttle', unit: '%', height: 110, data: [ch.throttle.a, ch.throttle.b], range: [-4, 104], splits: [0, 50, 100] },
    { title: 'Brake', unit: 'on / off', height: 64, data: [lane(ch.brake.a, 1.1), lane(ch.brake.b, 0.1)], range: [0, 2], lanes: true, stepped: true, splits: [0.5, 1.5], ticks: laneTicks },
    { title: 'Gear', unit: '', height: 110, data: [ch.gear.a, ch.gear.b], range: [0.5, 8.5], stepped: true, splits: [2, 4, 6, 8] },
    { title: 'RPM', unit: '', height: 120, data: [ch.rpm.a, ch.rpm.b], ticks: (v) => (v >= 1000 ? `${v / 1000}k` : String(v)) },
    { title: 'DRS', unit: 'open / closed', height: 64, data: [lane(ch.drs.a, 1.1), lane(ch.drs.b, 0.1)], range: [0, 2], lanes: true, stepped: true, splits: [0.5, 1.5], ticks: laneTicks },
  ];
}

export function renderCharts(
  root: HTMLElement,
  d: Compare,
  colours: Colours,
  labels: { a: string; b: string },
  onHover: (idx: number) => void,
): Charts {
  const muted = cssVar('--muted');
  const grid = cssVar('--grid');
  const text = cssVar('--text');
  const font = '11px system-ui, -apple-system, "Segoe UI", sans-serif';
  const plots: uPlot[] = [];
  let syncing = false;
  let lastIdx = -1;

  const hover = (u: uPlot): void => {
    const i = u.cursor.idx;
    if (i != null && i !== lastIdx) onHover((lastIdx = i));
  };
  const syncZoom = (u: uPlot, key: string): void => {
    const { min, max } = u.scales.x!;
    if (key !== 'x' || syncing || min == null || max == null) return;
    syncing = true;
    for (const o of plots) if (o !== u) o.setScale('x', { min, max });
    syncing = false;
  };
  const zeroLine = (u: uPlot): void => {
    const y = Math.round(u.valToPos(0, 'y', true)) + 0.5;
    const { ctx, bbox } = u;
    if (y < bbox.top || y > bbox.top + bbox.height) return;
    ctx.save();
    ctx.strokeStyle = muted;
    ctx.lineWidth = devicePixelRatio;
    ctx.beginPath();
    ctx.moveTo(bbox.left, y);
    ctx.lineTo(bbox.left + bbox.width, y);
    ctx.stroke();
    ctx.restore();
  };

  root.replaceChildren();
  const width = (): number => Math.max(280, root.clientWidth);
  const list = specs(d, labels.a, labels.b);

  list.forEach((s, i) => {
    const last = i === list.length - 1;
    const isDelta = i === 0;
    const holder = el('div', { class: 'plot' });
    root.append(
      el('section', { class: 'chart' }, el('h2', {}, s.title, ' ', el('small', {}, s.unit)), holder),
    );

    const line = (stroke: string, k: number): uPlot.Series => ({
      stroke,
      width: 1.5,
      spanGaps: false,
      points: { show: false },
      ...(s.stepped ? { paths: stepped } : {}),
      ...(s.lanes ? { fill: stroke + '55', fillTo: k === 0 ? 1.1 : 0.1 } : {}),
      ...(k === 1 && colours.dashB ? { dash: [6, 4] } : {}),
    });

    const opts: uPlot.Options = {
      width: width(),
      height: s.height + (last ? 26 : 0),
      padding: [6, 10, last ? 0 : 7, 0],
      legend: { show: false },
      cursor: {
        y: false,
        sync: { key: SYNC_KEY },
        drag: { x: true, y: false },
        points: { show: !s.lanes },
      },
      scales: {
        x: { time: false },
        y: {
          range: s.range ?? ((_u, min, max) => {
            // Fit the data, always keeping zero in view on the delta chart.
            const lo = isDelta ? Math.min(min, 0) : min;
            const hi = isDelta ? Math.max(max, 0) : max;
            const pad = (hi - lo || 1) * 0.08;
            return [lo - pad, hi + pad];
          }),
        },
      },
      axes: [
        {
          stroke: muted,
          font,
          size: last ? 26 : 0,
          grid: { stroke: grid, width: 1 },
          ticks: { show: false },
          values: (_u, v) => v.map((x) => (last ? String(x) : '')),
        },
        {
          stroke: muted,
          font,
          size: AXIS_W,
          grid: { stroke: grid, width: 1, show: !s.lanes },
          ticks: { show: false },
          ...(s.splits ? { splits: () => s.splits! } : {}),
          ...(s.ticks ? { values: (_u, v) => v.map(s.ticks!) } : {}),
        },
      ],
      series: [{}, ...(isDelta ? [line(text, 0)] : [line(colours.a, 0), line(colours.b, 1)])],
      hooks: {
        setCursor: [hover],
        setScale: [syncZoom],
        ...(isDelta ? { draw: [zeroLine] } : {}),
      },
    };
    const u = new uPlot(opts, [d.distance, ...s.data] as uPlot.AlignedData, holder);
    plots.push(u);

    // uPlot only listens for the mouse; drive the shared cursor from touches too.
    const scrub = (e: TouchEvent): void => {
      const t = e.touches[0];
      if (!t) return;
      const r = u.over.getBoundingClientRect();
      const left = Math.min(Math.max(t.clientX - r.left, 0), r.width);
      for (const p of plots) p.setCursor({ left, top: p.over.clientHeight / 2 }, true);
    };
    u.over.addEventListener('touchstart', scrub, { passive: true });
    u.over.addEventListener('touchmove', scrub, { passive: true });
  });

  const ro = new ResizeObserver(() => {
    const w = width();
    for (const p of plots) if (p.width !== w) p.setSize({ width: w, height: p.height });
  });
  ro.observe(root);

  return {
    destroy() {
      ro.disconnect();
      for (const p of plots) p.destroy();
      root.replaceChildren();
    },
  };
}
