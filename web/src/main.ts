// Start-up: identity → socket → screens. Arriving on the page starts
// matchmaking without a click (section 3.7: playing within 60 seconds,
// nothing explained).
//
// This is the only place that reads the clock and starts timers for the
// game: everything under game/ is handed the time.

import { Api } from "./net/api";
import { Identity, type KV } from "./net/identity";
import { Matchmaker } from "./net/matchmaking";
import { isActive } from "./net/protocol";
import { GameSocket, type Timers, type WSCtor } from "./net/socket";
import { Game, type GameEvent, type View } from "./game/game";
import { Loop } from "./game/loop";
import { KeyboardInput } from "./input/keyboard";
import { MouseInput } from "./input/mouse";
import { TouchInput } from "./input/touch";
import { NO_INPUT, type Input } from "./input/input";
import { CanvasView } from "./render/canvas";
import { Hud } from "./render/hud";
import { Screens } from "./render/screens";
import { Board } from "./render/board";
import { Overlay, push } from "./debug/overlay";

const BOARD_ROWS = 5;
const RENEWAL_CHECK_MS = 60_000;

const debug = new URLSearchParams(location.search).get("debug") === "1";

// ---- the page ----------------------------------------------------------

function div(cls: string, parent: HTMLElement, tag: "div" | "header" | "main" | "aside" = "div"): HTMLElement {
  const e = document.createElement(tag);
  e.className = cls;
  parent.append(e);
  return e;
}

const app = document.querySelector<HTMLDivElement>("#app")!;
const top = div("top", app, "header");
const stage = div("stage", app, "main");
const canvasEl = document.createElement("canvas");
stage.append(canvasEl);
const screenEl = div("screen", stage);
const boardEl = div("board", app, "aside");

const hud = new Hud(top);
const canvas = new CanvasView(canvasEl);
const screens = new Screens(screenEl);
const board = new Board(boardEl);
const overlay = debug ? new Overlay(document.body) : null;

const fit = () => {
  const side = Math.max(120, Math.floor(Math.min(stage.clientWidth, stage.clientHeight)) - 8);
  canvas.resize(side);
};
new ResizeObserver(fit).observe(stage);
fit();

// ---- the network -------------------------------------------------------

function storage(): KV {
  try {
    const s = window.localStorage;
    s.getItem("probe");
    return s;
  } catch {
    // Blocked storage: an identity for this page only.
    const m = new Map<string, string>();
    return { getItem: (k) => m.get(k) ?? null, setItem: (k, v) => void m.set(k, v), removeItem: (k) => void m.delete(k) };
  }
}

const timers: Timers = {
  setTimeout: (f, ms) => window.setTimeout(f, ms),
  clearTimeout: (id) => window.clearTimeout(id as number),
  setInterval: (f, ms) => window.setInterval(f, ms),
  clearInterval: (id) => window.clearInterval(id as number),
};

const api = new Api((input, init) => window.fetch(input, init));
const identity = new Identity(api, storage());
const socket = new GameSocket({
  // The browser's WebSocket, seen through the part of it the socket uses.
  WebSocket: WebSocket as unknown as WSCtor,
  url: (token) => `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/ws?token=${encodeURIComponent(token)}`,
  identity: { token: async () => (await identity.ensure(Date.now())).token, renew: () => identity.renew() },
  timers,
  random: Math.random,
  now: () => performance.now(),
});
const matchmaker = new Matchmaker(api, identity, socket, timers);

let game: Game | null = null;
let trouble = false;
let lookingNote = "";

function inMatch(): boolean {
  return game !== null && game.room.state !== "FINISHED";
}

async function refreshBoard(): Promise<void> {
  try {
    board.render(await api.board(BOARD_ROWS), identity.current()?.username ?? "");
  } catch {
    // The board is a summary; it waits for the next refresh.
  }
}

let boardTimer: number | null = null;
function refreshBoardSoon(): void {
  if (boardTimer !== null) return;
  boardTimer = window.setTimeout(() => {
    boardTimer = null;
    void refreshBoard();
  }, 500);
}

/** renewEarly is the first row of section 3.6: out of a match, close to expiry. */
async function renewEarly(): Promise<void> {
  try {
    if (await identity.renewIfDue(Date.now(), inMatch())) socket.reopen();
  } catch {
    // Tried again at the next check, or on a 401 or a 1008.
  }
}

