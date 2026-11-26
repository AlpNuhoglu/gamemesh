import { expect, test } from "vitest";
import { TAIL_MS, Trail } from "../src/game/trail";

// The trail is driven with local receive times the test chooses.
const TRAIL = 2000;

function filled(): Trail {
  const tr = new Trail(TRAIL);
  // The hunter at x = t / 10 from t = 0 to t = 3000, a packet every 40 ms.
  for (let t = 0; t <= 3000; t += 40) tr.push(t, t / 10, 0);
  return tr;
}

test("nothing is drawn while the buffer is younger than trail_ms", () => {
  const tr = new Trail(TRAIL);
  tr.push(1000, 1, 1);
  tr.push(1040, 2, 2);
  expect(tr.head(2999)).toBeNull(); // would need data from 999
  expect(tr.tail(2999)).toEqual([]);
  expect(tr.head(3000)).toEqual({ x: 1, y: 1 });
});

test("the drawn point is the position from trail_ms ago, never the live one", () => {
  const tr = filled();
  // At 3000 the newest packet says x = 300; the drawn one is from 1000.
  expect(tr.head(3000)).toEqual({ x: 100, y: 0 });
  // Between two packets it is interpolated.
  const h = tr.head(3020)!;
  expect(h.x).toBeCloseTo(102, 9);
});

test("the tail runs from the head back TAIL_MS, head first", () => {
  const tr = filled();
  const tail = tr.tail(3000);
  expect(tail[0]).toEqual({ x: 100, y: 0, age: 0 });
  const ages = tail.map((p) => p.age);
  expect(Math.max(...ages)).toBeLessThanOrEqual(TAIL_MS);
  expect(Math.max(...ages)).toBeGreaterThan(TAIL_MS - 40);
  for (let i = 1; i < ages.length; i++) expect(ages[i]!).toBeGreaterThan(ages[i - 1]!);
});

test("it forgets what is older than trail_ms + TAIL_MS", () => {
  const tr = filled();
  // At 3000 the buffer holds 500..3000: 2500 ms.
  const kept = tr.size;
  expect(kept).toBe(Math.floor((TRAIL + TAIL_MS) / 40) + 1);
  // The head of a time whose data is forgotten is gone.
  tr.forget(5000);
  expect(tr.head(4000)).toBeNull(); // would need 2000, older than 5000 - 2500
  expect(tr.size).toBe(Math.floor((3000 - 2500) / 40) + 1);
});

test("clear empties it (a new half)", () => {
  const tr = filled();
  tr.clear();
  expect(tr.head(3000)).toBeNull();
});
