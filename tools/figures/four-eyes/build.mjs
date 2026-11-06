// Fig: four-eyes on main, drawn as a two-bolt index wheel.
//
// The wheel is `main`; each notch is a revision. It can turn one notch only when
// both bolts are out of their holes: bolt A lifts when a reviewer (never the
// author) turns the key, bolt B when a change with a linked work item is
// loaded. A ratchet pawl lets it turn forward only (fast-forward). Mirrors
// docs/policies/four-eyes.cedar: approvals >= 1, hasWorkItem, fastForward.
//
// node tools/figures/four-eyes/build.mjs  ->  site/figures/four-eyes.html
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import * as k from "../kit/iso-kit.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const OUT = join(HERE, "..", "..", "..", "site", "figures");
const kitScript = () => readFileSync(join(HERE, "..", "kit", "iso-kit.mjs"), "utf8").replace(/^export /gm, "");

const WIDTH = 600;
const HEIGHT = 400;

// ---- world -------------------------------------------------------------
const PLATE = { x: -102, y: -98, w: 168, d: 166, r: 10 };
const PLATE_H = 12;
const FOOT_H = 5;

const W = [-30, -28]; // wheel centre
const WR = 54; // wheel radius at the tooth tips
const ROOT = 51.8; // tooth roots
const TEETH = 48; // four per revision
const BAND = 6; // the ratchet band at the foot of the wheel
const W0 = 6; // wheel underside
const WT = 18; // wheel top
const HOLE_R = 40;
const NOTCH = Math.PI / 6; // one revision = 30 degrees
const LIFT = 6;

// angle a: 0 points to -y, a quarter turn to +x (the kit's radialTicks convention)
const dir = (a) => [Math.sin(a), -Math.cos(a)];
const onWheel = (a, r) => [W[0] + Math.sin(a) * r, W[1] - Math.cos(a) * r];

// two bolt towers, at holes facing the viewer
const TOWERS = [
  { id: "a", a: Math.PI / 2 }, // +x: approval key
  { id: "b", a: Math.PI }, // +y: work-item card
].map((t) => ({ ...t, h: onWheel(t.a, HOLE_R), post: onWheel(t.a, 76), d: dir(t.a) }));

const POINTER_A = (3 * Math.PI) / 4; // main index, between the towers
const PAWL = { pivot: onWheel(0.665 * Math.PI, 64), contact: 0.6 * Math.PI, z: 7.2, h: 3.6 };

const COLLAR = [22, 24.5];
const GLAND = [36, 39];
const HOUSING = [39, 55];
const ARM = [46, 52];
const PLUG_TOP = 57;

const P = k.fitProjection(
  [...k.boxCorners(PLATE, -PLATE_H - FOOT_H, 0), [TOWERS[0].h[0], TOWERS[0].h[1], 74], [TOWERS[1].h[0], TOWERS[1].h[1], 82]],
  WIDTH,
  HEIGHT,
  { pad: 26, azimuth: 45 },
);

const at = (p) => k.iso(p, P);
const S = (paths, style) => k.solidSvg(paths, style);
const L = (d, style) => k.lineSvg(d, style);
const D = (points, style) => k.dotsSvg(points, style);
const byDepth = (pts) => [...pts].sort((a, b) => k.depthOf([a[0], a[1], 0], P) - k.depthOf([b[0], b[1], 0], P));
const roundedPlan = k.roundedPlan;
const rotPlan = (plan, steps, c, ang) =>
  roundedPlan(plan, steps).map(([x, y]) => [c[0] + x * Math.cos(ang) - y * Math.sin(ang), c[1] + x * Math.sin(ang) + y * Math.cos(ang)]);

// ---- parts that move are pure functions, shipped to the page as source ----
// They only call kit functions, which the page has in scope.

function teeth(rot) {
  // ratchet teeth: a radial drop at a0, a ramp down to the root by a0 + 0.92 per
  const per = (2 * Math.PI) / TEETH;
  const at = (a, r) => [W[0] + Math.sin(a) * r, W[1] - Math.cos(a) * r];
  const [vx, vy] = towardViewer(P);
  const list = [];
  for (let i = 0; i < TEETH; i++) {
    const a0 = rot + i * per;
    const a1 = a0 + per * 0.92;
    const mid = a0 + per * 0.3;
    list.push({ front: Math.sin(mid) * vx - Math.cos(mid) * vy > 0, ring: [at(a0, ROOT - 0.3), at(a0 + 0.004, WR), at(a1, ROOT - 0.3)] });
  }
  return list;
}

