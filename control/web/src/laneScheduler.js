import {
  TRAFFIC_CLASS_BULK,
  TRAFFIC_CLASS_CONTROL,
  TRAFFIC_CLASS_INTERACTIVE_MEDIA,
  TRAFFIC_CLASS_THUMBNAIL,
} from './multiLaneProtocol.js';
import { FRAME_BINARY, FRAME_TEXT } from './frame.js';

export const BASE_QUANTUM_BYTES = 64 * 1024;
export const CONTROL_BURST_LIMIT = 8;
export const CONTROL_QUEUE_CAP_BYTES = 8 * 1024 * 1024;
export const INTERACTIVE_MEDIA_QUEUE_CAP_BYTES = 512 * 1024;
export const THUMBNAIL_QUEUE_CAP_BYTES = 2 * 1024 * 1024;
export const BULK_QUEUE_CAP_BYTES = 256 * 1024;
export const NATIVE_BUFFER_HIGH_WATER_BYTES = 256 * 1024;

const classes = [TRAFFIC_CLASS_CONTROL, TRAFFIC_CLASS_INTERACTIVE_MEDIA, TRAFFIC_CLASS_THUMBNAIL, TRAFFIC_CLASS_BULK];
const caps = new Map([
  [TRAFFIC_CLASS_CONTROL, CONTROL_QUEUE_CAP_BYTES],
  [TRAFFIC_CLASS_INTERACTIVE_MEDIA, INTERACTIVE_MEDIA_QUEUE_CAP_BYTES],
  [TRAFFIC_CLASS_THUMBNAIL, THUMBNAIL_QUEUE_CAP_BYTES],
  [TRAFFIC_CLASS_BULK, BULK_QUEUE_CAP_BYTES],
]);

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

function abortError() {
  const error = new Error('request aborted');
  error.name = 'AbortError';
  return error;
}

export class LaneScheduler {
  constructor({ write }) {
    if (typeof write !== 'function') throw new TypeError('write must be a function');
    this.write = write;
    this.queues = new Map(classes.map((className) => [className, []]));
    this.bytes = new Map(classes.map((className) => [className, 0]));
    this.waiting = new Map(classes.map((className) => [className, []]));
    this.terminal = null;
    this.active = null;
    this.pumpScheduled = false;
    this.controlBurst = 0;
    this.outerCurrent = 0;
    this.outerDeficit = [0, 0];
    this.outerStarted = false;
    this.innerCurrent = 0;
    this.innerDeficit = [0, 0];
    this.innerStarted = false;
  }

  // Scheduled frames require a non-empty payload; lane envelopes are validated separately.
  async send({ className, kind, payload, signal }) {
    if (!caps.has(className)) throw new RangeError(`unknown traffic class: ${className}`);
    if (kind !== FRAME_TEXT && kind !== FRAME_BINARY) throw new RangeError(`unknown frame kind: ${kind}`);
    if (!(payload instanceof Uint8Array)) throw new TypeError('payload must be a Uint8Array');
    if (payload.byteLength === 0) throw new RangeError('scheduled payload must be non-empty');
    if (payload.length > caps.get(className)) throw new RangeError(`request too large for ${className} queue`);
    if (this.terminal) throw this.terminal;
    if (signal?.aborted) throw abortError();

    const completion = deferred();
    // Queue caps bound admitted payload bytes. A blocked payload remains caller-owned until enqueue.
    const request = { className, kind, payload, signal, completion, state: 'new', abortHandler: null };
    await this.admit(request);
    return completion.promise;
  }

  close() {
    if (!this.terminal) {
      this.terminal = new Error('lane scheduler closed');
      this.failWaiting(this.terminal);
      this.failQueued(this.terminal);
    }
    return Promise.resolve();
  }

  admit(request) {
    if (this.terminal) return Promise.reject(this.terminal);
    const className = request.className;
    if (this.waiting.get(className).length === 0 && this.bytes.get(className) + request.payload.length <= caps.get(className)) {
      this.enqueue(request);
      return Promise.resolve();
    }
    const admission = deferred();
    request.state = 'waiting';
    request.admission = admission;
    this.waiting.get(className).push(request);
    this.attachAbort(request);
    return admission.promise;
  }

  enqueue(request) {
    request.payload = new Uint8Array(request.payload);
    request.state = 'queued';
    this.bytes.set(request.className, this.bytes.get(request.className) + request.payload.length);
    this.queues.get(request.className).push(request);
    this.attachAbort(request);
    this.schedulePump();
  }

