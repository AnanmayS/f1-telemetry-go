// Race predictions page: pick a Grand Prix, see the simulated outcome and,
// once the race has been run, how it compared with what happened.
import './style.css';
import { ApiError, predictions, type PredDriver, type PredList, type Prediction } from './api';
import { el } from './state';

const $ = <T extends Element = HTMLElement>(id: string): T => document.getElementById(id) as unknown as T;
const select = $<HTMLSelectElement>('race');
const status = $('status');
const result = $('result');
const lede = $('lede');
const about = $('about');

let request: AbortController | null = null;
let list: PredList | null = null;

function setStatus(text: string, kind: 'loading' | 'error' | 'none'): void {
  status.hidden = kind === 'none';
  status.textContent = text;
  status.className = `status ${kind}`;
  status.setAttribute('role', kind === 'error' ? 'alert' : 'status');
}

/** 0.004 -> "<1%", 0.337 -> "34%", 0 -> "–" */
function pct(p: number): string {
  if (p <= 0) return '–';
  if (p < 0.01) return '<1%';
  if (p > 0.99 && p < 1) return '>99%';
  return `${Math.round(p * 100)}%`;
}

const ord = (n: number): string => `P${n}`;

function probCell(p: number): HTMLTableCellElement {
  const td = el('td', { class: 'prob' }, el('span', { class: 'val' }, pct(p)));
  td.style.setProperty('--p', String(Math.min(1, Math.max(0, p))));
  return td;
}

/** A bar from the 10th to the 90th percentile finishing position, with the median and the real result marked. */
function rangeCell(d: PredDriver, n: number): HTMLTableCellElement {
  const at = (pos: number): string => `${((pos - 0.5) / n) * 100}%`;
  const bar = el('span', { class: 'range' });
  const band = el('span', { class: 'band' });
  band.style.left = `${((d.p10 - 1) / n) * 100}%`;
  band.style.width = `${((d.p90 - d.p10 + 1) / n) * 100}%`;
  const mid = el('span', { class: 'mid' });
  mid.style.left = at(d.p50);
  bar.append(band, mid);
  if (d.actual) {
    const real = el('span', { class: 'real' });
    real.style.left = at(d.actual);
    bar.append(real);
  }
  const text = d.p10 === d.p90 ? ord(d.p50) : `${ord(d.p10)} to ${ord(d.p90)}`;
  return el('td', { class: 'rangecell' }, bar, el('small', {}, text));
}

function render(p: Prediction): void {
  const n = p.drivers.length;
  const run = p.race.run;
  const head = el('tr', {});
  const columns: [string, string][] = [
    ['Driver', ''],
    ['Grid', 'Starting position'],
    ['Win', 'Chance of winning'],
    ['Podium', 'Chance of finishing in the top three'],
    ['Points', 'Chance of finishing in the top ten'],
    ['Retires', 'Chance of not finishing'],
    ['Likely finish', 'Middle 80% of simulated finishing positions when the car finishes; the tick is the median'],
  ];
  if (run) columns.push(['Result', 'Where the driver actually finished']);
  for (const [label, title] of columns) {
    head.append(el('th', { scope: 'col', title }, label));
  }
  const body = el('tbody', {});
  for (const d of p.drivers) {
    const chip = el('span', { class: 'team' });
    if (d.colour && /^[0-9a-f]{6}$/i.test(d.colour)) chip.style.background = `#${d.colour}`;
    const row = el(
      'tr',
      {},
      el('th', { scope: 'row', class: 'drv' }, chip, el('span', {}, d.acronym), el('small', {}, d.team)),
      el('td', { class: 'val' }, ord(d.grid)),
      probCell(d.win),
      probCell(d.podium),
      probCell(d.points),
      el('td', { class: 'val' }, pct(d.dnf)),
      rangeCell(d, n),
    );
    if (run) {
      const inside = d.actual != null && d.actual >= d.p10 && d.actual <= d.p90;
      const text = d.actual != null ? ord(d.actual) : d.retired ? 'Retired' : 'Not classified';
      row.append(el('td', { class: `val res ${d.actual == null ? 'out' : inside ? 'in' : 'miss'}` }, text));
      if (d.actual === 1) row.classList.add('won');
    }
    body.append(row);
  }
  const table = el('table', { class: 'pred' }, el('thead', {}, head), body);

  const top = p.drivers[0];
  if (!top) return;
  const parts: string[] = [];
  if (run) {
    const winner = p.drivers.find((d) => d.actual === 1);
    if (winner && winner.number === top.number) {
      parts.push(`The favourite, ${top.name}, won. The simulation gave that a ${pct(top.win)} chance.`);
    } else if (winner) {
      const rank = p.drivers.indexOf(winner) + 1;
      parts.push(
        `${winner.name} won; the simulation gave that a ${pct(winner.win)} chance (ranked ${rank} of ${n}). The favourite was ${top.name} at ${pct(top.win)}, who ${top.actual ? `finished ${ord(top.actual)}` : 'did not finish'}.`,
      );
    }
    const inside = p.drivers.filter((d) => d.actual != null && d.actual >= d.p10 && d.actual <= d.p90).length;
    const classified = p.drivers.filter((d) => d.actual != null).length;
    parts.push(`${inside} of ${classified} classified drivers finished inside their likely range.`);
    if (p.race.wet) parts.push('This race was wet. The model does not know the weather, so expect it to do worse here.');
  } else {
    parts.push(`Not run yet. ${top.name} is the favourite at ${pct(top.win)}. The grid is the provisional qualifying order, before any penalties.`);
  }
  lede.textContent = parts.join(' ');

  result.replaceChildren(
    el('div', { class: 'scroll' }, table),
    el(
      'p',
      { class: 'hint' },
      `${p.runs.toLocaleString('en')} simulated races of ${p.inputs.laps} laps, using only qualifying, the grid and the ${p.inputs.trained_on} races before this one. ` +
        `Assumes a ${p.inputs.pit_loss_s.toFixed(1)} s pit stop loss. The likely finish is the middle 80% of results in the runs where the car reached the end.` +
        (run ? ' In the result column, green means inside the likely range and amber means outside it.' : ''),
    ),
  );
  result.hidden = false;
}

