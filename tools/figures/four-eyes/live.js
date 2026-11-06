// Live behaviour for the four-eyes figure. Inlined after the kit and the
// shared part builders (wheelSvg, keySvg, ...), which it calls by name.
const stage = document.querySelector(".fe-stage");
const svg = stage.querySelector("svg");
const part = (name) => svg.querySelector(`[data-part="${name}"]`);
const els = {
  wheel: part("wheel"),
  knob: part("knob"),
  pawl: part("pawl"),
  key: part("key"),
  card: part("card"),
  bolt: { a: part("bolt-a"), b: part("bolt-b") },
  back: { a: part("spring-back-a"), b: part("spring-back-b") },
  front: { a: part("spring-front-a"), b: part("spring-front-b") },
};
const TB = { id: "b", h: [W[0] + Math.sin(Math.PI) * HOLE_R, W[1] - Math.cos(Math.PI) * HOLE_R] };
const readout = document.querySelector("[data-readout]");
const still = matchMedia("(prefers-reduced-motion: reduce)");
const smooth = (x) => (x <= 0 ? 0 : x >= 1 ? 1 : x * x * (3 - 2 * x));
const QUARTER = Math.PI / 2;

// --- the model: what the server checks before main may move ------------------
// policy (docs/policies/four-eyes.cedar): approvals >= 1, hasWorkItem, fastForward
const model = {
  rev: 41, // main points at c41; c42 is the change waiting to be promoted
  approved: false, // a non-author approved this exact revision
  workItem: false, // the change links a work item
  refused: 0, // time the author's key was last refused
  phase: "waiting", // waiting | moving | settling
};

// --- motion state -----------------------------------------------------------
const key = { at: 0, rate: 0, target: 0 };
const card = { at: 0, rate: 0, target: 0 };
const wheel = { at: 0, rate: 0, target: 0 };
let lift = { a: 0, b: 0 };
let shown = { key: NaN, card: NaN, wheel: NaN, a: NaN, b: NaN };

function spring(s, stiff, damp, dt, calm) {
  if (calm) {
    s.at = s.target;
    s.rate = 0;
    return true;
  }
  for (let i = 0; i < 4; i++) {
    s.rate += ((s.target - s.at) * stiff - s.rate * damp) * (dt / 4);
    s.at += s.rate * (dt / 4);
  }
  if (Math.abs(s.target - s.at) < 1e-4 && Math.abs(s.rate) < 1e-4) {
    s.at = s.target;
    s.rate = 0;
    return true;
  }
  return false;
}

const r = (n) => "c" + n;
function say() {
  const next = r(model.rev + 1);
  let text;
  if (model.phase === "moving") text = `main → ${next} · fast-forward`;
  else if (!model.approved && performance.now() - model.refused < 1600) text = `main @ ${r(model.rev)} · ${next}: author can't approve`;
  else text = `main @ ${r(model.rev)} · ${next}: ${model.approved ? 1 : 0}/1 approvals · ${model.workItem ? "PAY-42" : "no work item"}`;
  if (readout.textContent !== text) readout.textContent = text;
}

// --- actions ------------------------------------------------------------------
function approve() {
  if (model.phase !== "waiting" || model.approved) return;
  model.approved = true;
  key.target = QUARTER;
  run();
}
function author() {
  if (model.phase !== "waiting" || model.approved) return;
  model.refused = performance.now();
  key.target = 0.22; // turns a hair, hits the ward, springs back
  setTimeout(() => {
    if (!model.approved) key.target = 0;
    run();
  }, 220);
  run();
}
function load() {
  if (model.phase !== "waiting" || model.workItem) return;
  model.workItem = true;
  card.target = 1;
  run();
}

// --- tour ------------------------------------------------------------------------
const TOUR = [
  [1.4, author],
  [3.6, approve],
  [5.6, load],
];
let touring = !still.matches;
let clock = 0;
let step = 0;
let idle = 0;
let visible = false;
let frame = 0;
let last = 0;
let settleAt = 0;
let clearSince = 0;

function pause() {
  touring = false;
  clearTimeout(idle);
  idle = setTimeout(() => {
    touring = !still.matches;
    run();
  }, 7000);
}

