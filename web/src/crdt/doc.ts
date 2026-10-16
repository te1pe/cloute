// RGA (Replicated Growable Array) sequence CRDT: a port of internal/crdt.
// Both implementations must give identical results on testdata/crdt.

/** Globally unique character ID. The zero ID is the virtual head. */
export interface ID {
  readonly lamport: number;
  readonly replica: number;
}

export interface InsertOp {
  readonly type: 'insert';
  readonly id: ID;
  readonly after: ID;
  /** Exactly one Unicode code point (may be two UTF-16 code units). */
  readonly char: string;
}

export interface DeleteOp {
  readonly type: 'delete';
  readonly id: ID;
}

export type Op = InsertOp | DeleteOp;

export const HEAD: ID = Object.freeze({ lamport: 0, replica: 0 });

/** Reports whether a precedes b: lower lamport first, then lower replica. */
export function idLess(a: ID, b: ID): boolean {
  if (a.lamport !== b.lamport) {
    return a.lamport < b.lamport;
  }
  return a.replica < b.replica;
}

function key(id: ID): string {
  return `${id.lamport}:${id.replica}`;
}

/** Operation that no correct replica can produce. */
export class InvalidOpError extends Error {
  override name = 'InvalidOpError';
}

/** Local edit at a position outside the text. */
export class OutOfRangeError extends Error {
  override name = 'OutOfRangeError';
}

/**
 * Reports whether s is exactly one Unicode scalar value: one code point that
 * is not a lone surrogate. The server would turn a lone surrogate into
 * U+FFFD, so sending one would make the replicas diverge.
 */
export function isValidChar(s: string): boolean {
  const cp = s.codePointAt(0);
  if (cp === undefined || String.fromCodePoint(cp) !== s) {
    return false;
  }
  return cp < 0xd800 || cp > 0xdfff;
}

/** One character of a snapshot, visible or tombstoned. */
export interface Entry {
  readonly id: ID;
  readonly after: ID;
  readonly char: string;
  readonly deleted: boolean;
}

interface Node {
  readonly id: ID;
  readonly after: ID;
  readonly char: string;
  deleted: boolean;
  next: Node | null;
}

/**
 * One replica of a text document. Characters, including tombstones, are
 * kept in a singly linked list in document order plus an index by ID.
 */
export class Doc {
  readonly replica: number;
  private clock = 0;
  private readonly head: Node = { id: HEAD, after: HEAD, char: '', deleted: true, next: null };
  private readonly nodes = new Map<string, Node>([[key(HEAD), this.head]]);
  private visible = 0;
  private readonly waiting = new Map<string, Op[]>();
  private waitingCount = 0;

  constructor(replica: number) {
    this.replica = replica;
  }

  /**
   * Builds a replica from a snapshot (all characters in document order).
   * Mirrors Restore in internal/crdt/snapshot.go: the list is linked as
   * given, after checking that each entry refers to an earlier character.
   */
  static restore(replica: number, entries: readonly Entry[]): Doc {
    const d = new Doc(replica);
    let prev = d.head;
    entries.forEach((e, i) => {
      if (e.id.lamport === 0) {
        throw new InvalidOpError(`snapshot entry ${i} has zero lamport timestamp`);
      }
      if (e.id.lamport <= e.after.lamport) {
        throw new InvalidOpError(`snapshot entry ${i}: ${key(e.id)} must have a greater lamport than ${key(e.after)}`);
      }
      if (!isValidChar(e.char)) {
        throw new InvalidOpError(`snapshot entry ${i}: invalid char ${JSON.stringify(e.char)}`);
      }
      if (d.nodes.has(key(e.id))) {
        throw new InvalidOpError(`snapshot entry ${i}: duplicate ${key(e.id)}`);
      }
      if (!d.nodes.has(key(e.after))) {
        throw new InvalidOpError(`snapshot entry ${i}: ${key(e.id)} refers to ${key(e.after)} that is not before it`);
      }
      const n: Node = { id: e.id, after: e.after, char: e.char, deleted: e.deleted, next: null };
      prev.next = n;
      prev = n;
      d.nodes.set(key(e.id), n);
      if (!e.deleted) {
        d.visible++;
      }
      d.observe(e.id);
    });
    return d;
  }