function rimAt(a, rot) {
  const per = (2 * Math.PI) / TEETH;
  let f = (((a - rot) % per) + per) % per / per;
  return f < 0.92 ? WR - (WR - ROOT) * (f / 0.92) : ROOT;
}

function wheelSvg(rot) {
  const top = WT;
  const ts = teeth(rot);
  const tooth = (t) => solidSvg(extrude(t.ring, W0, BAND, P, {}), { tone: "mid" });
  let out = ts.filter((t) => !t.front).map(tooth).join("");
  out += solidSvg(cylinder(W[0], W[1], ROOT, W0, top - W0, P, 96, 0.6), { tone: "hi", crease: "lo" });
  out += ts.filter((t) => t.front).map(tooth).join("");
  const t = radialTicks(W[0], W[1], 49.5, 72, 6, top, [1.3, 3], P, rot + Math.PI / 12);
  out += lineSvg(ring(W[0], W[1], 49.5, top, P, 96), { tone: "lo" });
  out += lineSvg(t.minor, { tone: "lo" }) + lineSvg(t.major, { tone: "mid" });
  out += lineSvg(ring(W[0], W[1], 44.5, top, P, 80), { tone: "faint" }) + lineSvg(ring(W[0], W[1], 35.5, top, P, 72), { tone: "faint" });
  for (let i = 0; i < 12; i++) {
    const a = rot + i * NOTCH;
    const hx = W[0] + Math.sin(a) * HOLE_R;
    const hy = W[1] - Math.cos(a) * HOLE_R;
    const d = ring(hx, hy, 2.8, top, P, 24);
    out += `<path d="${d}" class="fe-void"/>` + lineSvg(d, { tone: "lo" });
  }
  const spokes = [];
  for (let i = 0; i < 12; i++) {
    const a = rot + i * NOTCH + NOTCH / 2;
    spokes.push([W[0] + Math.sin(a) * 31, W[1] - Math.cos(a) * 31]);
  }
  out += dotsSvg(spokes.map(([x, y]) => iso([x, y, top], P)), { size: 0.45, tone: "mid" });
  return out;
}

function knobSvg(rot) {
  const [x, y] = W;
  const z = WT + 9;
  const a = rot + (3 * Math.PI) / 4;
  return lineSvg(lineOnTop([x, y], [x + Math.sin(a) * 6.2, y - Math.cos(a) * 6.2], z, P), { tone: "hi", free: "end" });
}

function pawlSvg(rot) {
  const { pivot, contact, z, h } = PAWL;
  const r = rimAt(contact, rot);
  const tip = [W[0] + Math.sin(contact) * (r + 0.5), W[1] - Math.cos(contact) * (r + 0.5)];
  const dx = tip[0] - pivot[0];
  const dy = tip[1] - pivot[1];
  const len = Math.hypot(dx, dy);
  const ang = Math.atan2(dy, dx);
  const body = [
    [-3.2, -3.2],
    [len - 2, -1.6],
    [len, 0],
    [len - 2, 1.4],
    [-3.2, 3.2],
  ].map(([u, v]) => [pivot[0] + u * Math.cos(ang) - v * Math.sin(ang), pivot[1] + u * Math.sin(ang) + v * Math.cos(ang)]);
  let out = solidSvg(extrude(body, z, h, P, {}), { tone: "mid" });
  out += solidSvg(cylinder(pivot[0], pivot[1], 2.4, z + h, 1.6, P, 20), { tone: "mid" });
  out += dotsSvg([iso([pivot[0], pivot[1], z + h + 1.6], P)], { size: 0.5, tone: "hi" });
  return out;
}

