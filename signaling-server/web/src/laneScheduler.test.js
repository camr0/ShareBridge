import assert from 'node:assert/strict';
import test from 'node:test';

import { FRAME_BINARY, FRAME_TEXT } from './frame.js';
import {
  TRAFFIC_CLASS_BULK,
  TRAFFIC_CLASS_CONTROL,
  TRAFFIC_CLASS_INTERACTIVE_MEDIA,
  TRAFFIC_CLASS_THUMBNAIL,
} from './multiLaneProtocol.js';
import {
  LaneScheduler,
  BULK_QUEUE_CAP_BYTES,
  NATIVE_BUFFER_HIGH_WATER_BYTES,
} from './laneScheduler.js';

const Q = 64 * 1024;
const payload = (marker = 0, size = Q) => { const value = new Uint8Array(size); value[0] = marker; return value; };
const deferred = () => { let resolve; let reject; const promise = new Promise((res, rej) => { resolve = res; reject = rej; }); return { promise, resolve, reject }; };
const tick = () => new Promise((resolve) => setImmediate(resolve));

test('scheduler starts continuously queued media and bulk at three to one', async () => {
  const writes = [];
  const scheduler = new LaneScheduler({ write: async (request) => { writes.push(request.className); } });
  const sends = [];
  for (let i = 0; i < 8; i++) sends.push(scheduler.send({ className: TRAFFIC_CLASS_INTERACTIVE_MEDIA, kind: FRAME_BINARY, payload: payload(i) }));
  for (let i = 0; i < 4; i++) sends.push(scheduler.send({ className: TRAFFIC_CLASS_BULK, kind: FRAME_BINARY, payload: payload(i) }));
  await Promise.all(sends);
  assert.deepEqual(writes.slice(0, 8), [TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_BULK, TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_BULK]);
  await scheduler.close();
});

test('scheduler limits a control burst to eight while data waits', async () => {
  const writes = [];
  const scheduler = new LaneScheduler({ write: async (request) => { writes.push(request.className); } });
  const sends = Array.from({ length: 16 }, () => scheduler.send({ className: TRAFFIC_CLASS_CONTROL, kind: FRAME_TEXT, payload: payload() }));
  sends.push(scheduler.send({ className: TRAFFIC_CLASS_BULK, kind: FRAME_BINARY, payload: payload() }));
  await Promise.all(sends);
  assert.ok(writes.indexOf(TRAFFIC_CLASS_BULK) <= 8, `bulk position was ${writes.indexOf(TRAFFIC_CLASS_BULK) + 1}`);
  await scheduler.close();
});

test('fifth bulk sender waits until one of four queued requests drains', async () => {
  const gate = deferred(); let writes = 0;
  const scheduler = new LaneScheduler({ write: async () => { writes++; if (writes === 1) await gate.promise; } });
  const firstFour = Array.from({ length: 4 }, () => scheduler.send({ className: TRAFFIC_CLASS_BULK, kind: FRAME_BINARY, payload: payload() }));
  const fifth = scheduler.send({ className: TRAFFIC_CLASS_BULK, kind: FRAME_BINARY, payload: payload() });
  await tick();
  let fifthDone = false; fifth.then(() => { fifthDone = true; }, () => { fifthDone = true; });
  await tick();
  assert.equal(writes, 1); assert.equal(fifthDone, false); assert.equal(BULK_QUEUE_CAP_BYTES, 256 * 1024);
  gate.resolve(); await Promise.all([...firstFour, fifth]); assert.equal(writes, 5);
  await scheduler.close();
});

test('idle lanes borrow service and media sublanes receive three to one service', async () => {
  for (const [classes, want] of [
    [[TRAFFIC_CLASS_BULK, TRAFFIC_CLASS_BULK, TRAFFIC_CLASS_BULK], [TRAFFIC_CLASS_BULK, TRAFFIC_CLASS_BULK, TRAFFIC_CLASS_BULK]],
    [[TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_INTERACTIVE_MEDIA], [TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_INTERACTIVE_MEDIA]],
    [[TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_THUMBNAIL, TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_THUMBNAIL, TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_INTERACTIVE_MEDIA], [TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_THUMBNAIL, TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_THUMBNAIL]],
    [[TRAFFIC_CLASS_THUMBNAIL, TRAFFIC_CLASS_THUMBNAIL, TRAFFIC_CLASS_THUMBNAIL], [TRAFFIC_CLASS_THUMBNAIL, TRAFFIC_CLASS_THUMBNAIL, TRAFFIC_CLASS_THUMBNAIL]],
  ]) {
    const writes = []; const scheduler = new LaneScheduler({ write: async (request) => { writes.push(request.className); } });
    await Promise.all(classes.map((className) => scheduler.send({ className, kind: FRAME_BINARY, payload: payload() })));
    assert.deepEqual(writes, want); await scheduler.close();
  }
});