matchmaker.onMatch = (room) => {
  const me = identity.current();
  if (me === null) return;
  game = new Game(me.playerId, room);
  document.body.dataset["room"] = room;
};

socket.onOpen = () => matchmaker.onSocketOpen();
socket.onTrouble = (t) => {
  trouble = t;
};
socket.onMessage = (m) => {
  if (matchmaker.handle(m)) return;
  if (m.type === "LeaderboardUpdated" && m.board === "ghosttag") refreshBoardSoon();
  if (m.type !== "Game" || game === null || m.room !== game.roomId) return;
  const now = performance.now();
  const before = game.room.state;
  const ev = game.onMessage(m.data, now);
  if (ev !== null) onGameEvent(game, ev, before);
};

function onGameEvent(g: Game, ev: GameEvent, before: string | null): void {
  switch (ev.kind) {
    case "state":
      loop.setSendInterval(isActive(ev.to) && g.cfg !== null ? g.cfg.send_ms : 0);
      break;
    case "match_end":
      loop.setSendInterval(0);
      if (before === "WAITING" || before === null) {
        // The room was cancelled before it began (the opponent never
        // joined): nothing was played, so look again.
        socket.leave();
        game = null;
        matchmaker.requeues++;
        matchmaker.start();
        return;
      }
      showEnd(g);
      refreshBoardSoon();
      break;
  }
}

screens.onPlayAgain = () => {
  socket.leave();
  game = null;
  delete document.body.dataset["room"];
  screens.looking("");
  void renewEarly().then(() => matchmaker.start());
};

// ---- the screens -------------------------------------------------------

function roleSentence(g: Game): string {
  const cfg = g.cfg;
  const s = cfg === null ? "" : `${cfg.trail_ms / 1000} s`;
  return g.room.myRole() === "HUNTER"
    ? `Catch the runner: you see it live, it sees only your trail, ${s} late.`
    : `Escape the hunter: you see only where it was ${s} ago.`;
}

function halfLine(g: Game, half: number): string {
  const h = g.room.halves.find((x) => x.half === half);
  if (h === undefined) return "";
  const who = h.hunter === g.me ? "You hunted" : "You ran";
  const how = h.reason === "CAPTURE" ? `caught in ${(h.dur_ms / 1000).toFixed(1)} s` : "no catch";
  return `Half ${half}: ${who}, ${how}.`;
}

function showEnd(g: Game): void {
  const end = g.room.end;
  if (end === null) return;
  let title: string;
  if (end.is_draw) title = "Draw";
  else if (end.winner === "") title = "No result";
  else title = end.winner === g.me ? "You win" : "You lose";
  const delta = end.deltas[g.me] ?? 0;
  const points = end.ranked ? `${delta >= 0 ? "+" : ""}${delta} points` : "Unranked match: no points.";
  const halves = [halfLine(g, 1), halfLine(g, 2)].filter((x) => x !== "").join(" ");
  screens.end(title, points, halves);
}

function updateScreens(g: Game | null, now: number): void {
  if (g === null) {
    if (screens.current !== "looking" && screens.current !== "end") screens.looking("");
    if (screens.current === "looking") {
      screens.looking(matchmaker.state === "matched" ? "Match found, joining…" : lookingNote);
    }
    return;
  }
  const opp = g.room.opponent();
  const oppName = opp?.name ?? "";
  switch (g.room.state) {
    case null:
    case "WAITING":
      screens.looking("Opponent found, waiting for them to join…");
      break;
    case "COUNTDOWN_1":
      screens.countdown(g.room.myRole(), roleSentence(g), g.room.remaining(now), oppName);
      break;
    case "ROLE_SWAP":
      screens.swap(g.room.myRole(), roleSentence(g), g.room.remaining(now), halfLine(g, 1));
      break;
    case "HALF_1_ACTIVE":
    case "HALF_2_ACTIVE":
      if (screens.current !== "none") screens.hide();
      break;
    case "DISCONNECT_GRACE":
      screens.grace(opp !== undefined && !opp.connected ? "Opponent disconnected, waiting for them…" : "Reconnecting…");
      break;
    case "FINISHED":
      if (screens.current !== "end") showEnd(g);
      break;
  }
}

// ---- input, the loop ---------------------------------------------------

const mouse = new MouseInput(document.documentElement);
const keyboard = new KeyboardInput(window);
const touch = new TouchInput(stage, () => Math.min(64, canvasEl.clientWidth * 0.15));

