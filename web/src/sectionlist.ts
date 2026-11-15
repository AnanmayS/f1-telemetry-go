// The section list: one option per section with a diverging bar for the time
// gained or lost there, plus a "Whole lap" entry. A listbox: arrow keys move
// the focus, Enter or Space selects.
import { winner, type Section } from './sections';
import { el, fmtDelta, fmtM } from './state';

export interface SectionList {
  set(sections: Section[], info: ListInfo): void;
  setSort(byDifference: boolean): void;
  setSelected(n: number | null): void;
  setHover(n: number | null): void;
}

export interface ListInfo {
  names: { a: string; b: string };
  lengthM: number;
  finalDelta: number | null;
}

export interface ListHandlers {
  onSelect(n: number | null): void;
  onHover(n: number | null): void;
}

const kmh = (v: number | null): string => (v == null ? '–' : String(Math.round(v)));

export function createSectionList(root: HTMLElement, handlers: ListHandlers): SectionList {
  let sections: Section[] = [];
  let info: ListInfo = { names: { a: 'A', b: 'B' }, lengthM: 0, finalDelta: null };
  let byDifference = false;
  let selected: number | null = null;
  let hover: number | null = null;
  let rows: HTMLElement[] = [];

  const key = (row: HTMLElement): number | null => {
    const n = Number(row.dataset.section);
    return n === 0 ? null : n;
  };
  const mark = (): void => {
    for (const r of rows) {
      const n = key(r);
      r.setAttribute('aria-selected', String(n === selected));
      r.classList.toggle('hover', n !== null && n === hover);
    }
    // Roving tabindex: Tab lands on the selected option.
    const home = rows.find((r) => key(r) === selected) ?? rows[0];
    for (const r of rows) r.tabIndex = r === home ? 0 : -1;
  };

  /** Keeps the selected chip in view on the phone layout without scrolling the page. */
  const reveal = (): void => {
    const row = rows.find((r) => key(r) === selected);
    if (!row || root.scrollWidth <= root.clientWidth) return;
    const left = row.offsetLeft - root.offsetLeft;
    if (left < root.scrollLeft || left + row.offsetWidth > root.scrollLeft + root.clientWidth) {
      root.scrollTo({ left: left - (root.clientWidth - row.offsetWidth) / 2 });
    }
  };

  const row = (s: Section, strongest: number): HTMLElement => {
    const w = winner(s);
    const { a, b } = info.names;
    const who = w === 'even' ? 'even' : `${w === 'a' ? a : b} faster`;
    const speeds = s.kind === 'straight' ? s.top : s.min;
    const bar = el('i');
    bar.style.width = `${w === 'even' ? 0 : (Math.abs(s.gain ?? 0) / strongest) * 50}%`;
    const node = el(
      'div',
      { class: `row w-${w}`, role: 'option', id: `sec-${s.n}` },
      el('span', { class: 'num' }, String(s.n)),
      el(
        'span',
        { class: 'what' },
        el('b', {}, s.kind === 'sector' ? `${s.name} · ${s.label}` : s.label),
        el('small', {}, `${s.kind === 'straight' ? 'top' : 'min'} ${a} ${kmh(speeds.a)} · ${b} ${kmh(speeds.b)}`),
      ),
      el('span', { class: 'bar' }, bar),
      el('span', { class: 'val' }, el('b', {}, `${fmtDelta(s.gain)} s`), el('small', {}, who)),
    );
    node.dataset.section = String(s.n);
    node.setAttribute(
      'aria-label',
      `${s.name}, ${s.label}. ${w === 'even' ? 'Even' : `${w === 'a' ? a : b} faster by ${Math.abs(s.gain ?? 0).toFixed(3)} seconds`}.`,
    );
    return node;
  };

  const render = (): void => {
    const hadFocus = root.contains(document.activeElement);
    const strongest = Math.max(1e-9, ...sections.map((s) => Math.abs(s.gain ?? 0)));
    const ordered = byDifference ? [...sections].sort((x, y) => Math.abs(y.gain ?? 0) - Math.abs(x.gain ?? 0)) : sections;
    const whole = el(
      'div',
      { class: 'row whole', role: 'option', id: 'sec-0' },
      el('span', { class: 'num' }, '↺'),
      el('span', { class: 'what' }, el('b', {}, 'Whole lap'), el('small', {}, `${fmtM(info.lengthM)} m`)),
      el('span', { class: 'bar' }),
      el('span', { class: 'val' }, el('b', {}, `${fmtDelta(info.finalDelta)} s`), el('small', {}, 'at the line')),
    );
    whole.dataset.section = '0';
    whole.setAttribute('aria-label', 'Whole lap');
    rows = [whole, ...ordered.map((s) => row(s, strongest))];
    root.replaceChildren(...rows);
    mark();
    if (hadFocus) (rows.find((r) => key(r) === selected) ?? rows[0])?.focus();
  };

  root.addEventListener('click', (e) => {
    const r = (e.target as Element).closest<HTMLElement>('.row');
    if (r) handlers.onSelect(key(r));
  });
  root.addEventListener('pointerover', (e) => {
    if (e.pointerType === 'touch') return;
    const r = (e.target as Element).closest<HTMLElement>('.row');
    handlers.onHover(r ? key(r) : null);
  });
  root.addEventListener('pointerleave', () => handlers.onHover(null));
  root.addEventListener('focusin', (e) => {
    const r = (e.target as Element).closest<HTMLElement>('.row');
    if (r) handlers.onHover(key(r));
  });
  root.addEventListener('focusout', () => handlers.onHover(null));
  root.addEventListener('keydown', (e) => {
    const at = rows.indexOf(document.activeElement as HTMLElement);
    if (at < 0) return;
    const move: Record<string, number> = { ArrowDown: at + 1, ArrowRight: at + 1, ArrowUp: at - 1, ArrowLeft: at - 1, Home: 0, End: rows.length - 1 };
    if (e.key in move) {
      e.preventDefault();
      const next = rows[Math.min(rows.length - 1, Math.max(0, move[e.key]!))]!;
      for (const r of rows) r.tabIndex = r === next ? 0 : -1;
      next.focus();
    } else if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      handlers.onSelect(key(rows[at]!));
    }
  });

  return {
    set(next, nextInfo) {
      sections = next;
      info = nextInfo;
      selected = hover = null;
      render();
    },
    setSort(v) {
      byDifference = v;
      render();
    },
    setSelected(n) {
      selected = n;
      mark();
      reveal();
    },
    setHover(n) {
      hover = n;
      mark();
    },
  };
}
