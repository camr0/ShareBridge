import { test } from 'node:test'
import assert from 'node:assert/strict'
import { DirectChannel, waitForDirectChannelOpen } from './directChannel.js'

function createMockRTCDataChannel() {
  return {
    bufferedAmount: 17,
    readyState: 'connecting',
    binaryType: 'blob',
    sent: [],
    onopen: null,
    onmessage: null,
    onclose: null,
    send(payload) {
      this.sent.push(payload)
    },
    closeCalled: false,
    close() {
      this.closeCalled = true
      this.readyState = 'closed'
    },
  }
}

test('DirectChannel normalizes RTCDataChannel into the shared channel contract', () => {
  const rtc = createMockRTCDataChannel()
  const channel = new DirectChannel(rtc)
  const seen = []

  channel.onmessage = (event) => seen.push(event.data)
  channel.onopen = () => seen.push('open')
  channel.onclose = () => seen.push('close')

  rtc.readyState = 'open'
  rtc.onopen()
  rtc.onmessage({ data: 'hi' })
  rtc.onmessage({ data: new ArrayBuffer(3) })
  rtc.onclose()

  channel.send(JSON.stringify({ type: 'ping' }))
  channel.sendBinary(new Uint8Array([1, 2, 3]))
  channel.close()

  assert.deepEqual(seen, ['open', 'hi', new ArrayBuffer(3), 'close'])
  assert.equal(rtc.binaryType, 'arraybuffer')
  assert.equal(channel.bufferedAmount, 17)
  assert.equal(rtc.sent.length, 2)
  assert.equal(rtc.closeCalled, true)
})

test('waitForDirectChannelOpen resolves when the RTC data channel opens later', async () => {
  const rtc = createMockRTCDataChannel()
  const channel = new DirectChannel(rtc)

  const pending = waitForDirectChannelOpen(channel)
  rtc.readyState = 'open'
  rtc.onopen()

  const opened = await pending
  assert.equal(opened, channel)
})
