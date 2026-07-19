import { createChannelSet } from './channelSet.js'
import {
  LANE_BULK,
  LANE_CONTROL,
  LANE_MEDIA,
  TRAFFIC_CLASS_BULK,
  TRAFFIC_CLASS_CONTROL,
  TRAFFIC_CLASS_INTERACTIVE_MEDIA,
  TRAFFIC_CLASS_THUMBNAIL,
} from './multiLaneProtocol.js'

const LANE_BY_LABEL = Object.freeze({
  control: LANE_CONTROL,
  media: LANE_MEDIA,
  bulk: LANE_BULK,
})

export class DirectChannel {
  constructor(rtcDataChannel, lane = LANE_BY_LABEL[rtcDataChannel?.label]) {
    this._dc = rtcDataChannel
    this.lane = lane
    this.onopen = null
    this.onmessage = null
    this.onclose = null

    this._dc.binaryType = 'arraybuffer'
    this._dc.onopen = () => { if (this.onopen) this.onopen() }
    this._dc.onmessage = (event) => { if (this.onmessage) this.onmessage(event) }
    this._dc.onclose = () => { if (this.onclose) this.onclose() }
  }

  get readyState() {
    return this._dc.readyState
  }

  get bufferedAmount() {
    return this._dc.bufferedAmount
  }

  send(text) {
    this._dc.send(text)
  }

  sendBinary(bytes) {
    this._dc.send(bytes)
  }

  sendBinaryClass(className, bytes) {
    if (laneForClass(className) !== this.lane) {
      throw new RangeError(`traffic class ${className} does not map to direct lane ${this.lane}`)
    }
    this.sendBinary(bytes)
  }

  close() {
    this._dc.close()
  }
}

export function createDirectChannelSet(options) {
  return new DirectChannelSet(options)
}

class DirectChannelSet {
  constructor(options) {
    this._set = createChannelSet(options)
    this.ready = this._set.ready.then(() => this)
  }

  accept(rtcDataChannel) {
    const label = rtcDataChannel?.label
    const channel = new DirectChannel(rtcDataChannel, LANE_BY_LABEL[label])
    this._set.attach(label, channel)
    return channel
  }

  get control() { return this._set.control }
  get media() { return this._set.media }
  get bulk() { return this._set.bulk }
  get readyState() { return this._set.readyState }
  get bufferedAmount() {
    return [this.control, this.media, this.bulk]
      .reduce((total, endpoint) => total + (endpoint?.bufferedAmount ?? 0), 0)
  }

  get onopen() { return this._onopen ?? null }
  set onopen(handler) { this._onopen = handler }
  get onmessage() { return this.control?.onmessage ?? null }
  set onmessage(handler) {
    if (this.control) this.control.onmessage = handler
  }
  get onclose() { return this._set.onclose }
  set onclose(handler) { this._set.onclose = handler }

  send(value) { return this.control.send(value) }
  sendBinary(bytes) { return this.control.sendBinary(bytes) }
  close() { return this._set.close() }
}

// extend the adapter with a helper used by app.js and tests
export function waitForDirectChannelOpen(channel) {
  if (channel?.ready) return channel.ready
  if (channel.readyState === 'open') return Promise.resolve(channel)
  return new Promise((resolve, reject) => {
    channel.onopen = () => resolve(channel)
    channel.onclose = () => reject(new Error('direct channel closed before opening'))
  })
}

function laneForClass(className) {
  switch (className) {
    case TRAFFIC_CLASS_CONTROL: return LANE_CONTROL
    case TRAFFIC_CLASS_INTERACTIVE_MEDIA:
    case TRAFFIC_CLASS_THUMBNAIL: return LANE_MEDIA
    case TRAFFIC_CLASS_BULK: return LANE_BULK
    default: throw new RangeError(`unknown traffic class: ${className}`)
  }
}
