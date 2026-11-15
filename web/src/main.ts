import './style.css';
import { api, ApiError, CHANNELS, type Channel, type Compare, type CompareSide } from './api';
import { renderCharts, type Charts } from './charts';
import { createPicker, type Picker } from './picker';
import { createSectionList } from './sectionlist';
import { cornerSections, EVEN_S, sectorSections, winner, type Mode, type Pair, type Section } from './sections';
import { el, fmtDelta, fmtLap, fmtM, pickColours, readUrl, shareUrl, writeUrl, type Selection, type View } from './state';
import { renderTrackMap, type TrackMap } from './trackmap';

const $ = <T extends Element = HTMLElement>(id: string): T => document.getElementById(id) as unknown as T;
const form = $<HTMLFormElement>('picker');
const summary = $('summary');
const status = $('status');
const compare = $('compare');
const chartsRoot = $('charts');
const readout = $('readout');
const mapRoot = $<SVGSVGElement>('map');
const legend = $('legend');
const detail = $('detail');
const barhead = $('barhead');
const modeNote = $('mode-note');
const sortBtn = $<HTMLButtonElement>('sort');
const modeBtn: Record<Mode, HTMLButtonElement> = {
  corners: $<HTMLButtonElement>('mode-corners'),
  sectors: $<HTMLButtonElement>('mode-sectors'),
};

type Names = { a: string; b: string };

let picker: Picker | null = null;
let current: Compare | null = null;
let names: Names = { a: 'A', b: 'B' };
let charts: Charts | null = null;
let map: TrackMap | null = null;
let request: AbortController | null = null;
let hoverIdx: number | null = null;
let showValues: (i: number) => void = () => {};

/** How the lap is split and which section is open; starts from the link, then follows the user. */
let view: View = readUrl().view;
let sections: Section[] = [];
/** A section from the link is kept for the first comparison only; a new pair of laps starts at the whole lap. */
let firstLoad = true;
let pickerOpen = false;
let failed = false;

const list = createSectionList($('sections'), {
  onSelect: (n) => select(n),
  onHover: (n) => hoverSection(n),
});

function setStatus(text: string, kind: 'loading' | 'error' | 'empty' | 'none'): void {
  status.hidden = kind === 'none';
  status.textContent = text;
  status.className = `status ${kind}`;
  status.setAttribute('role', kind === 'error' ? 'alert' : 'status');
}

/** The selects stay folded away behind "Change laps" unless there is nothing else to act on. */
function syncPicker(): void {
  form.hidden = picker === null || !(pickerOpen || failed);
  document.getElementById('change')?.setAttribute('aria-expanded', String(!form.hidden));
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
  showFailure(msg);
}

function showFailure(msg: string): void {
  compare.hidden = true;
  summary.hidden = true;
  failed = true;
  syncPicker();
  setStatus(msg, 'error');
}

/** Short names for the two laps; disambiguated when the same driver is on both sides. */
function labels(d: Compare): Names {
  const { a, b } = d;
  if (a.driver.acronym !== b.driver.acronym) return { a: a.driver.acronym, b: b.driver.acronym };
  const yr = (s: CompareSide): string => `${s.driver.acronym} ’${String(s.session.year).slice(2)}`;
  if (a.session.year !== b.session.year) return { a: yr(a), b: yr(b) };
  if (a.session_key !== b.session_key || a.lap.lap === b.lap.lap) return { a: `${a.driver.acronym} (A)`, b: `${b.driver.acronym} (B)` };
  return { a: `${a.driver.acronym} L${a.lap.lap}`, b: `${b.driver.acronym} L${b.lap.lap}` };
}

