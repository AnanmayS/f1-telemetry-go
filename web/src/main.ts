import './style.css';
import { api, ApiError, CHANNELS, type Channel, type Compare, type CompareSide } from './api';
import { renderCharts, type Charts } from './charts';
import { createPicker, type Picker } from './picker';
import { el, fmtDelta, fmtLap, pickColours, readUrl, shareUrl, writeUrl, type Selection } from './state';
import { renderTrackMap, type TrackMap } from './trackmap';

const $ = <T extends HTMLElement>(id: string): T => document.getElementById(id) as T;
const form = $<HTMLFormElement>('picker');
const summary = $('summary');
const status = $('status');
const compare = $('compare');
const chartsRoot = $('charts');
const readout = $('readout');
const mapCanvas = $<HTMLCanvasElement>('map');

let picker: Picker | null = null;
let current: Compare | null = null;
let charts: Charts | null = null;
let map: TrackMap | null = null;
let request: AbortController | null = null;
let hoverIdx: number | null = null;
let showValues: (i: number) => void = () => {};

function setStatus(text: string, kind: 'loading' | 'error' | 'empty' | 'none'): void {
  status.hidden = kind === 'none';
  status.textContent = text;
  status.className = `status ${kind}`;
  status.setAttribute('role', kind === 'error' ? 'alert' : 'status');
}

function fail(e: unknown): void {
  if (e instanceof DOMException && e.name === 'AbortError') return;
  const friendly: Record<string, string> = {
    no_telemetry: 'That lap has no telemetry. Pick another lap.',
    no_fastest_lap: 'That driver has no timed lap with telemetry in this session. Pick a specific lap or another driver.',
    no_circuit_length: 'One of these sessions has no clean lap to measure the circuit, so its laps cannot be compared.',
    rate_limited: 'Too many requests. Wait a moment and try again.',
  };
  let msg = 'Something went wrong. Reload the page to try again.';
  if (e instanceof ApiError) msg = friendly[e.code] ?? e.message.charAt(0).toUpperCase() + e.message.slice(1);
  else console.error(e);
  compare.hidden = true;
  summary.hidden = true;
  setStatus(msg, 'error');
}

/** Short names for the two laps; disambiguated when the same driver is on both sides. */
function labels(d: Compare): { a: string; b: string } {
  const { a, b } = d;
  if (a.driver.acronym !== b.driver.acronym) return { a: a.driver.acronym, b: b.driver.acronym };
  const yr = (s: CompareSide): string => `${s.driver.acronym} ’${String(s.session.year).slice(2)}`;
  if (a.session.year !== b.session.year) return { a: yr(a), b: yr(b) };
  if (a.session_key !== b.session_key || a.lap.lap === b.lap.lap) return { a: `${a.driver.acronym} (A)`, b: `${b.driver.acronym} (B)` };
  return { a: `${a.driver.acronym} L${a.lap.lap}`, b: `${b.driver.acronym} L${b.lap.lap}` };
}

function renderSummary(d: Compare, names: { a: string; b: string }): void {
  const who = (id: 'a' | 'b', s: CompareSide): HTMLElement =>
    el(
      'div',
      { class: `who who-${id}` },
      el('i', { class: `swatch swatch-${id}` }),
      el('strong', {}, s.driver.acronym),
      ` ${s.driver.name} · Lap ${s.lap.lap} · `,
      el('span', { class: 'time' }, fmtLap(s.lap.duration_ms)),
      el('small', {}, `${s.session.year} ${s.session.meeting_name} · ${s.session.session_name}`),
    );

  const gap = d.final_delta_s;
  const verdict =
    gap == null ? 'No gap available' : gap === 0 ? 'Identical times' : `${names.b} ${gap > 0 ? 'slower' : 'faster'} than ${names.a}`;
  const copy = el('button', { type: 'button', id: 'copy', class: 'copy' }, 'Copy link');
  copy.onclick = async () => {
    const sel = picker?.selection();
    if (!sel) return;
    let ok = true;
    try {
      await navigator.clipboard.writeText(shareUrl(sel));
    } catch {
      ok = false;
    }
    copy.textContent = ok ? 'Link copied' : 'Copy the address bar instead';
    setTimeout(() => (copy.textContent = 'Copy link'), 2000);
  };

  const note = !d.same_circuit
    ? 'These laps are from different circuits, so the delta and overlays are only a rough guide.'
    : !d.aligned
      ? 'The delta is approximate: one of these laps has no official time or has incomplete telemetry.'
      : '';

  summary.replaceChildren(
    who('a', d.a),
    who('b', d.b),
    el('div', { class: 'gap' }, el('span', {}, 'Gap'), el('strong', { id: 'gap' }, `${fmtDelta(gap)} s`), el('small', {}, verdict)),
    copy,
    ...(note ? [el('p', { class: 'note' }, note)] : []),
  );
  summary.hidden = false;
}