  /** All applied characters, including tombstones, in document order. */
  snapshot(): Entry[] {
    const out: Entry[] = [];
    for (let n = this.head.next; n !== null; n = n.next) {
      out.push({ id: n.id, after: n.after, char: n.char, deleted: n.deleted });
    }
    return out;
  }

  /** Number of visible characters (code points). */
  get length(): number {
    return this.visible;
  }

  /** Number of buffered operations waiting for a dependency. */
  get pending(): number {
    return this.waitingCount;
  }

  text(): string {
    let s = '';
    for (let n = this.head.next; n !== null; n = n.next) {
      if (!n.deleted) {
        s += n.char;
      }
    }
    return s;
  }

  /** Reports whether the character id has been applied (visible or deleted); the head always has. */
  contains(id: ID): boolean {
    return this.nodes.has(key(id));
  }

  /** The ID of the visible character at pos. */
  idAt(pos: number): ID {
    if (!Number.isInteger(pos) || pos < 0 || pos >= this.visible) {
      throw new OutOfRangeError(`no character at ${pos}, length ${this.visible}`);
    }
    return this.visibleAt(pos).id;
  }

  /**
   * The ID a caret at pos sits after: the character before it, or HEAD at
   * the start. Positions are kept as IDs so they survive others' edits.
   */
  idBefore(pos: number): ID {
    return pos === 0 ? HEAD : this.idAt(pos - 1);
  }

  /**
   * The position right after the character id, visible or deleted: the
   * number of visible characters up to and including it. HEAD gives 0;
   * an unknown ID gives null.
   */
  positionAfter(id: ID): number | null {
    const target = this.nodes.get(key(id));
    if (target === undefined) {
      return null;
    }
    let count = 0;
    for (let n: Node | null = this.head; n !== null; n = n.next) {
      if (n !== this.head && !n.deleted) {
        count++;
      }
      if (n === target) {
        return count;
      }
    }
    return null;
  }

  /**
   * The visible range [start, end) covered by the characters from..to in
   * document order (a comment). Insertions inside widen it, insertions
   * around it do not move it; start === end when all of it was deleted.
   * null if an end is unknown or the ends are out of order.
   */
  rangeOf(from: ID, to: ID): { start: number; end: number } | null {
    const end = this.positionAfter(to);
    const fromNode = this.nodes.get(key(from));
    if (end === null || fromNode === undefined || fromNode === this.head) {
      return null;
    }
    const start = (this.positionAfter(from) ?? 0) - (fromNode.deleted ? 0 : 1);
    return start <= end ? { start, end } : null;
  }

  /** Inserts char before the visible character at pos (pos === length appends). */
  insertAt(pos: number, char: string): InsertOp {
    if (!Number.isInteger(pos) || pos < 0 || pos > this.visible) {
      throw new OutOfRangeError(`insert at ${pos}, length ${this.visible}`);
    }
    if (!isValidChar(char)) {
      throw new InvalidOpError(`char ${JSON.stringify(char)} is not one Unicode scalar value`);
    }
    const after = pos === 0 ? this.head : this.visibleAt(pos - 1);
    this.clock++;
    const op: InsertOp = { type: 'insert', id: { lamport: this.clock, replica: this.replica }, after: after.id, char };
    this.integrate(op);
    this.release(op.id);
    return op;
  }

  /** Deletes the visible character at pos. */
  deleteAt(pos: number): DeleteOp {
    if (!Number.isInteger(pos) || pos < 0 || pos >= this.visible) {
      throw new OutOfRangeError(`delete at ${pos}, length ${this.visible}`);
    }
    const n = this.visibleAt(pos);
    this.tombstone(n);
    return { type: 'delete', id: n.id };
  }

