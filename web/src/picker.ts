// Lap picker: two independent sides (A and B), each season -> event -> session -> driver -> lap.
import { api, type Driver, type Lap, type LapRef, type Session } from './api';
import { el, fmtLap, type Selection } from './state';

interface Want {
  session: number;
  driver?: number;
  lap?: number | 'fastest';
}
type DriverDefault = (sorted: Driver[], session: number) => Driver | undefined;

const byPace = (x: Driver, y: Driver): number => (x.fastest_ms ?? Infinity) - (y.fastest_ms ?? Infinity);

function fill(select: HTMLSelectElement, options: HTMLOptionElement[], value: string): void {
  select.replaceChildren(...options);
  select.value = value;
  select.disabled = options.length === 0;
}

class Side {
  private seq = 0;
  private session: Session | null = null;
  private driver: Driver | null = null;
  private lap: number | 'fastest' | null = null;

  readonly root: HTMLFieldSetElement;
  private readonly year = el('select');
  private readonly meeting = el('select');
  private readonly sess = el('select');
  private readonly drv = el('select');
  private readonly lapSel = el('select');

  constructor(
    readonly id: 'a' | 'b',
    private readonly sessions: Session[],
    private readonly defaultDriver: DriverDefault,
    /** Called after a user edit has been applied; `prev` is the session before the edit. */
    private readonly onEdit: (prev: number | null) => void,
    private readonly onError: (e: unknown) => void,
  ) {
    const field = (label: string, select: HTMLSelectElement, cls: string): HTMLLabelElement => {
      select.id = `${id}-${cls}`;
      return el('label', { class: `field ${cls}`, htmlFor: select.id }, el('span', {}, label), select);
    };
    this.root = el(
      'fieldset',
      { class: `pick pick-${id}` },
      el('legend', {}, el('i', { class: `swatch swatch-${id}` }), `Lap ${id.toUpperCase()}`),
      field('Season', this.year, 'year'),
      field('Event', this.meeting, 'meeting'),
      field('Session', this.sess, 'session'),
      field('Driver', this.drv, 'driver'),
      field('Lap', this.lapSel, 'lap'),
    );

    const edit = (want: () => Want | undefined) => () => {
      const prev = this.sessionKey;
      const w = want();
      if (!w) return;
      const p = this.load(w);
      const mine = this.seq;
      // An error from a load that a newer edit has replaced must not cover the newer result.
      p.then((ok) => ok && this.onEdit(prev), (e) => mine === this.seq && this.onError(e));
    };
    this.year.onchange = edit(() => this.pickSession((s) => s.year === Number(this.year.value)));
    this.meeting.onchange = edit(() => this.pickSession((s) => s.meeting_key === Number(this.meeting.value)));
    this.sess.onchange = edit(() => ({ session: Number(this.sess.value), driver: this.driver?.number }));
    this.drv.onchange = edit(() => this.session ? { session: this.session.session_key, driver: Number(this.drv.value) } : undefined);
    this.lapSel.onchange = () => {
      const v = this.lapSel.value;
      this.lap = v === 'fastest' ? 'fastest' : Number(v);
      this.onEdit(this.sessionKey);
    };
  }

  get sessionKey(): number | null {
    return this.session?.session_key ?? null;
  }
  get driverNumber(): number | null {
    return this.driver?.number ?? null;
  }
  ref(): LapRef | null {
    if (!this.session || !this.driver || this.lap === null) return null;
    return { session: this.session.session_key, driver: this.driver.number, lap: this.lap };
  }

  /** Moving season or event keeps the same circuit and session type where possible (e.g. 2024 -> 2023 Suzuka Q). */
  private pickSession(match: (s: Session) => boolean): Want | undefined {
    const pool = this.sessions.filter(match);
    const cur = this.session;
    const next =
      pool.find((s) => s.circuit === cur?.circuit && s.session_name === cur.session_name) ??
      pool.find((s) => s.session_name === cur?.session_name) ??
      pool.find((s) => s.circuit === cur?.circuit) ??
      pool.find((s) => s.session_type === 'Qualifying') ??
      pool[0];
    return next && { session: next.session_key, driver: this.driver?.number };
  }

