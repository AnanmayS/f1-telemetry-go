// The track overview: lap A's X/Y outline as an SVG, cut into sections that are
// coloured by whoever was quicker through them. Colours come from CSS variables
// (--col-a, --fill-b, --even), so the map follows the theme without a redraw.
import type { Series } from './api';
import { winner, type Section } from './sections';

export interface TrackMap {
  setSections(sections: Section[]): void;
  setSelected(n: number | null): void;
  setHover(n: number | null): void;
  /** Moves the position dot to a sample index; null hides it. */
  setIndex(i: number | null): void;
  destroy(): void;
}

export interface MapHandlers {
  onSelect(n: number): void;
  onHover(n: number | null): void;
  /** Pointer moved along the track: the nearest sample index, or null when it left. */
  onScrub(idx: number | null): void;
}

type Pt = [number, number];
const NS = 'http://www.w3.org/2000/svg';
const PAD = 30;
/** How close (px) the pointer must be to the track to count as on it. */
const REACH = 26;
const LABEL_OFFSET = 21;

function svg<K extends keyof SVGElementTagNameMap>(tag: K, attrs: Record<string, string | number> = {}): SVGElementTagNameMap[K] {
  const node = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, String(v));
  return node;
}

const f = (v: number): string => v.toFixed(1);