  /**
   * Applies an operation from another replica. Operations may arrive in any
   * order and more than once; those with an unknown dependency are buffered.
   */
  apply(op: Op): void {
    if (op.type === 'insert') {
      this.applyInsert(op);
    } else {
      this.applyDelete(op);
    }
  }

  private applyInsert(op: InsertOp): void {
    if (op.id.lamport === 0) {
      throw new InvalidOpError('insert with zero lamport timestamp');
    }
    if (op.id.lamport <= op.after.lamport) {
      throw new InvalidOpError(`insert ${key(op.id)} must have a greater lamport than ${key(op.after)}`);
    }
    if (!isValidChar(op.char)) {
      throw new InvalidOpError(`char ${JSON.stringify(op.char)} is not one Unicode scalar value`);
    }
    const existing = this.nodes.get(key(op.id));
    if (existing !== undefined) {
      if (key(existing.after) !== key(op.after) || existing.char !== op.char) {
        throw new InvalidOpError(`insert ${key(op.id)} conflicts with an existing character`);
      }
      return;
    }

    this.observe(op.id);
    if (!this.nodes.has(key(op.after))) {
      this.buffer(op.after, op);
      return;
    }
    this.integrate(op);
    this.release(op.id);
  }

  private applyDelete(op: DeleteOp): void {
    if (op.id.lamport === 0) {
      throw new InvalidOpError('delete with zero lamport timestamp');
    }
    this.observe(op.id);
    const n = this.nodes.get(key(op.id));
    if (n === undefined) {
      this.buffer(op.id, op);
      return;
    }
    this.tombstone(n);
  }

  private observe(id: ID): void {
    this.clock = Math.max(this.clock, id.lamport);
  }

  /** See integrate in internal/crdt/doc.go for why skipping greater IDs works. */
  private integrate(op: InsertOp): void {
    let prev = this.nodes.get(key(op.after))!;
    while (prev.next !== null && idLess(op.id, prev.next.id)) {
      prev = prev.next;
    }
    const n: Node = { id: op.id, after: op.after, char: op.char, deleted: false, next: prev.next };
    prev.next = n;
    this.nodes.set(key(op.id), n);
    this.visible++;
  }

  private tombstone(n: Node): void {
    if (n.deleted) {
      return;
    }
    n.deleted = true;
    this.visible--;
  }

  private buffer(missing: ID, op: Op): void {
    const k = key(missing);
    const list = this.waiting.get(k) ?? [];
    if (list.some((o) => sameOp(o, op))) {
      return;
    }
    list.push(op);
    this.waiting.set(k, list);
    this.waitingCount++;
  }

  private release(id: ID): void {
    const ready: ID[] = [id];
    for (let next = ready.pop(); next !== undefined; next = ready.pop()) {
      const k = key(next);
      const list = this.waiting.get(k);
      if (list === undefined) {
        continue;
      }
      this.waiting.delete(k);
      this.waitingCount -= list.length;

      for (const op of list) {
        if (op.type === 'insert') {
          if (this.nodes.has(key(op.id))) {
            continue;
          }
          this.integrate(op);
          ready.push(op.id);
        } else {
          this.tombstone(this.nodes.get(key(op.id))!);
        }
      }
    }
  }

  private visibleAt(pos: number): Node {
    for (let n = this.head.next; n !== null; n = n.next) {
      if (n.deleted) {
        continue;
      }
      if (pos === 0) {
        return n;
      }
      pos--;
    }
    throw new Error('crdt: visibleAt: position out of range');
  }
}

function sameOp(a: Op, b: Op): boolean {
  if (a.type !== b.type || key(a.id) !== key(b.id)) {
    return false;
  }
  if (a.type === 'insert' && b.type === 'insert') {
    return key(a.after) === key(b.after) && a.char === b.char;
  }
  return true;
}
