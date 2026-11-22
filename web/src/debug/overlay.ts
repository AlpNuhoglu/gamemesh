// The developer mode behind ?debug=1 (section 3.8): RTT, packets per second
// in and out, the clamp count, the interpolation buffer, the current R(t).
//
// In this mode only, a probe on window records timestamps
// (performance.timeOrigin + performance.now()) of an input being applied to
// the own character, a pos packet being sent, and the opponent first being
// drawn at a new place: the browser latency measurement (section 9, Note B)
// is built on it. Nothing else may depend on it.

export interface ProbeEntry {
  t: number;
  x: number;
  y: number;
}

export interface Probe {
  input: ProbeEntry[];
  sent: ProbeEntry[];
  drawn: (ProbeEntry & { live: boolean })[];
}

const PROBE_CAP = 5000;

export function push<T>(list: T[], e: T): void {
  list.push(e);
  if (list.length > PROBE_CAP) list.splice(0, list.length - PROBE_CAP);
}

export interface DebugStats {
  rtt: number | null;
  inPerSec: number;
  outPerSec: number;
  clamps: number;
  interp: string;
  radius: number | null;
  state: string;
  renewals: number;
  requeues: number;
}

export class Overlay {
  private readonly pre: HTMLPreElement;
  readonly probe: Probe = { input: [], sent: [], drawn: [] };

  constructor(root: HTMLElement) {
    this.pre = document.createElement("pre");
    this.pre.className = "debug";
    root.append(this.pre);
    (window as unknown as { ghosttagProbe: Probe }).ghosttagProbe = this.probe;
  }

  update(s: DebugStats): void {
    this.pre.textContent = [
      `state     ${s.state}`,
      `rtt       ${s.rtt === null ? "-" : `${s.rtt.toFixed(1)} ms`}`,
      `pos in/s  ${s.inPerSec.toFixed(1)}`,
      `pos out/s ${s.outPerSec.toFixed(1)}`,
      `clamps    ${s.clamps}`,
      `interp    ${s.interp}`,
      `R(t)      ${s.radius === null ? "-" : s.radius.toFixed(1)}`,
      `renewals  ${s.renewals}  requeues ${s.requeues}`,
    ].join("\n");
  }
}
