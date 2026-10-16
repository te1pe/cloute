import { describe, expect, it } from 'vitest';
import { decodeOp } from '../sync/protocol';
import { Doc, HEAD, InvalidOpError, OutOfRangeError, idLess, type InsertOp, type Op } from './doc';

// Shared vectors, also run by internal/crdt; format in testdata/crdt/README.md.
interface Vector {
  name: string;
  ops: unknown[];
  expected: string;
}

const vectorFiles = import.meta.glob<Vector>('../../../testdata/crdt/*.json', { eager: true, import: 'default' });

function permutations(n: number): number[][] {
  if (n === 0) {
    return [[]];
  }
  const out: number[][] = [];
  for (const p of permutations(n - 1)) {
    for (let i = 0; i <= p.length; i++) {
      out.push([...p.slice(0, i), n - 1, ...p.slice(i)]);
    }
  }
  return out;
}

describe('shared test vectors', () => {
  const entries = Object.entries(vectorFiles);

  it('are found', () => {
    expect(entries.length).toBeGreaterThan(0);
  });

  for (const [file, v] of entries) {
    it(file.split('/').pop()!, () => {
      const ops = v.ops.map(decodeOp);
      expect(ops.length).toBeLessThanOrEqual(7); // 7! orders at most
      for (const order of permutations(ops.length)) {
        const d = new Doc(0);
        for (const i of order) {
          d.apply(ops[i]);
        }
        expect({ order, text: d.text(), pending: d.pending }).toEqual({ order, text: v.expected, pending: 0 });
        for (const i of order) {
          d.apply(ops[i]);
        }
        expect(d.text()).toBe(v.expected);
      }
    });
  }
});

function ins(lamport: number, replica: number, after: InsertOp['after'], char: string): InsertOp {
  return { type: 'insert', id: { lamport, replica }, after, char };
}

describe('Doc', () => {
  it('orders IDs by lamport, then replica', () => {
    expect(idLess({ lamport: 1, replica: 9 }, { lamport: 2, replica: 1 })).toBe(true);
    expect(idLess({ lamport: 1, replica: 1 }, { lamport: 1, replica: 2 })).toBe(true);
    expect(idLess({ lamport: 1, replica: 2 }, { lamport: 1, replica: 1 })).toBe(false);
    expect(idLess({ lamport: 1, replica: 1 }, { lamport: 1, replica: 1 })).toBe(false);
  });

  it('applies local edits by code point position', () => {
    const d = new Doc(1);
    d.insertAt(0, 'h');
    d.insertAt(1, '👋');
    d.insertAt(2, 'i');
    expect(d.text()).toBe('h👋i');
    expect(d.length).toBe(3);
    const del = d.deleteAt(1);
    expect(del).toEqual({ type: 'delete', id: { lamport: 2, replica: 1 } });
    expect(d.text()).toBe('hi');
  });

  it('advances the lamport clock on remote ops', () => {
    const d = new Doc(7);
    const a = d.insertAt(0, 'a');
    d.apply(ins(10, 3, a.id, 'x'));
    expect(d.insertAt(0, 'c').id).toEqual({ lamport: 11, replica: 7 });
  });

  it('rejects out-of-range positions and invalid chars', () => {
    const d = new Doc(1);
    expect(() => d.insertAt(1, 'a')).toThrow(OutOfRangeError);
    expect(() => d.deleteAt(0)).toThrow(OutOfRangeError);
    expect(() => d.insertAt(0, 'ab')).toThrow(InvalidOpError);
    expect(() => d.insertAt(0, '\ud800')).toThrow(InvalidOpError);
    expect(() => d.insertAt(0, '')).toThrow(InvalidOpError);
  });

  it('keeps tombstones as anchors for concurrent inserts', () => {
    const d = new Doc(1);
    const a = ins(1, 1, HEAD, 'a');
    d.apply(a);
    d.apply(ins(2, 1, a.id, 'b'));
    d.apply({ type: 'delete', id: a.id });
    d.apply(ins(3, 2, a.id, 'x'));
    expect(d.pending).toBe(0);
    expect(d.text()).toBe('xb');
  });

  it('buffers ops until their dependency arrives', () => {
    const a = ins(1, 1, HEAD, 'a');
    const b = ins(2, 1, a.id, 'b');
    const d = new Doc(2);
    d.apply(b);
    d.apply(b);
    d.apply({ type: 'delete', id: b.id });
    expect(d.text()).toBe('');
    expect(d.pending).toBe(2);
    d.apply(a);
    expect(d.text()).toBe('a');
    expect(d.pending).toBe(0);
  });

  it('rejects invalid remote ops without changing state', () => {
    const a = ins(2, 1, HEAD, 'a');
    const bad: Op[] = [
      ins(0, 1, HEAD, 'x'),
      ins(2, 2, a.id, 'x'),
      ins(3, 2, a.id, '\udc00'),
      ins(2, 1, HEAD, 'z'),
      { type: 'delete', id: HEAD },
    ];
    for (const op of bad) {
      const d = new Doc(3);
      d.apply(a);
      expect(() => d.apply(op)).toThrow(InvalidOpError);
      expect(d.text()).toBe('a');
      expect(d.pending).toBe(0);
    }
  });
});

// Small deterministic PRNG (mulberry32) so that failures are reproducible.
function rng(seed: number): (n: number) => number {
  let s = seed >>> 0;
  return (n) => {
    s = (s + 0x6d2b79f5) >>> 0;
    let t = s;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return Math.floor((((t ^ (t >>> 14)) >>> 0) / 4294967296) * n);
  };
}

function shuffle<T>(a: T[], rand: (n: number) => number): T[] {
  for (let i = a.length - 1; i > 0; i--) {
    const j = rand(i + 1);
    [a[i], a[j]] = [a[j], a[i]];
  }
  return a;
}

describe('randomized convergence of three replicas', () => {
  const alphabet = Array.from('abcxyzабв👋 \n');

  for (let seed = 0; seed < 50; seed++) {
    it(`seed=${seed}`, () => {
      const rand = rng(seed);
      const docs = [new Doc(1), new Doc(2), new Doc(3)];
      const inbox: Op[][] = [[], [], []];
      const log: Op[] = [];
      const broadcast = (from: number, op: Op) => {
        log.push(op);
        inbox.forEach((box, i) => i !== from && box.push(op));
      };

      for (let step = 0; step < 300; step++) {
        const r = rand(3);
        const d = docs[r];
        const k = rand(10);
        if (k < 5) {
          broadcast(r, d.insertAt(rand(d.length + 1), alphabet[rand(alphabet.length)]));
        } else if (k < 7 && d.length > 0) {
          broadcast(r, d.deleteAt(rand(d.length)));
        } else {
          const box = shuffle(inbox[r], rand);
          const n = rand(box.length + 1);
          box.slice(0, n).forEach((op) => d.apply(op));
          inbox[r] = box.slice(n);
        }
      }

      const fresh = new Doc(4);
      for (const d of [...docs, fresh]) {
        shuffle([...log], rand).forEach((op) => d.apply(op));
      }
      const want = docs[0].text();
      for (const d of [...docs, fresh]) {
        expect(d.pending).toBe(0);
        expect(Array.from(d.text()).length).toBe(d.length);
        expect(d.text()).toBe(want);
      }
    });
  }
});