function readInput(view: View | null): { input: Input; version: number } {
  const version = mouse.version + keyboard.version + touch.version;
  const k = keyboard.read() ?? touch.read();
  if (k !== null) return { input: k, version };
  if (view !== null) {
    const c = canvas.toClient(view, view.me);
    const m = mouse.read(c.x, c.y, canvasEl.clientWidth * 0.25);
    if (m !== null) return { input: m, version };
  }
  return { input: NO_INPUT, version };
}

let appliedVersion = -1;
let lastDrawn: { x: number; y: number } | null = null;
let statsAt = 0;
let lastIn = 0;
let lastOut = 0;
let inRate = 0;
let outRate = 0;

function wall(): number {
  return performance.timeOrigin + performance.now();
}

function frame(now: number, dtMs: number): void {
  const g = game;
  let view = g?.view(now) ?? null;
  const { input, version } = readInput(view);
  if (g !== null && g.canMove()) {
    const moved = g.frame(now, dtMs, input, (g.cfg?.send_ms ?? 0) + (socket.rtt ?? 0) / 2);
    if (overlay !== null && moved && version !== appliedVersion) {
      appliedVersion = version;
      push(overlay.probe.input, { t: wall(), x: g.pred.x, y: g.pred.y });
    }
    view = g.view(now);
  }
  canvas.draw(view);

  if (overlay !== null && view !== null) {
    const o = view.opponent ?? (view.trail[0] !== undefined ? view.trail[0] : null);
    if (o !== null && (lastDrawn === null || o.x !== lastDrawn.x || o.y !== lastDrawn.y)) {
      lastDrawn = { x: o.x, y: o.y };
      push(overlay.probe.drawn, { t: wall(), x: o.x, y: o.y, live: view.opponent !== null });
    }
  }

  const st = g?.room.state ?? null;
  document.body.dataset["state"] = st ?? "NONE";
  document.body.dataset["role"] = g?.room.myRole() ?? "";
  hud.update({
    role: g?.room.myRole() ?? "",
    remainingMs: g !== null && st !== null && st !== "WAITING" ? g.room.remaining(now) : null,
    half: g?.room.half() ?? 1,
    done: g?.room.halves.length ?? 0,
    opponent: g?.room.opponent()?.name ?? "",
    note: trouble ? "reconnecting…" : "",
  });
  updateScreens(g, now);

  if (overlay !== null && now - statsAt >= 1000) {
    const posIn = g?.posIn ?? 0;
    const posOut = g?.posOut ?? 0;
    const span = (now - statsAt) / 1000;
    inRate = Math.max(0, posIn - lastIn) / span;
    outRate = Math.max(0, posOut - lastOut) / span;
    lastIn = posIn;
    lastOut = posOut;
    statsAt = now;
    overlay.update({
      rtt: socket.rtt,
      inPerSec: inRate,
      outPerSec: outRate,
      clamps: g?.pred.clamps ?? 0,
      lastCorrection: g?.pred.lastCorrection ?? 0,
      interp: g?.interpState(now) ?? "-",
      radius: g !== null && g.cfg !== null ? g.room.radius(now) : null,
      state: `${st ?? "-"} / ${matchmaker.state}`,
      renewals: identity.renewals,
      requeues: matchmaker.requeues,
    });
  }
}

function send(now: number): void {
  const g = game;
  if (g === null) return;
  const p = g.packet(now);
  if (p === null) return;
  if (socket.send({ action: "relay", room: g.roomId, data: p }) && overlay !== null) {
    push(overlay.probe.sent, { t: wall(), x: p.x, y: p.y });
  }
}

const loop = new Loop(
  {
    now: () => performance.now(),
    raf: (cb) => window.requestAnimationFrame(cb),
    setInterval: (f, ms) => window.setInterval(f, ms),
    clearInterval: (id) => window.clearInterval(id as number),
  },
  frame,
  send,
);

// ---- start -------------------------------------------------------------

async function boot(): Promise<void> {
  screens.looking("");
  loop.start();
  void refreshBoard();
  for (let attempt = 0; ; attempt++) {
    try {
      await identity.ensure(Date.now());
      break;
    } catch {
      lookingNote = "Cannot reach the server, retrying…";
      await new Promise((r) => window.setTimeout(r, Math.min(5000, 1000 * (attempt + 1))));
    }
  }
  lookingNote = "";
  socket.connect();
  matchmaker.start();
  window.setInterval(() => void renewEarly(), RENEWAL_CHECK_MS);
}

void boot();
