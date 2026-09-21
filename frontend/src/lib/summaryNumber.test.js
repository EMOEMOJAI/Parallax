import test from 'node:test'
import assert from 'node:assert/strict'
import { summaryNumber } from './summaryNumber.js'

test('numeric badges accept finite scalars and reject composite coercion', () => {
  for (const value of [[], [0], {}, [{ toString: null }], null, true, false, undefined, '', '  ', 'Infinity', Infinity, NaN]) {
    assert.equal(summaryNumber(value), null)
  }
  for (const value of [0, '0', 2.5, '2.5', -1, '-1']) {
    assert.equal(summaryNumber(value), Number(value))
  }
})
