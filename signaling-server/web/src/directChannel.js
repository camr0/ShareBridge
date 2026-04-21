export class DirectChannel {
  constructor(rtcDataChannel) {
    this._dc = rtcDataChannel
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

  close() {
    this._dc.close()
  }
}

// extend the adapter with a helper used by app.js and tests
export function waitForDirectChannelOpen(channel) {
  if (channel.readyState === 'open') return Promise.resolve(channel)
  return new Promise((resolve, reject) => {
    channel.onopen = () => resolve(channel)
    channel.onclose = () => reject(new Error('direct channel closed before opening'))
  })
}