function renderSummary(d: Compare): void {
  const who = (id: 'a' | 'b', s: CompareSide): HTMLElement =>
    el(
      'div',
      { class: `who who-${id}` },
      el('i', { class: `swatch swatch-${id}` }),
      el('strong', {}, s.driver.acronym),
      ' ',
      el('span', { class: 'time' }, fmtLap(s.lap.duration_ms)),
      el('small', {}, `${s.driver.name} · ${s.session.year} ${s.session.meeting_name} · ${s.session.session_name} · Lap ${s.lap.lap}`),
    );

  const gap = d.final_delta_s;
  const verdict =
    gap == null ? 'No gap available' : gap === 0 ? 'Identical times' : `${names.b} ${gap > 0 ? 'slower' : 'faster'} than ${names.a}`;

  const change = el('button', { type: 'button', id: 'change' }, 'Change laps');
  change.setAttribute('aria-controls', 'picker');
  change.onclick = () => {
    pickerOpen = form.hidden !== false;
    syncPicker();
    if (pickerOpen) picker?.focus();
  };

  const copy = el('button', { type: 'button', id: 'copy', class: 'copy' }, 'Copy link');
  copy.onclick = async () => {
    const sel = picker?.selection();
    if (!sel) return;
    let ok = true;
    try {
      await navigator.clipboard.writeText(shareUrl(sel, view));
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
    el('div', { class: 'actions' }, change, copy),
    ...(note ? [el('p', { class: 'note' }, note)] : []),
  );
  summary.hidden = false;
}

function renderLegend(dashB: boolean): void {
  const item = (cls: string, text: string): HTMLElement => el('span', { class: 'key' }, el('i', { class: `chip ${cls}` }), text);
  legend.replaceChildren(
    item('chip-a', `${names.a} faster`),
    item('chip-b', `${names.b} faster`),
    item('chip-even', 'even'),
    el('span', { class: 'key more' }, 'Stronger colour: bigger gain'),
    ...(dashB ? [el('span', { class: 'key more' }, `Same team colour, so ${names.b} is shown in a neutral contrast colour here and dashed in the charts`)] : []),
  );
  barhead.replaceChildren(el('span', {}, `← ${names.a} faster`), el('span', {}, `${names.b} faster →`));
}

// ---- Section detail -------------------------------------------------------

const num = (v: number | null, digits: number, unit: string): string => (v == null ? '–' : `${v.toFixed(digits)} ${unit}`);
const sub = (p: Pair): number | null => (p.a == null || p.b == null ? null : p.b - p.a);

function stat(title: string, p: Pair, digits: number, unit: string): HTMLElement {
  const d = sub(p);
  return el(
    'div',
    { class: 'stat' },
    el('dt', {}, title),
    el('dd', { class: 'va' }, el('span', {}, names.a), num(p.a, digits, unit)),
    el('dd', { class: 'vb' }, el('span', {}, names.b), num(p.b, digits, unit)),
    el('dd', { class: 'df diff' }, d == null ? '–' : `${fmtDelta(d, digits)} ${unit}`),
  );
}

function topSpeed(d: Compare): Pair {
  const max = (s: (number | null)[]): number | null => s.reduce<number | null>((m, v) => (v != null && (m === null || v > m) ? v : m), null);
  return { a: max(d.channels.speed.a), b: max(d.channels.speed.b) };
}

function renderDetail(d: Compare): void {
  const focused = document.activeElement?.id;
  const s = view.section === null ? undefined : sections[view.section - 1];
  const button = (id: string, text: string, label: string, go: number | null, disabled: boolean): HTMLButtonElement => {
    const b = el('button', { type: 'button', id, disabled }, text);
    b.setAttribute('aria-label', label);
    b.onclick = () => select(go);
    return b;
  };
  const at = s?.n ?? 0;
  const unit = view.mode === 'sectors' ? 'sector' : 'section';
  const nav = el(
    'div',
    { class: 'nav' },
    button('prev', '‹ Prev', `Previous ${unit}`, at - 1, at <= 1),
    button('next', 'Next ›', `Next ${unit}`, at + 1, at >= sections.length),
    button('whole', 'Whole lap', 'Show the whole lap', null, !s),
  );

  let title: HTMLElement;
  let headline: HTMLElement;
  let stats: HTMLElement[];
  if (s) {
    const w = winner(s);
    title = el(
      'h2',
      {},
      s.name,
      el('span', {}, s.kind === 'sector' ? '' : s.label),
      el('small', {}, `${fmtM(s.startM)}–${fmtM(s.endM)} m · ${fmtM(s.endM - s.startM)} m long`),
    );
    headline =
      w === 'even'
        ? el('p', { class: 'headline' }, `Even through this ${unit} (under ${EVEN_S} s apart)`)
        : el(
            'p',
            { class: `headline w-${w}` },
            el('strong', {}, w === 'a' ? names.a : names.b),
            ` gained ${Math.abs(s.gain ?? 0).toFixed(3)} s on ${w === 'a' ? names.b : names.a} here`,
          );
    stats = [
      stat(`Time in ${unit}`, s.time, 3, 's'),
      stat('Entry speed', s.entry, 0, 'km/h'),
      stat('Minimum speed', s.min, 0, 'km/h'),
      stat('Exit speed', s.exit, 0, 'km/h'),
      stat('Top speed', s.top, 0, 'km/h'),
    ];
  } else {
    const gap = d.final_delta_s;
    title = el('h2', {}, 'Whole lap', el('small', {}, `${fmtM(d.length_m)} m · pick a ${unit} on the map or the list to zoom in`));
    headline =
      gap == null || gap === 0
        ? el('p', { class: 'headline' }, gap === 0 ? 'Identical lap times' : 'No gap available for these laps')
        : el(
            'p',
            { class: `headline w-${gap > 0 ? 'a' : 'b'}` },
            el('strong', {}, gap > 0 ? names.a : names.b),
            ` was ${Math.abs(gap).toFixed(3)} s quicker over the lap`,
          );
    const best = (side: 'a' | 'b'): HTMLElement => {
      const won = sections.filter((x) => winner(x) === side);
      const top = won.sort((x, y) => Math.abs(y.gain ?? 0) - Math.abs(x.gain ?? 0))[0];
      const jump = top && el('button', { type: 'button', class: 'link' }, `${top.name} · ${Math.abs(top.gain ?? 0).toFixed(3)} s`);
      if (jump) jump.onclick = () => select(top.n);
      return el(
        'div',
        { class: 'stat' },
        el('dt', {}, `Biggest gain · ${names[side]}`),
        el('dd', { class: `v${side}` }, jump ?? 'none'),
        el('dd', { class: 'df' }, top && top.kind !== 'sector' ? top.label : ''),
      );
    };
    const ms = (v: number | null): number | null => (v == null ? null : v / 1000);
    const lap = stat('Lap time', { a: ms(d.a.lap.duration_ms), b: ms(d.b.lap.duration_ms) }, 3, 's');
    lap.querySelector('.va')!.lastChild!.textContent = fmtLap(d.a.lap.duration_ms);
    lap.querySelector('.vb')!.lastChild!.textContent = fmtLap(d.b.lap.duration_ms);
    stats = [lap, stat('Top speed', topSpeed(d), 0, 'km/h'), best('a'), best('b')];
  }
  detail.replaceChildren(el('div', { class: 'dhead' }, title, nav), headline, el('dl', { class: 'stats' }, ...stats));
  // Keep the keyboard where it was when a nav button re-rendered the panel.
  if (focused === 'prev' || focused === 'next' || focused === 'whole') {
    const again = document.getElementById(focused) as HTMLButtonElement | null;
    (again && !again.disabled ? again : (document.getElementById(focused === 'whole' ? 'next' : 'whole') as HTMLButtonElement | null))?.focus();
  }
}

// ---- View state: mode and selected section --------------------------------

function applySelection(): void {
  if (!current) return;
  const s = view.section === null ? undefined : sections[view.section - 1];
  map?.setSelected(view.section);
  list.setSelected(view.section);
  charts?.setView(s ? { i0: s.i0, i1: s.i1 } : null);
  renderDetail(current);
  const sel = picker?.selection();
  if (sel) writeUrl(sel, view);
}

function select(n: number | null): void {
  view = { mode: view.mode, section: n !== null && n >= 1 && n <= sections.length ? n : null };
  applySelection();
}

function hoverSection(n: number | null): void {
  map?.setHover(n);
  list.setHover(n);
}

/** Recomputes the sections for the current mode and redraws everything that depends on them. */
function applySections(d: Compare): void {
  const sectors = sectorSections(d);
  if (view.mode === 'sectors' && !sectors) view = { mode: 'corners', section: null };
  sections = view.mode === 'sectors' && sectors ? sectors : cornerSections(d);
  if (view.section !== null && view.section > sections.length) view = { mode: view.mode, section: null };

  for (const m of ['corners', 'sectors'] as const) modeBtn[m].setAttribute('aria-pressed', String(view.mode === m));
  modeBtn.sectors.disabled = !sectors;
  modeNote.hidden = sectors !== null;
  modeNote.textContent = sectors ? '' : `Sectors are unavailable: ${names.a}’s lap has no complete sector times.`;
  sortBtn.hidden = view.mode === 'sectors';

  map?.setSections(sections);
  list.set(sections, { names, lengthM: d.length_m, finalDelta: d.final_delta_s });
  mapRoot.setAttribute(
    'aria-label',
    `Track map of ${d.a.session.circuit} in ${sections.length} ${view.mode === 'sectors' ? 'sectors' : 'sections'}, each coloured by the faster driver. The list next to it has the same information.`,
  );
  applySelection();
}

function setMode(mode: Mode): void {
  if (!current || view.mode === mode) return;
  view = { mode, section: null };
  applySections(current);
}
modeBtn.corners.onclick = () => setMode('corners');
modeBtn.sectors.onclick = () => setMode('sectors');
sortBtn.onclick = () => {
  const on = sortBtn.getAttribute('aria-pressed') !== 'true';
  sortBtn.setAttribute('aria-pressed', String(on));
  list.setSort(on);
  list.setSelected(view.section);
};

const FORMAT: Record<Channel, { label: string; fmt: (v: number) => string }> = {
  speed: { label: 'Speed', fmt: (v) => v.toFixed(0) },
  throttle: { label: 'Throttle', fmt: (v) => `${v.toFixed(0)}%` },
  brake: { label: 'Brake', fmt: (v) => (v >= 0.5 ? 'on' : 'off') },
  gear: { label: 'Gear', fmt: (v) => (v < 0.5 ? 'N' : v.toFixed(0)) },
  rpm: { label: 'RPM', fmt: (v) => v.toFixed(0) },
  drs: { label: 'DRS', fmt: (v) => (v >= 0.5 ? 'open' : 'shut') },
};

/** Builds the readout once per comparison and returns a cheap per-hover updater. */
function renderReadout(d: Compare, names: Names): (i: number) => void {
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
  root.style.setProperty('--fill-b', colours.fillB);
  root.classList.toggle('dash-b', colours.dashB);

  names = labels(d);
  failed = false;
  renderSummary(d);
  syncPicker();
  renderLegend(colours.dashB);
  compare.hidden = false;
  compare.classList.remove('stale');

  showValues = renderReadout(d, names);
  const cursor = (i: number): void => {
    hoverIdx = i;
    showValues(i);
    map?.setIndex(i);
  };
  map?.destroy();
  map = renderTrackMap(mapRoot, d.channels.x.a, d.channels.y.a, {
    onSelect: (n) => select(n),
    onHover: (n) => hoverSection(n),
    onScrub: (i) => {
      charts?.setCursor(i);
      if (i !== null) cursor(i); // also when the point is outside the zoomed charts
    },
  });
  charts?.destroy();
  charts = renderCharts(chartsRoot, d, colours, names, cursor);
  applySections(d);
  if (hoverIdx !== null && hoverIdx < d.distance.length) cursor(hoverIdx);
  setStatus('', 'none');
}

async function load(sel: Selection | null): Promise<void> {
  request?.abort();
  if (!firstLoad) view = { mode: view.mode, section: null };
  firstLoad = false;
  if (!sel) {
    showFailure('That driver has no laps with telemetry in this session. Pick another driver.');
    return;
  }
  writeUrl(sel, view);
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
    if (ctl.signal.aborted) return; // superseded by a newer selection; its result decides what is shown
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
