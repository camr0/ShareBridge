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