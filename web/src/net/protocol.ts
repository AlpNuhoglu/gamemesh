// The wire contract of Ghost Tag (design document, section 6; the Go side is
// internal/ghosttag/messages.go and internal/wsgateway).
//
// Everything the server sends arrives in the GameMesh envelope
// {"type":..., "room":..., "data":...}. A validated room's own messages have
// type "Game" and carry one of the payloads below in data; the hub's own
// messages (MatchFound, JoinRejected, ...) have their own types. What the
// client sends is {"action":"join"|"leave"|"relay", "room":..., "data":...}.
//
// Parsing is strict about the fields the client uses and ignores the rest, so
// a server that adds a field does not break the client and a malformed frame
// is dropped rather than half-applied.

/** snap.cfg: every game constant the client uses (section 6). */
export interface Cfg {
  v_runner: number;
  v_hunter: number;
  omega_max: number;
  alpha: number;
  r0: number;
  r_min: number;
  d_capture: number;
  slack_max: number;
  move_eps: number;
  dt_max_ms: number;
  t_half_ms: number;
  t_shrink_ms: number;
  send_ms: number;
  trail_ms: number;
  interp_ms: number;
  dr_ms: number;
}

const cfgKeys: readonly (keyof Cfg)[] = [
  "v_runner", "v_hunter", "omega_max", "alpha", "r0", "r_min", "d_capture", "slack_max",
  "move_eps", "dt_max_ms", "t_half_ms", "t_shrink_ms", "send_ms", "trail_ms", "interp_ms", "dr_ms",
];

export type Role = "HUNTER" | "RUNNER";

export type RoomState =
  | "WAITING"
  | "COUNTDOWN_1"
  | "HALF_1_ACTIVE"
  | "ROLE_SWAP"
  | "HALF_2_ACTIVE"
  | "DISCONNECT_GRACE"
  | "FINISHED";

const roomStates: readonly RoomState[] = [
  "WAITING", "COUNTDOWN_1", "HALF_1_ACTIVE", "ROLE_SWAP", "HALF_2_ACTIVE", "DISCONNECT_GRACE", "FINISHED",
];

export function isActive(s: RoomState | null): boolean {
  return s === "HALF_1_ACTIVE" || s === "HALF_2_ACTIVE";
}

export interface SnapPlayer {
  id: string;
  name: string;
  role: Role | "";
  x: number;
  y: number;
  theta: number;
  connected: boolean;
}

export interface Snap {
  t: "snap";
  state: RoomState;
  sts: number;
  rem_ms: number;
  r: number;
  players: SnapPlayer[];
  cfg: Cfg;
}

/** The room's broadcast of another player's validated position. */
export interface PosIn {
  t: "pos";
  from: string;
  sts: number;
  x: number;
  y: number;
  vx: number;
  vy: number;
}

export interface Pong {
  t: "pong";
  cts: number;
  sts: number;
}

export interface Clamp {
  t: "clamp";
  x: number;
  y: number;
  theta: number;
}

export interface HalfEnd {
  t: "half_end";
  half: number;
  reason: "CAPTURE" | "TIMEOUT";
  dur_ms: number;
  hunter: string;
  runner: string;
}

export interface MatchEnd {
  t: "match_end";
  winner: string;
  is_draw: boolean;
  ranked: boolean;
  deltas: Record<string, number>;
}

export type GameMsg = Snap | PosIn | Pong | Clamp | HalfEnd | MatchEnd;

/** What the hub sends, as far as the client reads it. */
export type ServerMsg =
  | { type: "Game"; room: string; data: GameMsg }
  | { type: "MatchFound"; room: string; players: string[] }
  | { type: "JoinRejected"; room: string }
  | { type: "LeaderboardUpdated"; board: string }
  | { type: "Other"; name: string };

/** The client's position report (section 6, pos inbound). */
export interface PosOut {
  t: "pos";
  seq: number;
  cts: number;
  x: number;
  y: number;
  vx: number;
  vy: number;
}

export interface PingOut {
  t: "ping";
  cts: number;
}

export type ClientMsg =
  | { action: "join"; room: string }
  | { action: "leave"; room: string }
  | { action: "relay"; room: string; data: PosOut | PingOut };

type Obj = Record<string, unknown>;

