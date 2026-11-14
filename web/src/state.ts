// URL <-> selection, plus small formatting helpers shared by the views.
import { refString, type LapRef } from './api';

export interface Selection {
  a: LapRef;
  b: LapRef;
}

export function parseRef(s: string | null): LapRef | null {
  const m = s?.match(/^(\d+):(\d+):(\d+|fastest)$/);
  if (!m) return null;
  return { session: Number(m[1]), driver: Number(m[2]), lap: m[3] === 'fastest' ? 'fastest' : Number(m[3]) };
}

export function readUrl(): { a: LapRef | null; b: LapRef | null } {
  const q = new URLSearchParams(location.search);
  return { a: parseRef(q.get('a')), b: parseRef(q.get('b')) };
}

/** Colons are legal in a query string; keep them unescaped so links stay readable. */
export const shareQuery = (s: Selection): string => `?a=${refString(s.a)}&b=${refString(s.b)}`;

export function writeUrl(s: Selection): void {
  history.replaceState(null, '', location.pathname + shareQuery(s));
}

export const shareUrl = (s: Selection): string => location.origin + location.pathname + shareQuery(s);

/** 88197 -> "1:28.197" */
export function fmtLap(ms: number | null | undefined): string {
  if (ms == null) return 'no time';
  const m = Math.floor(ms / 60000);
  const s = (ms - m * 60000) / 1000;
  return `${m}:${s.toFixed(3).padStart(6, '0')}`;
}

export function fmtDelta(s: number | null | undefined): string {
  if (s == null) return '–';
  return `${s > 0 ? '+' : s < 0 ? '−' : ''}${Math.abs(s).toFixed(3)}`;
}

/** Tiny DOM builder. */
export function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  props: Partial<HTMLElementTagNameMap[K]> & { class?: string } = {},
  ...children: (Node | string)[]
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  const { class: cls, ...rest } = props;
  if (cls) node.className = cls;
  Object.assign(node, rest);
  node.append(...children);
  return node;
}

export interface Colours {
  a: string;
  b: string;
  /** True when both laps share a colour, so B is drawn dashed. */
  dashB: boolean;
}

function rgb(hex: string): [number, number, number] {
  const n = parseInt(hex.slice(1), 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}
const hex = (c: number[]): string => '#' + c.map((v) => Math.round(v).toString(16).padStart(2, '0')).join('');
function luminance(c: number[]): number {
  const [r, g, b] = c.map((v) => {
    const x = v / 255;
    return x <= 0.03928 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4;
  }) as [number, number, number];
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}
const contrast = (x: number[], y: number[]): number => {
  const [hi, lo] = [luminance(x), luminance(y)].sort((p, q) => q - p) as [number, number];
  return (hi + 0.05) / (lo + 0.05);
};

/** Team colour, nudged toward the text colour until it is readable on the page background. */
function legible(colour: string, fallback: string, bg: string, fg: string): string {
  const base = /^[0-9a-f]{6}$/i.test(colour) ? '#' + colour.toLowerCase() : fallback;
  let c = rgb(base);
  const [b, f] = [rgb(bg), rgb(fg)];
  for (let i = 0; i < 8 && contrast(c, b) < 3; i++) c = c.map((v, k) => v + (f[k]! - v) * 0.2) as typeof c;
  return hex(c);
}

export function cssVar(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

export function pickColours(a: string, b: string): Colours {
  const bg = cssVar('--bg');
  const fg = cssVar('--text');
  const ca = legible(a, cssVar('--fallback-a'), bg, fg);
  const cb = legible(b, cssVar('--fallback-b'), bg, fg);
  const [x, y] = [rgb(ca), rgb(cb)];
  const dist = Math.hypot(x[0] - y[0], x[1] - y[1], x[2] - y[2]);
  return { a: ca, b: cb, dashB: dist < 40 };
}