function helix(cx, cy, r, z0, z1, turns) {
  const [vx, vy] = towardViewer(P);
  const n = Math.round(turns * 28);
  let back = "";
  let front = "";
  let run = null;
  let side = null;
  const flush = () => {
    if (run && run.length > 1) {
      const d = "M" + run.map(([sx, sy]) => sx.toFixed(1) + " " + sy.toFixed(1)).join("L");
      if (side) front += d;
      else back += d;
    }
  };
  for (let i = 0; i <= n; i++) {
    const t = (i / n) * turns * Math.PI * 2;
    const c = Math.cos(t);
    const s = Math.sin(t);
    const p = iso([cx + c * r, cy + s * r, z0 + ((z1 - z0) * i) / n], P);
    const f = c * vx + s * vy > 0;
    if (f !== side) {
      if (run) run.push(p);
      flush();
      run = [p];
      side = f;
    } else run.push(p);
  }
  flush();
  return { back, front };
}

function springSvg(t, lift, part) {
  const h = helix(t.h[0], t.h[1], 3.6, COLLAR[1] + lift, GLAND[0], 5.5);
  return lineSvg(part === "back" ? h.back : h.front, { tone: part === "back" ? "lo" : "mid" });
}

function keySvg(t, phi) {
  const [x, y] = t.h;
  const slot = rotPlan({ x: -2.8, y: -0.45, w: 5.6, d: 0.9, r: 0.45 }, 3, t.h, phi);
  let out = `<path d="${onTop(slot, PLUG_TOP, P, true)}" class="iso-line fe-void" data-tone="lo"/>`;
  out += solidSvg(cylinder(x, y, 1.15, PLUG_TOP, 3.4, P, 16), { tone: "mid", crease: "none" });
  out += solidSvg(cylinder(x, y, 2.1, PLUG_TOP + 3.4, 1.2, P, 20), { tone: "mid" });
  out += solidSvg(cylinder(x, y, 1.15, PLUG_TOP + 4.6, 5, P, 16), { tone: "mid", crease: "none" });
  const bar = rotPlan({ x: -8, y: -1.9, w: 16, d: 3.8, r: 1.9 }, 6, t.h, phi);
  out += solidSvg(extrude(bar, PLUG_TOP + 9.6, 3.4, P, { bevel: 0.4 }), { tone: "hi", crease: "lo" });
  const end = (s) => [x + Math.cos(phi) * s, y + Math.sin(phi) * s];
  out += lineSvg(ring(...end(5.6), 0.9, PLUG_TOP + 13, P, 12) + ring(...end(-5.6), 0.9, PLUG_TOP + 13, P, 12), { tone: "lo" });
  return out;
}

// ---- static drawing ------------------------------------------------------
function base() {
  const out = [`<path class="iso-halo" d="${k.haloOf(PLATE, -PLATE_H - FOOT_H, PLATE_H, P)}"/>`];
  for (const [x, y] of byDepth(k.corners(PLATE, 22))) {
    out.push(S(k.cylinder(x, y, 7, -PLATE_H - FOOT_H, 1.6, P, 28), { tone: "lo" }));
    out.push(S(k.cylinder(x, y, 5.4, -PLATE_H - FOOT_H + 1.6, FOOT_H - 1.6, P, 28), { tone: "mid" }));
  }
  out.push(S(k.slabOf(PLATE, -PLATE_H, PLATE_H, P, 8, 1.6), { tone: "mid", inner: L(k.sideSeam(PLATE, -5, P, 8), { tone: "faint" }) }));
  out.push(L(k.planOutline(k.insetPlan(PLATE, 5), 0, P), { tone: "lo" }));
  const screws = k.corners(PLATE, 10);
  out.push(L(screws.map(([x, y]) => k.ring(x, y, 2.2, 0, P, 16)).join(""), { tone: "lo" }));
  out.push(D(screws.map(([x, y]) => at([x, y, 0])), { size: 0.5 }));

  // ruler strip along the front edge, one tick per revision-sized step
  const strip = { x: -14, y: PLATE.y + PLATE.d - 14, w: 70, d: 7, r: 1 };
  out.push(S(k.slabOf(strip, 0, 0.6, P, 3), { tone: "lo", crease: "none" }));
  const ticks = k.topTicks(strip.x + 4, strip.x + strip.w - 4, 4, 5, strip.y + 0.01, 0.6, [2, 3.6], P);
  out.push(L(ticks.minor, { tone: "lo" }), L(ticks.major, { tone: "mid" }));
  out.push(D([at([strip.x + 2, strip.y + 3.5, 0.6]), at([strip.x + strip.w - 2, strip.y + 3.5, 0.6])], { size: 0.45 }));

  // label plate on the right
  const label = { x: PLATE.x + PLATE.w - 22, y: -88, w: 14, d: 34, r: 1.4 };
  out.push(S(k.slabOf(label, 0, 0.8, P, 4), { tone: "lo" }));
  out.push(L(k.planOutline(k.insetPlan(label, 1.6), 0.8, P, 4), { tone: "faint" }));
  const rules = [
    [label.x + 4.5, 22],
    [label.x + 7.5, 16],
    [label.x + 10.5, 19],
  ];
  out.push(rules.map(([x, len]) => L(k.lineOnTop([x, label.y + 6], [x, label.y + 6 + len], 0.8, P), { tone: "lo", free: true })).join(""));
  out.push(D([at([label.x + 7, label.y + 3, 0.8]), at([label.x + 7, label.y + label.d - 3, 0.8])], { size: 0.45 }));
  return out.join("");
}