  attachAbort(request) {
    if (!request.signal || request.abortHandler) return;
    request.abortHandler = () => this.cancel(request);
    request.signal.addEventListener('abort', request.abortHandler, { once: true });
  }

  detachAbort(request) {
    if (request.signal && request.abortHandler) request.signal.removeEventListener('abort', request.abortHandler);
    request.abortHandler = null;
  }

  cancel(request) {
    const error = abortError();
    if (request.state === 'waiting') {
      const queue = this.waiting.get(request.className);
      const index = queue.indexOf(request);
      if (index >= 0) queue.splice(index, 1);
      request.state = 'done';
      request.admission.reject(error);
      this.detachAbort(request);
      this.drainAdmissions(request.className);
      if (this.queues.get(request.className).length === 0) this.resetEmptyLane(request.className);
      return;
    }
    if (request.state === 'queued') {
      const queue = this.queues.get(request.className);
      const index = queue.indexOf(request);
      if (index >= 0) queue.splice(index, 1);
      this.bytes.set(request.className, this.bytes.get(request.className) - request.payload.length);
      request.state = 'done';
      request.completion.reject(error);
      this.detachAbort(request);
      if (queue.length === 0) this.resetEmptyLane(request.className);
      this.drainAdmissions(request.className);
    }
  }

  schedulePump() {
    if (this.pumpScheduled || this.active || this.terminal) return;
    this.pumpScheduled = true;
    queueMicrotask(() => { this.pumpScheduled = false; void this.pump(); });
  }

  async pump() {
    if (this.active || this.terminal) return;
    const request = this.next();
    if (!request) return;
    this.active = request;
    request.state = 'active';
    this.detachAbort(request);
    let error = null;
    try {
      await this.write({ className: request.className, kind: request.kind, payload: request.payload });
    } catch (caught) {
      error = caught instanceof Error ? caught : new Error(String(caught));
    }
    this.active = null;
    this.bytes.set(request.className, this.bytes.get(request.className) - request.payload.length);
    request.state = 'done';
    if (error) {
      if (!this.terminal) {
        this.terminal = error;
        this.failWaiting(error);
        this.failQueued(error);
      }
      request.completion.reject(error);
      return;
    }
    request.completion.resolve();
    this.drainAdmissions(request.className);
    this.schedulePump();
  }

  drainAdmissions(className) {
    const waiting = this.waiting.get(className);
    while (waiting.length) {
      const request = waiting[0];
      if (this.bytes.get(className) + request.payload.length > caps.get(className)) break;
      waiting.shift();
      this.detachAbort(request);
      this.enqueue(request);
      request.admission.resolve();
    }
  }

  next() {
    const dataWaiting = this.hasMedia() || this.queues.get(TRAFFIC_CLASS_BULK).length > 0;
    if (this.queues.get(TRAFFIC_CLASS_CONTROL).length && (this.controlBurst < CONTROL_BURST_LIMIT || !dataWaiting)) {
      this.controlBurst++;
      return this.pop(TRAFFIC_CLASS_CONTROL);
    }
    if (dataWaiting) {
      const request = this.nextData();
      if (request) { this.controlBurst = 0; return request; }
    }
    if (this.queues.get(TRAFFIC_CLASS_CONTROL).length) {
      this.controlBurst++;
      return this.pop(TRAFFIC_CLASS_CONTROL);
    }
    return null;
  }

  nextData() {
    if (!this.outerStarted) { this.addOuterRound(); this.outerStarted = true; }
    while (this.hasMedia() || this.queues.get(TRAFFIC_CLASS_BULK).length) {
      if (this.outerCurrent === 0) {
        if (!this.hasMedia()) { this.outerDeficit[0] = 0; this.advanceOuter(); continue; }
        const innerState = [this.innerCurrent, [...this.innerDeficit], this.innerStarted];
        const request = this.peekMedia();
        if (request.payload.length <= this.outerDeficit[0]) {
          this.outerDeficit[0] -= request.payload.length;
          return this.pop(request.className);
        }
        [this.innerCurrent, this.innerDeficit, this.innerStarted] = innerState;
        this.advanceOuter();
        continue;
      }
      const bulk = this.queues.get(TRAFFIC_CLASS_BULK);
      if (!bulk.length) { this.outerDeficit[1] = 0; this.advanceOuter(); continue; }
      if (bulk[0].payload.length <= this.outerDeficit[1]) {
        this.outerDeficit[1] -= bulk[0].payload.length;
        return this.pop(TRAFFIC_CLASS_BULK);
      }
      this.advanceOuter();
    }
    return null;
  }

