// Splits a comparison into track sections and measures each one. Pure functions, no DOM.
//
// Every array in a Compare shares the `distance` grid, so a section is just an
// inclusive index range [i0, i1]. Neighbouring sections share their boundary
// index (next.i0 === this.i1), which makes the section gains telescope: they
// sum to the gap at the line.
import type { Compare, Series } from './api';

export type Mode = 'corners' | 'sectors';
export type Kind = 'slow' | 'medium' | 'fast' | 'straight' | 'sector';

export interface Pair {
  a: number | null;
  b: number | null;
}

export interface Section {
  /** 1-based position in track order. */
  n: number;
  i0: number;
  i1: number;
  startM: number;
  endM: number;
  kind: Kind;
  /** "Section 4" or "Sector 2". */
  name: string;
  /** "Slow corner · 78 km/h", "Straight · 312 km/h". */
  label: string;
  /** Index the map label points at: the apex of a corner, else the middle. */
  anchor: number;
  /** Change of the delta across the section in seconds. Positive: B lost time to A here. */
  gain: number | null;
  /** Seconds each driver spent in the section. */
  time: Pair;
  entry: Pair;
  exit: Pair;
  min: Pair;
  top: Pair;
}

/** Below this a section counts as even. */
export const EVEN_S = 0.01;

const SLOW_KMH = 125;
const FAST_KMH = 200;
/** A corner is a speed minimum at least this far below the maxima either side. */
const PROMINENCE_KMH = 20;
const MIN_SECTION_M = 120;
/** A run from an apex to the next braking point at least this long gets its own "Straight" section. */
const STRAIGHT_RUN_M = 650;
const STRAIGHT_MIN_M = 400;
const MAX_CORNERS = 16;
const MIN_CORNERS = 5;

/** Value at i, or the nearest known value (searching outward) when i is a gap. */
export function near(s: Series, i: number): number | null {
  for (let k = 0; k < s.length; k++) {
    const lo = s[i - k];
    if (lo != null) return lo;
    const hi = s[i + k];
    if (hi != null) return hi;
  }
  return null;
}

/** Gaps filled by linear interpolation (held flat at the ends). Null when there is no data at all. */
export function fillGaps(s: Series): number[] | null {
  const out = new Array<number>(s.length);
  let prev = -1;
  for (let i = 0; i < s.length; i++) {
    const v = s[i];
    if (v == null) continue;
    for (let k = prev + 1; k < i; k++) {
      out[k] = prev < 0 ? v : out[prev]! + ((v - out[prev]!) * (k - prev)) / (i - prev);
    }
    out[i] = v;
    prev = i;
  }
  if (prev < 0) return null;
  for (let k = prev + 1; k < s.length; k++) out[k] = out[prev]!;
  return out;
}

/** Centred moving average over 2*half+1 samples. */
export function smooth(v: number[], half: number): number[] {
  if (half <= 0) return v.slice();
  return v.map((_, i) => {
    const lo = Math.max(0, i - half);
    const hi = Math.min(v.length - 1, i + half);
    let sum = 0;
    for (let k = lo; k <= hi; k++) sum += v[k]!;
    return sum / (hi - lo + 1);
  });
}

export interface Turn {
  i: number;
  type: 'min' | 'max';
}

/**
 * Alternating maxima and minima that differ from their neighbours by at least
 * `threshold`. The first and last samples are never reported as extrema: the
 * lap starts and ends mid-straight.
 */
export function turningPoints(v: number[], threshold: number): Turn[] {
  const out: Turn[] = [];
  let dir = 0; // +1 looking for a max, -1 looking for a min, 0 not known yet
  let hi = 0;
  let lo = 0;
  for (let i = 1; i < v.length; i++) {
    const x = v[i]!;
    if (x > v[hi]!) hi = i;
    if (x < v[lo]!) lo = i;
    if (dir >= 0 && v[hi]! - x >= threshold) {
      if (hi > 0) out.push({ i: hi, type: 'max' });
      dir = -1;
      lo = i;
    } else if (dir <= 0 && x - v[lo]! >= threshold) {
      if (lo > 0) out.push({ i: lo, type: 'min' });
      dir = 1;
      hi = i;
    }
  }
  return out;
}