function pedestal() {
  const [x, y] = W;
  const out = [`<path d="${k.haloOf({ x: x - 48, y: y - 48, w: 96, d: 96, r: 48 }, 0, 0.1, P)}" class="iso-halo fe-blur"/>`];
  out.push(S(k.cylinder(x, y, 30, 0, 2, P, 64, 0.5), { tone: "lo" }));
  out.push(S(k.cylinder(x, y, 24, 2, W0 - 2, P, 64), { tone: "mid" }));
  const bolts = [];
  for (let i = 0; i < 8; i++) {
    const a = (i / 8) * Math.PI * 2 + 0.2;
    bolts.push([x + Math.sin(a) * 27, y - Math.cos(a) * 27]);
  }
  out.push(D(bolts.filter(([bx, by]) => (bx - x) * 1 + (by - y) * 1 > -20).map(([bx, by]) => at([bx, by, 2])), { size: 0.45 }));
  return out.join("");
}

function hub() {
  const [x, y] = W;
  const out = [];
  out.push(S(k.cylinder(x, y, 11, WT, 2.4, P, 48, 0.4), { tone: "mid" }));
  out.push(S(k.cylinder(x, y, 7, WT + 2.4, 6.6, P, 40), { tone: "mid" }));
  out.push(L(k.knurl(x, y, 7, WT + 3.4, WT + 9, 40, P), { tone: "lo" }));
  out.push(L(k.sideArc(x, y, 7, WT + 3.4, P, 32), { tone: "faint" }));
  out.push(L(k.ring(x, y, 1.4, WT + 9, P, 16), { tone: "mid" }));
  out.push(D([[x + 8.6, y], [x - 8.6, y], [x, y + 8.6], [x, y - 8.6]].map(([a, b]) => at([a, b, WT + 2.4])), { size: 0.45 }));
  return out.join("");
}

function pawlPost() {
  const [x, y] = PAWL.pivot;
  const out = [];
  out.push(S(k.cylinder(x, y, 6, 0, 1.6, P, 28), { tone: "lo" }));
  out.push(S(k.cylinder(x, y, 4, 1.6, PAWL.z - 1.6, P, 28), { tone: "mid" }));
  return out.join("");
}

function boltSvg(t) {
  const [x, y] = t.h;
  const out = [];
  out.push(S(k.cylinder(x, y, 1.7, WT - 0.5, 1.6, P, 20), { tone: "hi" }));
  out.push(S(k.cylinder(x, y, 2.3, WT + 1.1, GLAND[1] + 6 - WT - 1.1, P, 24), { tone: "hi", crease: "none", lit: true }));
  out.push(S(k.cylinder(x, y, 3.9, COLLAR[0], COLLAR[1] - COLLAR[0], P, 28, 0.3), { tone: "hi", lit: true }));
  return out.join("");
}

function housing(t) {
  const [x, y] = t.h;
  const out = [];
  out.push(S(k.cylinder(x, y, 4.6, GLAND[0], GLAND[1] - GLAND[0], P, 28), { tone: "mid" }));
  out.push(
    S(k.cylinder(x, y, 6.6, HOUSING[0], HOUSING[1] - HOUSING[0], P, 40, 0.4), {
      tone: "mid",
      inner: L(k.sideArc(x, y, 6.6, HOUSING[0] + 3, P, 28) + k.sideArc(x, y, 6.6, HOUSING[1] - 3, P, 28), { tone: "faint" }),
    }),
  );
  return out.join("");
}