  addOuterRound() {
    this.outerDeficit[0] = this.hasMedia() ? this.outerDeficit[0] + 3 * BASE_QUANTUM_BYTES : 0;
    this.outerDeficit[1] = this.queues.get(TRAFFIC_CLASS_BULK).length ? this.outerDeficit[1] + BASE_QUANTUM_BYTES : 0;
  }

  advanceOuter() {
    this.outerCurrent = (this.outerCurrent + 1) % 2;
    if (this.outerCurrent === 0) this.addOuterRound();
  }

  peekMedia() {
    if (!this.innerStarted) { this.addInnerRound(); this.innerStarted = true; }
    while (this.hasMedia()) {
      const className = this.innerCurrent === 0 ? TRAFFIC_CLASS_INTERACTIVE_MEDIA : TRAFFIC_CLASS_THUMBNAIL;
      const queue = this.queues.get(className);
      if (!queue.length) { this.innerDeficit[this.innerCurrent] = 0; this.advanceInner(); continue; }
      if (queue[0].payload.length <= this.innerDeficit[this.innerCurrent]) {
        this.innerDeficit[this.innerCurrent] -= queue[0].payload.length;
        return queue[0];
      }
      this.advanceInner();
    }
    return null;
  }

  addInnerRound() {
    this.innerDeficit[0] = this.queues.get(TRAFFIC_CLASS_INTERACTIVE_MEDIA).length ? this.innerDeficit[0] + 3 * BASE_QUANTUM_BYTES : 0;
    this.innerDeficit[1] = this.queues.get(TRAFFIC_CLASS_THUMBNAIL).length ? this.innerDeficit[1] + BASE_QUANTUM_BYTES : 0;
  }

  advanceInner() {
    this.innerCurrent = (this.innerCurrent + 1) % 2;
    if (this.innerCurrent === 0) this.addInnerRound();
  }

  hasMedia() {
    return this.queues.get(TRAFFIC_CLASS_INTERACTIVE_MEDIA).length > 0 || this.queues.get(TRAFFIC_CLASS_THUMBNAIL).length > 0;
  }

  pop(className) {
    const queue = this.queues.get(className);
    const request = queue.shift();
    if (queue.length === 0) this.resetEmptyLane(className);
    return request;
  }

  resetEmptyLane(className) {
    if (!this.hasClassDemand(className)) {
      if (className === TRAFFIC_CLASS_INTERACTIVE_MEDIA) this.innerDeficit[0] = 0;
      if (className === TRAFFIC_CLASS_THUMBNAIL) this.innerDeficit[1] = 0;
      if (className === TRAFFIC_CLASS_BULK) this.outerDeficit[1] = 0;
    }
    if (!this.hasMediaDemand()) {
      this.outerDeficit[0] = 0;
      this.innerCurrent = 0; this.innerDeficit = [0, 0]; this.innerStarted = false;
    }
    if (!this.hasMediaDemand() && !this.hasClassDemand(TRAFFIC_CLASS_BULK)) {
      this.outerCurrent = 0; this.outerDeficit = [0, 0]; this.outerStarted = false;
    }
  }

  hasClassDemand(className) {
    return this.queues.get(className).length > 0 || this.waiting.get(className).length > 0;
  }

  hasMediaDemand() {
    return this.hasClassDemand(TRAFFIC_CLASS_INTERACTIVE_MEDIA) || this.hasClassDemand(TRAFFIC_CLASS_THUMBNAIL);
  }

  failQueued(error) {
    for (const className of classes) {
      const queue = this.queues.get(className);
      while (queue.length) {
        const request = queue.shift();
        this.bytes.set(className, this.bytes.get(className) - request.payload.length);
        request.state = 'done'; this.detachAbort(request); request.completion.reject(error);
      }
    }
  }

  failWaiting(error) {
    for (const className of classes) {
      const waiting = this.waiting.get(className);
      while (waiting.length) {
        const request = waiting.shift();
        request.state = 'done'; this.detachAbort(request); request.admission.reject(error);
      }
    }
  }
}
