import test from 'node:test'
import assert from 'node:assert/strict'
import { commandSucceeded } from './commandResult.js'

test('command completion honors explicit failures and legacy error frames', () => {
  assert.equal(commandSucceeded('{"exit_ok":false}'), false)
  assert.equal(commandSucceeded({ exit_ok: false }), false)
  assert.equal(commandSucceeded('{"exit_ok":true}'), true)
  assert.equal(commandSucceeded(''), true)
  assert.equal(commandSucceeded(undefined), true)
  assert.equal(commandSucceeded('', true), false)
  assert.equal(commandSucceeded('{"exit_ok":true}', true), false)
})