function plug(t) {
  const [x, y] = t.h;
  const out = [];
  if (t.id === "a") {
    out.push(S(k.cylinder(x, y, 4.6, HOUSING[1], PLUG_TOP - HOUSING[1], P, 32, 0.3), { tone: "mid" }));
    out.push(L(k.ring(x, y, 3.5, PLUG_TOP, P, 32), { tone: "faint" }));
  } else {
    const collar = { x: x - 6, y: y - 2.4, w: 12, d: 4.8, r: 1.2 };
    out.push(S(k.slabOf(collar, HOUSING[1], PLUG_TOP + 1 - HOUSING[1], P, 3, 0.3), { tone: "mid" }));
    const mouth = { x: x - 5, y: y - 0.7, w: 10, d: 1.4, r: 0.6 };
    out.push(`<path d="${k.planOutline(mouth, PLUG_TOP + 1, P, 3)}" class="iso-line fe-void" data-tone="lo"/>`);
    out.push(D([at([x - 4.6, y + 1.7, PLUG_TOP + 1]), at([x + 4.6, y + 1.7, PLUG_TOP + 1])], { size: 0.4 }));
  }
  return out.join("");
}

// the card slides into the slot: clip it to the space above the slot mouth
const CARD = { w: 9, d: 0.8, h: 15, sink: 5 };
function cardSvg(t) {
  const [x, y] = t.h;
  const plan = { x: x - CARD.w / 2, y: y - CARD.d / 2, w: CARD.w, d: CARD.d, r: 0.3 };
  const z0 = PLUG_TOP + 1 - CARD.sink;
  let out = S(k.slabOf(plan, z0, CARD.h, P, 2), { tone: "hi", crease: "lo" });
  const face = k.sideMatrix([x - CARD.w / 2, y + CARD.d / 2], z0 + CARD.h, P, "left");
  out += k.faceTextSvg(face.replace(/\)$/, "") + ") translate(0.9 4)", "PAY-42", { size: 1.9, tone: "mid" });
  out += L(k.segment([x - CARD.w / 2 + 1.2, y + CARD.d / 2, z0 + CARD.h - 6.2], [x + CARD.w / 2 - 1.2, y + CARD.d / 2, z0 + CARD.h - 6.2], P), { tone: "faint", free: true });
  return out;
}
function cardClip(t) {
  const [x, y] = t.h;
  const z = PLUG_TOP + 1;
  const pts = [
    [x - CARD.w / 2 - 1, y - 1.2],
    [x + CARD.w / 2 + 1, y - 1.2],
    [x + CARD.w / 2 + 1, y + 1.2],
    [x - CARD.w / 2 - 1, y + 1.2],
  ];
  const low = pts.map(([a, b]) => at([a, b, z]));
  const high = pts.map(([a, b]) => at([a, b, z + 60]));
  const all = [...low, ...high];
  // hull of the slot footprint swept upward
  const hull = (ps) => {
    const s = [...ps].sort((p, q) => p[0] - q[0] || p[1] - q[1]);
    const cross = (o, a, b) => (a[0] - o[0]) * (b[1] - o[1]) - (a[1] - o[1]) * (b[0] - o[0]);
    const lower = [];
    for (const p of s) {
      while (lower.length >= 2 && cross(lower[lower.length - 2], lower[lower.length - 1], p) <= 0) lower.pop();
      lower.push(p);
    }
    const upper = [];
    for (const p of s.reverse()) {
      while (upper.length >= 2 && cross(upper[upper.length - 2], upper[upper.length - 1], p) <= 0) upper.pop();
      upper.push(p);
    }
    return [...lower.slice(0, -1), ...upper.slice(0, -1)];
  };
  return "M" + hull(all).map(([a, b]) => a.toFixed(1) + " " + b.toFixed(1)).join("L") + "Z";
}

