import test from 'node:test'
import assert from 'node:assert/strict'
import { detectHopAddress, parseRouteHops } from './hops.js'

test('route parsing excludes destination header and preserves numbered order', () => {
  assert.deepEqual(parseRouteHops([
    'traceroute to example.com (93.184.215.14), 30 hops max, 60 byte packets',
    ' 1  1.1.1.1  2 ms',
    ' 2  * * *',
    ' 3  8.8.8.8  3 ms',
    ' 4  93.184.215.14  4 ms',
  ]), [{ hop: 1, ip: '1.1.1.1' }, { hop: 3, ip: '8.8.8.8' }, { hop: 4, ip: '93.184.215.14' }])
})

test('mtr, nexttrace and IPv6 retain literal offsets for terminal links', () => {
  for (const [text, ip] of [
    ['  1.|-- one.example (1.1.1.1) 0.0% 5', '1.1.1.1'],
    ['2 | 8.8.8.8 3 ms', '8.8.8.8'],
    [' 3  2606:4700:4700::1111 2 ms', '2606:4700:4700::1111'],
    [' 4  dns.example (2001:4860:4860::8888) 3 ms', '2001:4860:4860::8888'],
  ]) {
    const found = detectHopAddress(text)
    assert.equal(found?.ip, ip)
    assert.equal(text.slice(found.start, found.end), ip)
  }
})

test('timeouts, private addresses and non-hop output are not geo lookup targets', () => {
  for (const text of [' 1  * * *', ' 1  192.168.1.1 2 ms', ' 1  fe80::1 2 ms', '64 bytes from 1.1.1.1: time=2ms', 'PING example (1.1.1.1)']) {
    assert.equal(detectHopAddress(text), null)
  }
})

test('repeated addresses retain their separate positions in looping routes', () => {
  assert.deepEqual(parseRouteHops([' 1 1.1.1.1', ' 2 8.8.8.8', ' 3 1.1.1.1']), [
    { hop: 1, ip: '1.1.1.1' }, { hop: 2, ip: '8.8.8.8' }, { hop: 3, ip: '1.1.1.1' },
  ])
})