const FORMAT: Record<Channel, { label: string; fmt: (v: number) => string }> = {
  speed: { label: 'Speed', fmt: (v) => v.toFixed(0) },
  throttle: { label: 'Throttle', fmt: (v) => `${v.toFixed(0)}%` },
  brake: { label: 'Brake', fmt: (v) => (v >= 0.5 ? 'on' : 'off') },
  gear: { label: 'Gear', fmt: (v) => (v < 0.5 ? 'N' : v.toFixed(0)) },
  rpm: { label: 'RPM', fmt: (v) => v.toFixed(0) },
  drs: { label: 'DRS', fmt: (v) => (v >= 0.5 ? 'open' : 'shut') },
};

/** Builds the readout once per comparison and returns a cheap per-hover updater. */
function renderReadout(d: Compare, names: { a: string; b: string }): (i: number) => void {
  const val = (cls: string): HTMLElement => el('dd', { class: cls }, '–');
  const dist = val('one');
  const delta = val('one');
  const rows = CHANNELS.map((c) => ({ c, a: val('va'), b: val('vb') }));
  readout.replaceChildren(
    el('div', { class: 'cell head' }, el('dt', {}, 'At cursor'), el('dd', { class: 'va' }, names.a), el('dd', { class: 'vb' }, names.b)),
    el('div', { class: 'cell' }, el('dt', {}, 'Distance'), dist),
    el('div', { class: 'cell' }, el('dt', {}, 'Delta'), delta),
    ...rows.map((r) => el('div', { class: 'cell' }, el('dt', {}, FORMAT[r.c].label), r.a, r.b)),
  );
  return (i) => {
    const show = (v: number | null | undefined, f: (v: number) => string): string => (v == null ? '–' : f(v));
    dist.textContent = show(d.distance[i], (v) => `${v.toFixed(0)} m`);
    delta.textContent = show(d.delta[i], (v) => `${fmtDelta(v)} s`);
    for (const r of rows) {
      r.a.textContent = show(d.channels[r.c].a[i], FORMAT[r.c].fmt);
      r.b.textContent = show(d.channels[r.c].b[i], FORMAT[r.c].fmt);
    }
  };
}

function render(d: Compare): void {
  const colours = pickColours(d.a.driver.colour, d.b.driver.colour);
  const root = document.documentElement;
  root.style.setProperty('--col-a', colours.a);
  root.style.setProperty('--col-b', colours.b);
  root.classList.toggle('dash-b', colours.dashB);

  const names = labels(d);
  renderSummary(d, names);
  compare.hidden = false;
  compare.classList.remove('stale');

  showValues = renderReadout(d, names);
  map?.destroy();
  map = renderTrackMap(mapCanvas, d.channels.x.a, d.channels.y.a, colours.a);
  charts?.destroy();
  charts = renderCharts(chartsRoot, d, colours, names, (i) => {
    hoverIdx = i;
    showValues(i);
    map?.setIndex(i);
  });
  if (hoverIdx !== null && hoverIdx < d.distance.length) {
    showValues(hoverIdx);
    map.setIndex(hoverIdx);
  }
  setStatus('', 'none');
}

async function load(sel: Selection | null): Promise<void> {
  request?.abort();
  if (!sel) {
    compare.hidden = true;
    summary.hidden = true;
    setStatus('That driver has no laps with telemetry in this session. Pick another driver.', 'error');
    return;
  }
  writeUrl(sel);
  const ctl = (request = new AbortController());
  compare.classList.add('stale');
  setStatus('Loading comparison…', 'loading');
  try {
    const d = await api.compare(sel.a, sel.b, ctl.signal);
    if (ctl.signal.aborted) return;
    if (current && (current.distance.length !== d.distance.length || current.length_m !== d.length_m)) hoverIdx = null;
    current = d;
    render(d);
  } catch (e) {
    fail(e);
  }
}

async function boot(): Promise<void> {
  try {
    const [seasons, sessions] = await Promise.all([api.seasons(), api.sessions()]);
    if (seasons.length === 0 || sessions.length === 0) {
      setStatus('No seasons have been ingested yet. Run the ingest command, then reload this page.', 'empty');
      return;
    }
    picker = createPicker(form, sessions, (sel) => void load(sel), fail);
    const url = readUrl();
    await picker.init(url.a, url.b);
  } catch (e) {
    fail(e);
  }
}

// Chart colours are read from CSS, so redraw when the OS theme flips.
matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => current && !compare.hidden && render(current));

void boot();
