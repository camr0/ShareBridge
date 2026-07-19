export const PROTOCOL_VERSION = 2;

export const LANE_CONTROL = 0x00;
export const LANE_MEDIA = 0x01;
export const LANE_BULK = 0x02;

export const TRAFFIC_CLASS_CONTROL = 'control';
export const TRAFFIC_CLASS_INTERACTIVE_MEDIA = 'interactive-media';
export const TRAFFIC_CLASS_THUMBNAIL = 'thumbnail';
export const TRAFFIC_CLASS_BULK = 'bulk';

function isValidLane(lane) {
  return lane === LANE_CONTROL || lane === LANE_MEDIA || lane === LANE_BULK;
}

function requireUint8Array(value, name) {
  if (!(value instanceof Uint8Array)) {
    throw new TypeError(`${name} must be a Uint8Array`);
  }
}

export function encodeLaneEnvelope(lane, payload) {
  if (!isValidLane(lane)) {
    throw new RangeError(`invalid lane: ${lane}`);
  }
  requireUint8Array(payload, 'payload');

  const encoded = new Uint8Array(payload.length + 1);
  encoded[0] = lane;
  encoded.set(payload, 1);
  return encoded;
}

export function decodeLaneEnvelope(encoded) {
  requireUint8Array(encoded, 'encoded envelope');
  if (encoded.length === 0) {
    throw new RangeError('empty lane envelope');
  }

  const lane = encoded[0];
  if (!isValidLane(lane)) {
    throw new RangeError(`invalid lane: ${lane}`);
  }

  return {
    lane,
    payload: new Uint8Array(encoded.subarray(1)),
  };
}