interface Span {
  i0: number;
  i1: number;
  /** Index of the slowest point of a corner; undefined for a straight. */
  apex?: number;
}

function cornerSpans(dist: number[], speed: number[], threshold: number): Span[] {
  const last = dist.length - 1;
  const turns = turningPoints(speed, threshold);
  const apexes = turns.filter((t) => t.type === 'min').map((t) => t.i);
  if (apexes.length === 0) return [{ i0: 0, i1: last }];

  // Cut at the fastest point between consecutive corners: each span is braking zone + corner + exit.
  const spans: Span[] = [];
  let from = 0;
  apexes.forEach((apex, k) => {
    const next = apexes[k + 1];
    let cut = last;
    if (next !== undefined) {
      cut = apex;
      for (let i = apex; i <= next; i++) if (speed[i]! > speed[cut]!) cut = i;
    }
    spans.push({ i0: from, i1: cut, apex });
    from = cut;
  });

  // Carve the long flat-out runs into their own sections.
  const out: Span[] = [];
  const lead = turns[0]!;
  const first = spans[0]!;
  if (lead.type === 'max' && dist[lead.i]! - dist[0]! >= STRAIGHT_MIN_M && lead.i < first.apex!) {
    out.push({ i0: 0, i1: lead.i });
    first.i0 = lead.i;
  }
  for (const s of spans) {
    const apex = s.apex!;
    if (dist[s.i1]! - dist[apex]! < STRAIGHT_RUN_M) {
      out.push(s);
      continue;
    }
    let vmax = speed[apex]!;
    for (let i = apex; i <= s.i1; i++) vmax = Math.max(vmax, speed[i]!);
    const level = speed[apex]! + 0.55 * (vmax - speed[apex]!);
    let start = apex;
    while (start < s.i1 && (dist[start]! - dist[apex]! < 150 || speed[start]! < level)) start++;
    if (dist[s.i1]! - dist[start]! >= STRAIGHT_MIN_M) {
      out.push({ i0: s.i0, i1: start, apex }, { i0: start, i1: s.i1 });
    } else {
      out.push(s);
    }
  }

  // Fold slivers into a neighbour, keeping the slower apex.
  for (let k = 0; k < out.length && out.length > 1; ) {
    const s = out[k]!;
    if (dist[s.i1]! - dist[s.i0]! >= MIN_SECTION_M) {
      k++;
      continue;
    }
    const into = k > 0 ? out[k - 1]! : out[k + 1]!;
    into.i0 = Math.min(into.i0, s.i0);
    into.i1 = Math.max(into.i1, s.i1);
    if (s.apex !== undefined && (into.apex === undefined || speed[s.apex]! < speed[into.apex]!)) into.apex = s.apex;
    out.splice(k, 1);
    if (k > 0) k--;
  }
  return out;
}

function stats(series: Series, i0: number, i1: number): { min: number | null; max: number | null; argmin: number } {
  let min: number | null = null;
  let max: number | null = null;
  let argmin = i0;
  for (let i = i0; i <= i1; i++) {
    const v = series[i];
    if (v == null) continue;
    if (min === null || v < min) {
      min = v;
      argmin = i;
    }
    if (max === null || v > max) max = v;
  }
  return { min, max, argmin };
}

const diff = (x: number | null, y: number | null): number | null => (x == null || y == null ? null : x - y);