export function renderTrackMap(root: SVGSVGElement, xs: Series, ys: Series, handlers: MapHandlers): TrackMap {
  const raw: (Pt | null)[] = xs.map((x, i) => {
    const y = ys[i];
    return x == null || y == null ? null : [x, y];
  });
  const known = raw.filter((p): p is Pt => p !== null);
  let [x0, x1, y0, y1] = [Infinity, -Infinity, Infinity, -Infinity];
  for (const [x, y] of known) {
    x0 = Math.min(x0, x);
    x1 = Math.max(x1, x);
    y0 = Math.min(y0, y);
    y1 = Math.max(y1, y);
  }

  let sections: Section[] = [];
  let selected: number | null = null;
  let hover: number | null = null;
  let idx: number | null = null;
  /** Screen position of every sample (null where the lap has no position). */
  let pts: (Pt | null)[] = [];
  let groups = new Map<number, SVGGElement[]>();
  const dot = svg('circle', { class: 'dot', r: 6 });

  /** Polyline through samples i0..i1, broken wherever position data is missing. */
  const path = (i0: number, i1: number): string => {
    let d = '';
    let pen = false;
    for (let i = i0; i <= i1; i++) {
      const p = pts[i];
      if (!p) {
        pen = false;
        continue;
      }
      d += `${pen ? 'L' : 'M'}${f(p[0])} ${f(p[1])}`;
      pen = true;
    }
    return d;
  };

  /** Unit tangent at a sample, from its nearest known neighbours a few samples away. */
  const tangent = (i: number): Pt | null => {
    let a: Pt | null = null;
    let b: Pt | null = null;
    for (let k = 4; k >= 1 && !(a && b); k--) {
      a = a ?? pts[Math.max(0, i - k)] ?? null;
      b = b ?? pts[Math.min(pts.length - 1, i + k)] ?? null;
    }
    if (!a || !b) return null;
    const len = Math.hypot(b[0] - a[0], b[1] - a[1]);
    return len < 1e-6 ? null : [(b[0] - a[0]) / len, (b[1] - a[1]) / len];
  };

  /** Distance from a point to the nearest part of the track. */
  const clearance = (p: Pt): number => {
    let best = Infinity;
    for (let i = 0; i < pts.length; i += 2) {
      const q = pts[i];
      if (q) best = Math.min(best, Math.hypot(q[0] - p[0], q[1] - p[1]));
    }
    return best;
  };

  const nearest = (px: number, py: number): { i: number; dist: number } | null => {
    let best = -1;
    let bd = Infinity;
    for (let i = 0; i < pts.length; i++) {
      const q = pts[i];
      if (!q) continue;
      const dd = (q[0] - px) ** 2 + (q[1] - py) ** 2;
      if (dd < bd) {
        bd = dd;
        best = i;
      }
    }
    return best < 0 ? null : { i: best, dist: Math.sqrt(bd) };
  };

  const sectionAt = (i: number): Section | undefined => sections.find((s) => i <= s.i1);

  const placeDot = (): void => {
    const p = idx === null ? null : pts[idx];
    dot.style.display = p ? '' : 'none';
    if (p) {
      dot.setAttribute('cx', f(p[0]));
      dot.setAttribute('cy', f(p[1]));
    }
  };

  const mark = (): void => {
    root.classList.toggle('has-sel', selected !== null);
    for (const [n, els] of groups) {
      for (const g of els) {
        g.classList.toggle('sel', n === selected);
        g.classList.toggle('hover', n === hover);
      }
    }
  };

  const draw = (): void => {
    const w = root.clientWidth;
    const h = root.clientHeight;
    groups = new Map();
    if (w === 0 || h === 0) return;
    root.setAttribute('viewBox', `0 0 ${w} ${h}`);
    if (known.length < 2) {
      const t = svg('text', { class: 'nodata', x: w / 2, y: h / 2, 'text-anchor': 'middle' });
      t.textContent = 'No position data for this lap';
      root.replaceChildren(t);
      return;
    }

    // One scale for both axes keeps the circuit's shape; y is flipped (track y points up).
    const k = Math.min((w - 2 * PAD) / (x1 - x0 || 1), (h - 2 * PAD) / (y1 - y0 || 1));
    const ox = (w - (x1 - x0) * k) / 2;
    const oy = (h - (y1 - y0) * k) / 2;
    pts = raw.map((p) => (p ? [ox + (p[0] - x0) * k, h - oy - (p[1] - y0) * k] : null));

    const nodes: SVGElement[] = [svg('path', { class: 'track-base', d: path(0, pts.length - 1) })];
    const strongest = Math.max(1e-9, ...sections.map((s) => Math.abs(s.gain ?? 0)));
    const group = (s: Section, cls: string): SVGGElement => {
      const g = svg('g', { class: `${cls} w-${winner(s)}`, 'data-section': s.n });
      groups.set(s.n, [...(groups.get(s.n) ?? []), g]);
      return g;
    };

    for (const s of sections) {
      const g = group(s, 'sec');
      const strength = winner(s) === 'even' ? 0 : Math.abs(s.gain ?? 0) / strongest;
      g.style.setProperty('--strength', strength.toFixed(3));
      const d = path(s.i0, s.i1);
      g.append(svg('path', { class: 'sec-line', d }), svg('path', { class: 'sec-hit', d }));
      nodes.push(g);
    }

    // A notch across the track at each boundary keeps same-coloured neighbours apart.
    const across = (i: number, half: number, cls: string): SVGLineElement | null => {
      const p = pts[i];
      const t = tangent(i);
      if (!p || !t) return null;
      return svg('line', { class: cls, x1: f(p[0] - t[1] * half), y1: f(p[1] + t[0] * half), x2: f(p[0] + t[1] * half), y2: f(p[1] - t[0] * half) });
    };
    for (const s of sections.slice(1)) {
      const notch = across(s.i0, 8, 'notch');
      if (notch) nodes.push(notch);
    }

    // Start/finish line, and an arrow a little way down the road showing the direction of travel.
    const startIdx = raw.findIndex((p) => p !== null);
    const line = across(startIdx, 11, 'start');
    if (line) nodes.push(line);
    let run = 0;
    for (let i = startIdx + 1; i < pts.length; i++) {
      const [p, q] = [pts[i - 1], pts[i]];
      if (p && q) run += Math.hypot(q[0] - p[0], q[1] - p[1]);
      if (run < 26) continue;
      const t = tangent(i);
      if (q && t) {
        const deg = (Math.atan2(t[1], t[0]) * 180) / Math.PI;
        nodes.push(svg('path', { class: 'arrow', d: 'M7 0L-5 -6.5L-5 6.5Z', transform: `translate(${f(q[0])} ${f(q[1])}) rotate(${f(deg)})` }));
      }
      break;
    }

    // Section numbers sit beside the track, on whichever side has more room.
    const placed: Pt[] = [];
    for (const s of sections) {
      const p = pts[s.anchor] ?? pts[Math.round((s.i0 + s.i1) / 2)];
      const t = tangent(s.anchor);
      if (!p || !t) continue;
      const options: Pt[] = [];
      for (const off of [LABEL_OFFSET, LABEL_OFFSET + 9]) {
        for (const side of [1, -1]) options.push([p[0] - t[1] * off * side, p[1] + t[0] * off * side]);
      }
      const score = (c: Pt): number => {
        const crowd = Math.min(Infinity, ...placed.map((q) => Math.hypot(q[0] - c[0], q[1] - c[1])));
        const inside = c[0] >= 12 && c[0] <= w - 12 && c[1] >= 12 && c[1] <= h - 12;
        return Math.min(clearance(c), crowd - 6) - (inside ? 0 : 100);
      };
      const at = options.reduce((best, c) => (score(c) > score(best) + 1 ? c : best));
      placed.push(at);
      const g = group(s, 'lbl');
      g.setAttribute('transform', `translate(${f(at[0])} ${f(at[1])})`);
      const text = svg('text', { 'text-anchor': 'middle', dy: '0.35em' });
      text.textContent = String(s.n);
      g.append(svg('circle', { r: 10.5 }), text);
      nodes.push(g);
    }

    nodes.push(dot);
    root.replaceChildren(...nodes);
    mark();
    placeDot();
  };

  const local = (e: MouseEvent): Pt => {
    const r = root.getBoundingClientRect();
    return [e.clientX - r.left, e.clientY - r.top];
  };
  /** The section under the pointer: a number label, else the nearest stretch of track. */
  const target = (e: MouseEvent): { n: number; i: number | null } | null => {
    const lbl = (e.target as Element).closest?.('.lbl');
    if (lbl) return { n: Number(lbl.getAttribute('data-section')), i: null };
    const hit = nearest(...local(e));
    const s = hit && hit.dist <= REACH ? sectionAt(hit.i) : undefined;
    return s && hit ? { n: s.n, i: hit.i } : null;
  };
  const onMove = (e: PointerEvent): void => {
    if (e.pointerType === 'touch') return; // a touch is a tap or a page scroll, not a scrub
    const t = target(e);
    root.classList.toggle('near', t !== null);
    handlers.onHover(t ? t.n : null);
    if (t?.i != null) handlers.onScrub(t.i);
  };
  const onLeave = (): void => {
    root.classList.remove('near');
    handlers.onHover(null);
    handlers.onScrub(null);
  };
  const onClick = (e: MouseEvent): void => {
    const t = target(e);
    if (t) handlers.onSelect(t.n);
  };
  root.addEventListener('pointermove', onMove);
  root.addEventListener('pointerleave', onLeave);
  root.addEventListener('click', onClick);

  const ro = new ResizeObserver(draw);
  ro.observe(root);
  draw();

  return {
    setSections(next) {
      sections = next;
      selected = hover = null;
      draw();
    },
    setSelected(n) {
      selected = n;
      mark();
    },
    setHover(n) {
      hover = n;
      mark();
    },
    setIndex(i) {
      idx = i;
      placeDot();
    },
    destroy() {
      ro.disconnect();
      root.removeEventListener('pointermove', onMove);
      root.removeEventListener('pointerleave', onLeave);
      root.removeEventListener('click', onClick);
      root.replaceChildren();
    },
  };
}
