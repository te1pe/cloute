import { describe, expect, it } from 'vitest';
import type { Op } from '../crdt/doc';
import { ProtocolError, decodeOp, encodeOp, parseServerMessage } from './protocol';

describe('op codec', () => {
  it('round-trips inserts and deletes', () => {
    const ops: Op[] = [
      { type: 'insert', id: { lamport: 5, replica: 2 }, after: { lamport: 0, replica: 0 }, char: '👋' },
      { type: 'delete', id: { lamport: 5, replica: 2 } },
    ];
    for (const op of ops) {
      const wire = JSON.parse(JSON.stringify(encodeOp(op)));
      expect(decodeOp(wire)).toEqual(op);
    }
    expect(encodeOp(ops[0])).toEqual({ type: 'insert', id: [5, 2], after: [0, 0], char: '👋' });
  });

  it('rejects malformed ops', () => {
    const bad = [
      null,
      { type: 'move', id: [1, 1] },
      { type: 'delete', id: [1] },
      { type: 'delete', id: [-1, 1] },
      { type: 'delete', id: [1.5, 1] },
      { type: 'delete', id: [2 ** 53, 1] },
      { type: 'delete', id: [1, 2 ** 32] },
      { type: 'insert', id: [1, 1], after: [0, 0], char: 'ab' },
      { type: 'insert', id: [1, 1], after: [0, 0], char: '\ud800' },
      { type: 'insert', id: [1, 1], char: 'a' },
    ];
    for (const w of bad) {
      expect(() => decodeOp(w), JSON.stringify(w)).toThrow(ProtocolError);
    }
  });
});

describe('parseServerMessage', () => {
  it('accepts known messages of version 1', () => {
    expect(parseServerMessage('{"v":1,"type":"ack","seq":3}')).toEqual({ v: 1, type: 'ack', seq: 3 });
  });

  it('rejects other versions, types and garbage', () => {
    expect(() => parseServerMessage('{"v":2,"type":"ack","seq":3}')).toThrow(ProtocolError);
    expect(() => parseServerMessage('{"v":1,"type":"surprise"}')).toThrow(ProtocolError);
    expect(() => parseServerMessage('not json')).toThrow(ProtocolError);
  });
});