test('scheduler copies payloads and exports native high water', async () => {
  const gate = deferred(); let seen;
  const scheduler = new LaneScheduler({ write: async (request) => { await gate.promise; seen = request.payload[0]; } });
  const original = payload(7); const sent = scheduler.send({ className: TRAFFIC_CLASS_BULK, kind: FRAME_BINARY, payload: original });
  original[0] = 99; gate.resolve(); await sent;
  assert.equal(seen, 7); assert.equal(NATIVE_BUFFER_HIGH_WATER_BYTES, 256 * 1024); await scheduler.close();
});

test('scheduler rejects an oversized request clearly', async () => {
  const scheduler = new LaneScheduler({ write: async () => {} });
  await assert.rejects(scheduler.send({ className: TRAFFIC_CLASS_BULK, kind: FRAME_BINARY, payload: payload(0, BULK_QUEUE_CAP_BYTES + 1) }), /too large/i);
  await scheduler.close();
});

test('cancellation restores queue capacity', async () => {
  const gate = deferred(); let writes = 0;
  const scheduler = new LaneScheduler({ write: async () => { writes++; if (writes === 1) await gate.promise; } });
  const controller = new AbortController();
  const first = scheduler.send({ className: TRAFFIC_CLASS_BULK, kind: FRAME_BINARY, payload: payload(1) });
  const cancellable = scheduler.send({ className: TRAFFIC_CLASS_BULK, kind: FRAME_BINARY, payload: payload(2), signal: controller.signal });
  const queued = Array.from({ length: 2 }, (_, i) => scheduler.send({ className: TRAFFIC_CLASS_BULK, kind: FRAME_BINARY, payload: payload(i + 3) }));
  const replacement = scheduler.send({ className: TRAFFIC_CLASS_BULK, kind: FRAME_BINARY, payload: payload(9) });
  await tick(); controller.abort(); await assert.rejects(cancellable, /abort/i); gate.resolve();
  await Promise.all([first, ...queued, replacement]); assert.equal(writes, 4); await scheduler.close();
});

test('writer failure and close reject queued and future senders', async () => {
  const boom = new Error('boom'); const failureGate = deferred();
  const failed = new LaneScheduler({ write: async () => { await failureGate.promise; throw boom; } });
  const firstFailure = failed.send({ className: TRAFFIC_CLASS_CONTROL, kind: FRAME_TEXT, payload: payload() });
  const queuedFailure = failed.send({ className: TRAFFIC_CLASS_BULK, kind: FRAME_BINARY, payload: payload() });
  const firstRejected = assert.rejects(firstFailure, /boom/); const queuedRejected = assert.rejects(queuedFailure, /boom/);
  await tick(); failureGate.resolve(); await Promise.all([firstRejected, queuedRejected]);
  await assert.rejects(failed.send({ className: TRAFFIC_CLASS_CONTROL, kind: FRAME_TEXT, payload: payload() }), /boom/);

  const gate = deferred(); const closing = new LaneScheduler({ write: async () => { await gate.promise; } });
  const active = closing.send({ className: TRAFFIC_CLASS_CONTROL, kind: FRAME_TEXT, payload: payload() });
  const queued = closing.send({ className: TRAFFIC_CLASS_BULK, kind: FRAME_BINARY, payload: payload() });
  await tick(); await closing.close(); await assert.rejects(queued, /closed/i);
  await assert.rejects(closing.send({ className: TRAFFIC_CLASS_BULK, kind: FRAME_BINARY, payload: payload() }), /closed/i);
  await closing.close(); gate.resolve(); await active;
});