function measure(d: Compare, spans: Span[], mode: Mode): Section[] {
  const { a: sa, b: sb } = d.channels.speed;
  return spans.map((s, k) => {
    const [stA, stB] = [stats(sa, s.i0, s.i1), stats(sb, s.i0, s.i1)];
    const kmh = (v: number | null): string => (v == null ? '' : ` · ${Math.round(v)} km/h`);
    let kind: Kind;
    let label: string;
    let anchor = Math.round((s.i0 + s.i1) / 2);
    if (mode === 'sectors') {
      kind = 'sector';
      label = `${Math.round(d.distance[s.i1]! - d.distance[s.i0]!).toLocaleString('en-US')} m`;
    } else if (s.apex === undefined) {
      kind = 'straight';
      label = `Straight${kmh(stA.max)}`;
    } else {
      // Label with the real minimum, not the smoothed one used for detection.
      const v = stA.min ?? 0;
      kind = v < SLOW_KMH ? 'slow' : v < FAST_KMH ? 'medium' : 'fast';
      label = `${kind === 'slow' ? 'Slow' : kind === 'medium' ? 'Medium' : 'Fast'} corner${kmh(stA.min)}`;
      anchor = stA.min == null ? s.apex : stA.argmin;
    }
    return {
      n: k + 1,
      i0: s.i0,
      i1: s.i1,
      startM: d.distance[s.i0]!,
      endM: d.distance[s.i1]!,
      kind,
      name: `${mode === 'sectors' ? 'Sector' : 'Section'} ${k + 1}`,
      label,
      anchor,
      gain: diff(near(d.delta, s.i1), near(d.delta, s.i0)),
      time: { a: diff(near(d.time.a, s.i1), near(d.time.a, s.i0)), b: diff(near(d.time.b, s.i1), near(d.time.b, s.i0)) },
      entry: { a: near(sa, s.i0), b: near(sb, s.i0) },
      exit: { a: near(sa, s.i1), b: near(sb, s.i1) },
      min: { a: stA.min, b: stB.min },
      top: { a: stA.max, b: stB.max },
    };
  });
}

/** Corner-by-corner sections from lap A's speed trace. They tile the whole lap. */
export function cornerSections(d: Compare): Section[] {
  const n = d.distance.length;
  if (n < 2) return [];
  const raw = fillGaps(d.channels.speed.a);
  if (!raw) return measure(d, [{ i0: 0, i1: n - 1 }], 'corners');
  const step = Math.max(1e-6, (d.distance[n - 1]! - d.distance[0]!) / (n - 1));
  const speed = smooth(raw, Math.round(12 / step)); // roughly a 25 m window

  // Busy or very flowing circuits: move the prominence until the count is readable.
  const count = (spans: Span[]): number => spans.filter((s) => s.apex !== undefined).length;
  let threshold = PROMINENCE_KMH;
  let spans = cornerSpans(d.distance, speed, threshold);
  while (count(spans) > MAX_CORNERS && threshold < 60) spans = cornerSpans(d.distance, speed, (threshold += 5));
  while (count(spans) < MIN_CORNERS && threshold > 10) spans = cornerSpans(d.distance, speed, (threshold -= 5));
  return measure(d, spans, 'corners');
}

/**
 * The three official sectors, cut where lap A's elapsed time reaches S1 and
 * S1+S2. Null when a sector time is missing or the cuts do not fall inside the lap.
 */
export function sectorSections(d: Compare): Section[] | null {
  const [s1, s2, s3] = d.a.lap.sectors_ms;
  const n = d.distance.length;
  if (s1 == null || s2 == null || s3 == null || n < 4) return null;
  const reach = (seconds: number): number => d.time.a.findIndex((t) => t != null && t >= seconds);
  const c1 = reach(s1 / 1000);
  const c2 = reach((s1 + s2) / 1000);
  if (c1 <= 0 || c2 <= c1 || c2 >= n - 1) return null;
  return measure(d, [{ i0: 0, i1: c1 }, { i0: c1, i1: c2 }, { i0: c2, i1: n - 1 }], 'sectors');
}

/** 'a' or 'b' when that driver was quicker through the section, else 'even'. */
export function winner(s: Section): 'a' | 'b' | 'even' {
  if (s.gain == null || Math.abs(s.gain) < EVEN_S) return 'even';
  return s.gain > 0 ? 'a' : 'b';
}