function isObj(v: unknown): v is Obj {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function num(o: Obj, k: string): number | undefined {
  const v = o[k];
  return typeof v === "number" && Number.isFinite(v) ? v : undefined;
}

function str(o: Obj, k: string): string | undefined {
  const v = o[k];
  return typeof v === "string" ? v : undefined;
}

export function parseCfg(v: unknown): Cfg | null {
  if (!isObj(v)) return null;
  const out: Partial<Cfg> = {};
  for (const k of cfgKeys) {
    const n = num(v, k);
    if (n === undefined) return null;
    out[k] = n;
  }
  return out as Cfg;
}

function parsePlayer(v: unknown): SnapPlayer | null {
  if (!isObj(v)) return null;
  const id = str(v, "id");
  const role = str(v, "role");
  const x = num(v, "x");
  const y = num(v, "y");
  const theta = num(v, "theta");
  if (id === undefined || x === undefined || y === undefined || theta === undefined) return null;
  if (role !== "HUNTER" && role !== "RUNNER" && role !== "") return null;
  return { id, name: str(v, "name") ?? "", role, x, y, theta, connected: v["connected"] === true };
}

/** parseGame reads a Game frame's data; null for anything it does not know. */
export function parseGame(d: unknown): GameMsg | null {
  if (!isObj(d)) return null;
  switch (d["t"]) {
    case "pos": {
      const from = str(d, "from");
      const sts = num(d, "sts");
      const x = num(d, "x");
      const y = num(d, "y");
      if (from === undefined || sts === undefined || x === undefined || y === undefined) return null;
      return { t: "pos", from, sts, x, y, vx: num(d, "vx") ?? 0, vy: num(d, "vy") ?? 0 };
    }
    case "pong": {
      const cts = num(d, "cts");
      const sts = num(d, "sts");
      if (cts === undefined || sts === undefined) return null;
      return { t: "pong", cts, sts };
    }
    case "clamp": {
      const x = num(d, "x");
      const y = num(d, "y");
      const theta = num(d, "theta");
      if (x === undefined || y === undefined || theta === undefined) return null;
      return { t: "clamp", x, y, theta };
    }
    case "snap": {
      const state = str(d, "state") as RoomState | undefined;
      const sts = num(d, "sts");
      const rem = num(d, "rem_ms");
      const r = num(d, "r");
      const cfg = parseCfg(d["cfg"]);
      const ps = d["players"];
      if (state === undefined || !roomStates.includes(state) || sts === undefined || rem === undefined ||
        r === undefined || cfg === null || !Array.isArray(ps)) return null;
      const players: SnapPlayer[] = [];
      for (const p of ps) {
        const pp = parsePlayer(p);
        if (pp === null) return null;
        players.push(pp);
      }
      return { t: "snap", state, sts, rem_ms: rem, r, players, cfg };
    }
    case "half_end": {
      const half = num(d, "half");
      const reason = str(d, "reason");
      if (half === undefined || (reason !== "CAPTURE" && reason !== "TIMEOUT")) return null;
      return {
        t: "half_end", half, reason, dur_ms: num(d, "dur_ms") ?? 0,
        hunter: str(d, "hunter") ?? "", runner: str(d, "runner") ?? "",
      };
    }
    case "match_end": {
      const deltas: Record<string, number> = {};
      const dv = d["deltas"];
      if (isObj(dv)) {
        for (const [k, v] of Object.entries(dv)) if (typeof v === "number") deltas[k] = v;
      }
      return {
        t: "match_end", winner: str(d, "winner") ?? "", is_draw: d["is_draw"] === true,
        ranked: d["ranked"] === true, deltas,
      };
    }
  }
  return null;
}

/** parseServer reads one WebSocket frame; null if it is not JSON or not known. */
export function parseServer(raw: string): ServerMsg | null {
  let m: unknown;
  try {
    m = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!isObj(m)) return null;
  const type = str(m, "type");
  const room = str(m, "room") ?? "";
  const data = m["data"];
  switch (type) {
    case "Game": {
      const g = parseGame(data);
      return g === null ? null : { type: "Game", room, data: g };
    }
    case "MatchFound": {
      if (!isObj(data)) return null;
      const id = str(data, "room_id") ?? room;
      const players = Array.isArray(data["players"]) ? data["players"].filter((p): p is string => typeof p === "string") : [];
      return id === "" ? null : { type: "MatchFound", room: id, players };
    }
    case "JoinRejected":
      return { type: "JoinRejected", room };
    case "LeaderboardUpdated":
      return { type: "LeaderboardUpdated", board: isObj(data) ? str(data, "board") ?? "" : "" };
    case undefined:
      return null;
  }
  return { type: "Other", name: type };
}
