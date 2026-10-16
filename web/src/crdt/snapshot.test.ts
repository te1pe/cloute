import { describe, expect, it } from 'vitest';
import fixture from '../../../testdata/protocol/snapshot.json';
import { decodeSnapshot } from '../sync/protocol';
import { Doc, HEAD, InvalidOpError, type Entry, type Op } from './doc';

describe('snapshot', () => {
  // The same file is checked by internal/crdt/wire: Go encodes the state of
  // vector 09 exactly as this JSON, and the web client must restore it.
  it('restores the shared fixture', () => {
    const d = Doc.restore(7, decodeSnapshot(fixture.snapshot));
    expect(d.text()).toBe(fixture.text);
    expect(d.length).toBe(Array.from(fixture.text).length);
    // The clock is restored: the next local ID is above every ID seen (4).
    expect(d.insertAt(0, 'n').id).toEqual({ lamport: 5, replica: 7 });
  });

  it('round-trips through snapshot()', () => {
    const d = new Doc(1);
    for (const ch of Array.from('hello👋')) {
      d.insertAt(d.length, ch);
    }
    d.deleteAt(1);
    const r = Doc.restore(2, d.snapshot());
    expect(r.text()).toBe('hllo👋');
    expect(r.snapshot()).toEqual(d.snapshot());
  });

  it('rejects malformed snapshots', () => {
    const a: Entry = { id: { lamport: 1, replica: 1 }, after: HEAD, char: 'a', deleted: false };
    const bad: Entry[][] = [
      [{ ...a, id: HEAD }],
      [a, { id: { lamport: 1, replica: 2 }, after: a.id, char: 'b', deleted: false }],
      [{ ...a, char: '\ud800' }],
      [a, a],
      [{ id: { lamport: 2, replica: 1 }, after: a.id, char: 'b', deleted: false }, a],
    ];
    for (const entries of bad) {
      expect(() => Doc.restore(1, entries)).toThrow(InvalidOpError);
    }
  });

  it('gives a replica that behaves like the original', () => {
    let s = 42;
    const rand = (n: number) => {
      s = (Math.imul(s, 1103515245) + 12345) >>> 0; // exact 32-bit LCG
      return (s >>> 16) % n;
    };
    const alphabet = Array.from('аб👋x'); // code points, not UTF-16 units
    const editors = [new Doc(1), new Doc(2)];
    const edit = (log: Op[]) => {
      const e = editors[rand(2)];
      const op = e.length > 0 && rand(3) === 0 ? e.deleteAt(rand(e.length)) : e.insertAt(rand(e.length + 1), alphabet[rand(alphabet.length)]);
      log.push(op);
      editors.filter((o) => o !== e).forEach((o) => o.apply(op));
    };
    const history: Op[] = [];
    const future: Op[] = [];
    for (let i = 0; i < 200; i++) edit(history);
    for (let i = 0; i < 200; i++) edit(future);

    const original = new Doc(3);
    history.forEach((op) => original.apply(op));
    const restored = Doc.restore(3, original.snapshot());
    future.forEach((op) => original.apply(op));
    [...future].reverse().forEach((op) => restored.apply(op));

    expect(restored.pending).toBe(0);
    expect(restored.snapshot()).toEqual(original.snapshot());
  });
});