function draw() {
  if (wheel.at !== shown.wheel) {
    els.wheel.innerHTML = wheelSvg(wheel.at);
    els.knob.innerHTML = knobSvg(wheel.at);
    els.pawl.innerHTML = pawlSvg(wheel.at);
    shown.wheel = wheel.at;
  }
  if (key.at !== shown.key) {
    els.key.innerHTML = keySvg(TA, key.at);
    shown.key = key.at;
  }
  if (card.at !== shown.card) {
    const c = Math.max(0, Math.min(1.05, card.at));
    els.card.setAttribute("transform", translateAlong(P, [0, 0, (1 - c) * 18]));
    els.card.style.opacity = String(smooth(c * 2.2));
    shown.card = card.at;
  }
  for (const id of ["a", "b"]) {
    if (lift[id] === shown[id]) continue;
    const t = id === "a" ? TA : TB;
    els.bolt[id].setAttribute("transform", translateAlong(P, [0, 0, lift[id]]));
    els.back[id].innerHTML = springSvg(t, lift[id], "back");
    els.front[id].innerHTML = springSvg(t, lift[id], "front");
    shown[id] = lift[id];
  }
  say();
}

function tick(now) {
  const dt = last ? Math.min((now - last) / 1000, 1 / 30) : 1 / 60;
  last = now;
  const calm = still.matches;

  if (touring && model.phase === "waiting") {
    clock += dt;
    while (step < TOUR.length && clock >= TOUR[step][0]) TOUR[step++][1]();
  }

  let busy = false;
  busy = !spring(key, 140, 17, dt, calm) || busy;
  busy = !spring(card, 60, 13, dt, calm) || busy;
  busy = !spring(wheel, 26, 10.2, dt, calm) || busy;
  // a bolt rises over the last part of the key's quarter turn / the card's travel
  lift.a = LIFT * smooth((key.at - 0.5) / (QUARTER - 0.6));
  lift.b = LIFT * smooth((card.at - 0.55) / 0.4);

  const clear = model.approved && model.workItem && lift.a > LIFT - 0.02 && lift.b > LIFT - 0.02;
  if (!clear) clearSince = 0;
  else if (!clearSince) clearSince = now;
  if (model.phase === "waiting" && clear && now - clearSince > (calm ? 0 : 900)) {
    model.phase = "moving";
    wheel.target += NOTCH; // the ratchet only lets it go this way
    busy = true;
  }
  if (model.phase === "moving" && Math.abs(wheel.target - wheel.at) < 0.003) {
    // main now points at the promoted change; its approval is spent, the card comes out
    wheel.at = wheel.target;
    wheel.rate = 0;
    model.rev += 1;
    model.phase = "settling";
    model.approved = false;
    model.workItem = false;
    key.target = 0;
    card.target = 0;
    settleAt = now;
    busy = true;
  }
  if (model.phase === "settling" && key.at < 0.01 && card.at < 0.01) {
    model.phase = "waiting";
    if (touring) {
      clock = -2.4;
      step = 0;
    }
  }
  if (model.phase !== "waiting" || performance.now() - model.refused < 1700 || clearSince) busy = true;

  draw();
  frame = visible && (busy || touring) ? requestAnimationFrame(tick) : 0;
}

function run() {
  if (!frame && visible) {
    last = 0;
    frame = requestAnimationFrame(tick);
  }
}

new IntersectionObserver(
  (entries) => {
    visible = entries[entries.length - 1].isIntersecting;
    if (visible) run();
  },
  { rootMargin: "80px 0px" },
).observe(stage);

// click the key or the card: whichever is nearer the pointer
const screenOf = (p) => {
  const pt = svg.createSVGPoint();
  const [x, y] = iso(p, P);
  pt.x = x;
  pt.y = y;
  return pt.matrixTransform(svg.getScreenCTM());
};
stage.addEventListener("click", (event) => {
  const k = screenOf([TA.h[0], TA.h[1], PLUG_TOP + 10]);
  const c = screenOf([TB.h[0], TB.h[1], PLUG_TOP + 10]);
  const dk = Math.hypot(event.clientX - k.x, event.clientY - k.y);
  const dc = Math.hypot(event.clientX - c.x, event.clientY - c.y);
  pause();
  if (dk < dc) approve();
  else load();
});
stage.addEventListener("keydown", (event) => {
  const map = { 1: approve, 2: load, a: author, A: author };
  const fn = map[event.key];
  if (!fn) return;
  event.preventDefault();
  pause();
  fn();
});
still.addEventListener("change", () => {
  touring = !still.matches;
  run();
});
draw();