  /** Resolves a wanted selection against the API and redraws the selects. False if superseded. */
  async load(want: Want): Promise<boolean> {
    const seq = ++this.seq;
    const session = this.sessions.find((s) => s.session_key === want.session) ?? this.sessions[0];
    if (!session) return false;
    this.root.setAttribute('aria-busy', 'true');
    try {
      const drivers = [...(await api.drivers(session.session_key))].sort(byPace);
      const driver = drivers.find((d) => d.number === want.driver) ?? this.defaultDriver(drivers, session.session_key) ?? drivers[0] ?? null;
      const laps = driver ? await api.laps(session.session_key, driver.number) : [];
      if (seq !== this.seq) return false;

      const wanted = laps.find((l) => l.lap === want.lap && l.has_telemetry);
      const fastest = laps.find((l) => l.is_fastest && l.has_telemetry);
      const chosen = wanted ?? fastest ?? laps.find((l) => l.has_telemetry);
      this.session = session;
      this.driver = driver;
      this.lap = chosen ? (chosen.is_fastest ? 'fastest' : chosen.lap) : null;
      this.render(drivers, laps);
      return true;
    } finally {
      if (seq === this.seq) this.root.removeAttribute('aria-busy');
    }
  }

  private render(drivers: Driver[], laps: Lap[]): void {
    const cur = this.session!;
    const opt = (value: string | number, text: string, disabled = false): HTMLOptionElement =>
      el('option', { value: String(value), textContent: text, disabled });

    const years = [...new Set(this.sessions.map((s) => s.year))].sort((x, y) => y - x);
    fill(this.year, years.map((y) => opt(y, String(y))), String(cur.year));

    const inYear = this.sessions
      .filter((s) => s.year === cur.year)
      .sort((x, y) => x.date_start.localeCompare(y.date_start));
    const meetings = new Map(inYear.map((s) => [s.meeting_key, s.meeting_name]));
    fill(this.meeting, [...meetings].map(([k, name]) => opt(k, name)), String(cur.meeting_key));

    fill(
      this.sess,
      inYear.filter((s) => s.meeting_key === cur.meeting_key).map((s) => opt(s.session_key, s.session_name)),
      String(cur.session_key),
    );

    fill(
      this.drv,
      drivers.map((d) => opt(d.number, `${d.acronym} · ${d.fastest_ms == null ? 'no time' : fmtLap(d.fastest_ms)} · ${d.team}`)),
      String(this.driver?.number ?? ''),
    );

    fill(
      this.lapSel,
      laps.map((l) => {
        const notes = [
          fmtLap(l.duration_ms),
          l.is_fastest && 'fastest',
          l.pit_out && 'out lap',
          l.pit_in && 'in lap',
          !l.has_telemetry && 'no telemetry',
        ].filter(Boolean);
        return opt(l.is_fastest && l.has_telemetry ? 'fastest' : l.lap, `Lap ${l.lap} · ${notes.join(' · ')}`, !l.has_telemetry);
      }),
      String(this.lap ?? ''),
    );
  }
}

export interface Picker {
  /** Restores a selection (from the URL) or picks defaults: the two fastest drivers of the newest qualifying. */
  init(a: LapRef | null, b: LapRef | null): Promise<void>;
  selection(): Selection | null;
  /** Moves the keyboard focus to the first field. */
  focus(): void;
}

export function createPicker(
  form: HTMLFormElement,
  sessions: Session[],
  onChange: (sel: Selection | null) => void,
  onError: (e: unknown) => void,
): Picker {
  const selection = (): Selection | null => {
    const [ra, rb] = [a.ref(), b.ref()];
    return ra && rb ? { a: ra, b: rb } : null;
  };

  const a: Side = new Side('a', sessions, (d) => d[0], onEditA, onError);
  // B defaults to the quickest driver who is not A, or to A's own driver when B is in another session.
  const b: Side = new Side(
    'b',
    sessions,
    (d, session) =>
      session === a.sessionKey
        ? d.find((x) => x.number !== a.driverNumber)
        : d.find((x) => x.number === a.driverNumber),
    () => onChange(selection()),
    onError,
  );

  function onEditA(prev: number | null): void {
    // B follows A's session for as long as the two were on the same one.
    const follow = a.sessionKey !== null && a.sessionKey !== prev && b.sessionKey === prev;
    if (!follow) return onChange(selection());
    b.load({ session: a.sessionKey!, driver: b.driverNumber ?? undefined }).then(
      (ok) => ok && onChange(selection()),
      onError,
    );
  }

  form.replaceChildren(a.root, b.root);
  form.onsubmit = (e) => e.preventDefault();

  return {
    selection,
    focus: () => form.querySelector('select')?.focus(),
    async init(ra, rb) {
      // A link to a session we do not have falls back to the defaults as a
      // whole: keeping its driver and lap would show some other comparison.
      const known = (r: typeof ra) => (r && sessions.some((s) => s.session_key === r.session) ? r : undefined);
      const wa = known(ra);
      const start = sessions.find((s) => s.session_type === 'Qualifying') ?? sessions[0]!;
      await a.load(wa ?? { session: start.session_key });
      await b.load(known(rb) ?? { session: a.sessionKey! });
      onChange(selection());
    },
  };
}
