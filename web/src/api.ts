// Typed client for the ghostlap JSON API (docs/API.md).

export interface Session {
  session_key: number;
  meeting_key: number;
  year: number;
  meeting_name: string;
  session_name: string;
  session_type: string;
  circuit: string;
  country: string;
  date_start: string;
  date_end: string;
}

export interface Driver {
  number: number;
  acronym: string;
  name: string;
  team: string;
  colour: string;
  laps?: number;
  fastest_lap?: number | null;
  fastest_ms?: number | null;
}

export interface Lap {
  lap: number;
  duration_ms: number | null;
  sectors_ms: (number | null)[];
  pit_out: boolean;
  pit_in: boolean;
  valid_time: boolean;
  has_telemetry: boolean;
  is_fastest: boolean;
}

export type Series = (number | null)[];
export const CHANNELS = ['speed', 'throttle', 'brake', 'gear', 'rpm', 'drs'] as const;
export type Channel = (typeof CHANNELS)[number];

export interface CompareSide {
  session_key: number;
  session: Session;
  driver: Driver;
  lap: Lap;
}

export interface Compare {
  a: CompareSide;
  b: CompareSide;
  step_m: number;
  length_m: number;
  same_circuit: boolean;
  aligned: boolean;
  official_delta_s: number | null;
  final_delta_s: number | null;
  distance: number[];
  delta: Series;
  time: { a: Series; b: Series };
  channels: Record<Channel | 'x' | 'y', { a: Series; b: Series }>;
}

/** One side of a comparison, as it appears in the URL: session:driver:lap. */
export interface LapRef {
  session: number;
  driver: number;
  lap: number | 'fastest';
}

export class ApiError extends Error {
  constructor(
    message: string,
    readonly code: string,
    readonly status: number,
  ) {
    super(message);
  }
}

async function get<T>(path: string, signal?: AbortSignal): Promise<T> {
  let res: Response;
  try {
    res = await fetch(path, { signal, headers: { Accept: 'application/json' } });
  } catch (e) {
    if (e instanceof DOMException && e.name === 'AbortError') throw e;
    throw new ApiError('Could not reach the server. Check your connection and try again.', 'network', 0);
  }
  let body: unknown = null;
  try {
    body = await res.json();
  } catch (e) {
    // An abort while the body is still streaming is a cancellation, not a bad response.
    if (signal?.aborted || (e instanceof DOMException && e.name === 'AbortError')) {
      throw new DOMException('Aborted', 'AbortError');
    }
    // otherwise fall through: handled below
  }
  if (!res.ok) {
    const err = (body ?? {}) as { error?: string; code?: string };
    throw new ApiError(err.error ?? `Request failed (${res.status})`, err.code ?? 'http_error', res.status);
  }
  if (body === null) throw new ApiError('The server sent an unreadable response.', 'bad_response', res.status);
  return body as T;
}

const cache = new Map<string, Promise<unknown>>();
/** Listings are small and immutable enough to keep for the page's lifetime. */
function cached<T>(path: string): Promise<T> {
  let p = cache.get(path) as Promise<T> | undefined;
  if (!p) {
    p = get<T>(path);
    cache.set(path, p);
    p.catch(() => cache.delete(path));
  }
  return p;
}

export const refString = (r: LapRef): string => `${r.session}:${r.driver}:${r.lap}`;

export const api = {
  seasons: () => cached<{ seasons: number[] }>('/api/seasons').then((r) => r.seasons ?? []),
  sessions: () => cached<{ sessions: Session[] }>('/api/sessions').then((r) => r.sessions ?? []),
  drivers: (session: number) =>
    cached<{ drivers: Driver[] }>(`/api/sessions/${session}/drivers`).then((r) => r.drivers ?? []),
  laps: (session: number, driver: number) =>
    cached<{ laps: Lap[] }>(`/api/sessions/${session}/laps?driver=${driver}`).then((r) => r.laps ?? []),
  compare: (a: LapRef, b: LapRef, signal?: AbortSignal) =>
    get<Compare>(
      `/api/compare?a=${refString(a)}&b=${refString(b)}&channels=${CHANNELS.join(',')},x,y`,
      signal,
    ),
};
