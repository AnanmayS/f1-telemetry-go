// Small X/Y outline of lap A with a dot at the hovered distance.
import type { Series } from './api';
import { cssVar } from './state';

export interface TrackMap {
  setIndex(i: number | null): void;
  destroy(): void;
}

export function renderTrackMap(canvas: HTMLCanvasElement, xs: Series, ys: Series, dotColour: string): TrackMap {
  const pts: ([number, number] | null)[] = xs.map((x, i) => {
    const y = ys[i];
    return x == null || y == null ? null : [x, y];
  });
  const known = pts.filter((p): p is [number, number] => p !== null);
  let idx: number | null = null;

  const draw = (): void => {
    const w = canvas.clientWidth;
    const h = canvas.clientHeight;
    const dpr = devicePixelRatio || 1;
    if (canvas.width !== Math.round(w * dpr) || canvas.height !== Math.round(h * dpr)) {
      canvas.width = Math.round(w * dpr);
      canvas.height = Math.round(h * dpr);
    }
    const ctx = canvas.getContext('2d');
    if (!ctx || w === 0 || h === 0) return;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);
    if (known.length < 2) {
      ctx.fillStyle = cssVar('--muted');
      ctx.font = '12px system-ui, sans-serif';
      ctx.textAlign = 'center';
      ctx.fillText('No position data', w / 2, h / 2);
      return;
    }

    let [x0, x1, y0, y1] = [Infinity, -Infinity, Infinity, -Infinity];
    for (const [x, y] of known) {
      x0 = Math.min(x0, x);
      x1 = Math.max(x1, x);
      y0 = Math.min(y0, y);
      y1 = Math.max(y1, y);
    }
    // One scale for both axes keeps the circuit's shape; y is flipped (track y points up).
    const pad = 8;
    const k = Math.min((w - 2 * pad) / (x1 - x0 || 1), (h - 2 * pad) / (y1 - y0 || 1));
    const ox = (w - (x1 - x0) * k) / 2;
    const oy = (h - (y1 - y0) * k) / 2;
    const at = ([x, y]: [number, number]): [number, number] => [ox + (x - x0) * k, h - oy - (y - y0) * k];

    ctx.lineWidth = 2;
    ctx.lineJoin = 'round';
    ctx.lineCap = 'round';
    ctx.strokeStyle = cssVar('--muted');
    ctx.beginPath();
    let pen = false;
    for (const p of pts) {
      if (!p) {
        pen = false; // a gap in the data breaks the line
        continue;
      }
      const [px, py] = at(p);
      if (pen) ctx.lineTo(px, py);
      else ctx.moveTo(px, py);
      pen = true;
    }
    ctx.stroke();

    // Start/finish marker.
    const [sx, sy] = at(known[0]!);
    ctx.fillStyle = cssVar('--text');
    ctx.fillRect(sx - 1.5, sy - 5, 3, 10);

    const p = idx === null ? null : pts[idx];
    if (p) {
      const [px, py] = at(p);
      ctx.beginPath();
      ctx.arc(px, py, 5, 0, Math.PI * 2);
      ctx.fillStyle = dotColour;
      ctx.fill();
      ctx.lineWidth = 2;
      ctx.strokeStyle = cssVar('--bg');
      ctx.stroke();
    }
  };

  const ro = new ResizeObserver(draw);
  ro.observe(canvas);
  draw();
  return {
    setIndex(i) {
      idx = i;
      draw();
    },
    destroy: () => ro.disconnect(),
  };
}