function arm(t) {
  const [hx, hy] = t.h;
  const [px, py] = t.post;
  const out = [];
  // post
  out.push(S(k.cylinder(px, py, 8, 0, 1.8, P, 32), { tone: "lo" }));
  out.push(S(k.cylinder(px, py, 5, 1.8, ARM[0] - 1.8, P, 32), { tone: "mid", inner: L(k.sideArc(px, py, 5, 14, P, 24) + k.sideArc(px, py, 5, 32, P, 24), { tone: "faint" }) }));
  // arm from the housing wall to past the post
  const from = 6.2;
  const plan =
    t.d[0] > 0.5
      ? { x: hx + from, y: hy - 4, w: px + 6 - hx - from, d: 8, r: 1.5 }
      : { x: hx - 4, y: hy + from, w: 8, d: py + 6 - hy - from, r: 1.5 };
  out.push(S(k.slabOf(plan, ARM[0], ARM[1] - ARM[0], P, 3, 0.5), { tone: "mid" }));
  out.push(S(k.cylinder(px, py, 4.2, ARM[1], 1.6, P, 28), { tone: "mid" }));
  out.push(L(k.ring(px, py, 2, ARM[1] + 1.6, P, 16), { tone: "lo" }));
  const mid = [(hx + px) / 2 + t.d[0] * 4, (hy + py) / 2 + t.d[1] * 4];
  out.push(D([at([mid[0] - t.d[1] * 2.2, mid[1] - t.d[0] * 2.2, ARM[1]]), at([mid[0] + t.d[1] * 2.2, mid[1] + t.d[0] * 2.2, ARM[1]])], { size: 0.45 }));
  return out.join("");
}

function pointer() {
  // fixed index: reads the revision that main points at
  const a = POINTER_A;
  const [bx, by] = onWheel(a, 74);
  const block = { x: bx - 7, y: by - 7, w: 14, d: 14, r: 2 };
  const out = [];
  out.push(S(k.slabOf(block, 0, 22, P, 3, 0.5), { tone: "mid", inner: L(k.sideSeam(block, 6, P, 3), { tone: "faint" }) }));
  const u = dir(a);
  const v = [-u[1], u[0]];
  const pt = (r, s) => [W[0] + u[0] * r + v[0] * s, W[1] + u[1] * r + v[1] * s];
  const armRing = [pt(76, -3.4), pt(76, 3.4), pt(52.5, 3.4), pt(49.4, 0.8), pt(49.4, -0.8), pt(52.5, -3.4)];
  out.push(S(k.extrude(armRing, 22, 3, P, { bevel: 0.3 }), { tone: "mid" }));
  const tip = pt(50.2, 0);
  out.push(S(k.cylinder(tip[0], tip[1], 0.55, WT + 0.4, 22 - WT - 0.4, P, 10), { tone: "hi", crease: "none" }));
  out.push(D([at([...pt(66, 0), 25])], { size: 0.45 }));
  out.push(k.faceTextSvg(k.topMatrix([block.x + 2.4, block.y + 8.6], 22, P, "x"), "MAIN", { size: 2.6, tone: "mid" }));
  return out.join("");
}

// ---- assemble ------------------------------------------------------------
const [TA, TB] = TOWERS;
// The page has the kit in scope under the same names; give the build the same.
const scope = { ...k, P };
const bind = (fn) => new Function(...Object.keys(scope), `const W=${JSON.stringify(W)},WR=${WR},ROOT=${ROOT},TEETH=${TEETH},W0=${W0},BAND=${BAND},WT=${WT},HOLE_R=${HOLE_R},NOTCH=${NOTCH},PAWL=${JSON.stringify(PAWL)},COLLAR=${JSON.stringify(COLLAR)},GLAND=${JSON.stringify(GLAND)},PLUG_TOP=${PLUG_TOP};${[teeth, rimAt, helix].map(String).join("\n")}\nconst rotPlan=${rotPlan};\nreturn ${fn};`)(...Object.values(scope));
const live = {
  wheel: bind(wheelSvg),
  knob: bind(knobSvg),
  pawl: bind(pawlSvg),
  spring: bind(springSvg),
  key: bind(keySvg),
};

const tB = [
  `<g data-part="spring-back-b">${live.spring(TB, 0, "back")}</g>`,
  `<g data-part="bolt-b">${boltSvg(TB)}</g>`,
  `<g data-part="spring-front-b">${live.spring(TB, 0, "front")}</g>`,
  housing(TB),
  plug(TB),
  `<clipPath id="fe-card-clip"><path d="${cardClip(TB)}"/></clipPath><g clip-path="url(#fe-card-clip)"><g data-part="card" opacity="0" transform="${k.translateAlong(P, [0, 0, 18])}">${cardSvg(TB)}</g></g>`,
  arm(TB),
].join("");
const tA = [
  `<g data-part="spring-back-a">${live.spring(TA, 0, "back")}</g>`,
  `<g data-part="bolt-a">${boltSvg(TA)}</g>`,
  `<g data-part="spring-front-a">${live.spring(TA, 0, "front")}</g>`,
  housing(TA),
  plug(TA),
  `<g data-part="key">${live.key(TA, 0)}</g>`,
  arm(TA),
].join("");