function renderAbout(): void {
  const b = list?.backtest;
  const items: (Node | string)[] = [
    el('h2', {}, 'How far to trust this'),
    el(
      'p',
      {},
      'Every race is predicted using only information from before it started, then scored against the real result. ',
      'The settings were tuned on 2023 and 2024; the figures below are for later races. ',
      'For picking the winner alone, the starting grid has been the better guide; the simulation does better on the rest of the order.',
    ),
  ];
  if (b && b.model.races > 0) {
    const row = (name: string, m: typeof b.model) =>
      el(
        'tr',
        {},
        el('th', { scope: 'row' }, name),
        el('td', { class: 'val' }, pct(m.winner_hit)),
        el('td', { class: 'val' }, `${m.podium_overlap.toFixed(2)} of 3`),
        el('td', { class: 'val' }, `${m.mae.toFixed(2)} places`),
      );
    items.push(
      el(
        'div',
        { class: 'scroll' },
        el(
          'table',
          { class: 'pred small' },
          el(
            'thead',
            {},
            el(
              'tr',
              {},
              el('th', { scope: 'col' }, `${b.model.races} races, ${b.from_year} onwards`),
              el('th', { scope: 'col' }, 'Top pick won'),
              el('th', { scope: 'col' }, 'Podium finishers picked'),
              el('th', { scope: 'col' }, 'Average miss'),
            ),
          ),
          el('tbody', {}, row('This model', b.model), row('Grid order alone', b.grid_only)),
        ),
      ),
    );
  }
  items.push(
    el(
      'p',
      { class: 'hint' },
      b && b.model.coverage > 0 ? `${Math.round(b.model.coverage * 100)}% of finishers ended inside their likely range (80% would be ideal). ` : '',
      'It does not know about weather, grid penalties announced after qualifying, car upgrades, team orders or damage. ',
      'It is an estimate from past data for interest only, not betting odds or advice. ',
      el('a', { href: 'https://github.com/AnanmayS/f1-telemetry-go/blob/main/docs/PREDICTIONS.md', rel: 'noopener' }, 'Model and full backtest'),
      '.',
    ),
  );
  about.replaceChildren(...items);
  about.hidden = false;
}

async function show(key: number): Promise<void> {
  request?.abort();
  const mine = (request = new AbortController());
  setStatus('Simulating…', 'loading');
  result.classList.add('stale');
  try {
    const p = await predictions.race(key, mine.signal);
    if (mine.signal.aborted) return;
    render(p);
    setStatus('', 'none');
    history.replaceState(null, '', `${location.pathname}?race=${key}`);
    document.title = `${p.race.year} ${p.race.name} prediction · ghostlap`;
  } catch (e) {
    if (mine.signal.aborted || (e instanceof DOMException && e.name === 'AbortError')) return;
    result.hidden = true;
    lede.textContent = '';
    setStatus(e instanceof ApiError ? e.message.charAt(0).toUpperCase() + e.message.slice(1) : 'Something went wrong. Reload the page to try again.', 'error');
  } finally {
    result.classList.remove('stale');
  }
}

async function start(): Promise<void> {
  try {
    list = await predictions.list();
  } catch (e) {
    setStatus(e instanceof ApiError ? e.message : 'Could not load the races. Reload the page to try again.', 'error');
    return;
  }
  if (list.races.length === 0) {
    setStatus('No race predictions are available on this server yet.', 'error');
    return;
  }
  const asked = Number(new URLSearchParams(location.search).get('race'));
  const first = list.races.find((r) => r.session_key === asked) ?? list.races[0];
  if (!first) return;
  let group: HTMLOptGroupElement | null = null;
  for (const r of list.races) {
    if (!group || group.label !== String(r.year)) {
      group = el('optgroup', { label: String(r.year) });
      select.append(group);
    }
    group.append(el('option', { value: String(r.session_key) }, `${r.name}${r.run ? '' : ' (not run yet)'}`));
  }
  select.disabled = false;
  select.value = String(first.session_key);
  select.addEventListener('change', () => void show(Number(select.value)));
  renderAbout();
  await show(first.session_key);
}

void start();