const svg = k.figureSvg({
  width: WIDTH,
  height: HEIGHT,
  label:
    "An index wheel on a bench plate stands for the branch main; each notch is one revision. Two spring-loaded bolts drop into holes in the wheel. One lifts only when a reviewer who is not the author turns a key; the other lifts when a card with a linked work item is loaded. A ratchet pawl on the rim lets the wheel turn forward only. When both bolts are out the wheel advances one notch, and both bolts drop into the next revision's holes.",
  body: [
    base(),
    pedestal(),
    `<g data-part="wheel">${live.wheel(0)}</g>`,
    hub(),
    `<g data-part="knob">${live.knob(0)}</g>`,
    pawlPost(),
    `<g data-part="pawl">${live.pawl(0)}</g>`,
    tB,
    tA,
    pointer(),
  ],
});

// ---- live ------------------------------------------------------------------
const LIVE = `
const P = ${JSON.stringify(P)};
const W=${JSON.stringify(W)},WR=${WR},ROOT=${ROOT},TEETH=${TEETH},W0=${W0},BAND=${BAND},WT=${WT},HOLE_R=${HOLE_R},NOTCH=${NOTCH},PAWL=${JSON.stringify(PAWL)},COLLAR=${JSON.stringify(COLLAR)},GLAND=${JSON.stringify(GLAND)},PLUG_TOP=${PLUG_TOP},LIFT=${LIFT};
const TA=${JSON.stringify(TA)};
${[teeth, rimAt, helix, wheelSvg, knobSvg, pawlSvg, springSvg, keySvg].map(String).join("\n")}
const rotPlan=${rotPlan};
${readFileSync(join(HERE, "live.js"), "utf8")}
`;

const body = k.plateHtml({
  fig: "Fig 1",
  title: "Four-eyes on main",
  hint: "Click key or card · 1, 2, A",
  readout: "main @ c41 · c42: 0/1 approvals · no work item",
  body: `<div class="iso-stage fe-stage" tabindex="0" role="group" aria-label="Four-eyes index wheel. Press 1 to turn the reviewer's key, 2 to load the work-item card, A to try the author's key.">${svg}</div>`,
});

// The site's CSP allows only same-origin files under /figures/: no inline script, style or style attributes.
const CSS = `${k.ISO_CSS}
*{box-sizing:border-box}
html,body{margin:0;background:transparent}
:root{color-scheme:light dark}
body{color:var(--anatomy-text);font:14px/1.6 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif;-webkit-font-smoothing:antialiased}
body[data-theme="light"]{color-scheme:light}
body[data-theme="dark"]{color-scheme:dark}
.iso-figure{margin:0}
.fe-stage{cursor:pointer;outline:none}
.fe-stage:focus-visible{outline:1px solid var(--anatomy-mid);outline-offset:-1px}
path.fe-void{fill:var(--anatomy-shade-0);stroke:none}
path.iso-line.fe-void{fill:var(--anatomy-shade-0)}
.fe-blur{filter:blur(5px)}
`;
const THEME = `const dark = matchMedia("(prefers-color-scheme: dark)");
const theme = () => (document.body.dataset.theme = dark.matches ? "dark" : "light");
theme();
dark.addEventListener("change", theme);
`;
const html = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Four-eyes on main</title><link rel="stylesheet" href="four-eyes.css"></head><body class="iso" data-theme="light">${body}<script type="module" src="four-eyes.js"></script></body></html>
`;
writeFileSync(join(OUT, "four-eyes.html"), html);
writeFileSync(join(OUT, "four-eyes.css"), CSS);
writeFileSync(join(OUT, "four-eyes.js"), "// Generated by tools/figures/four-eyes/build.mjs. Do not edit.\n// Includes the Anatomy iso kit, MIT License, Copyright (c) 2026 Ryan (tools/figures/kit/LICENSE).\n" + THEME + kitScript() + LIVE);
console.log("wrote", OUT);
