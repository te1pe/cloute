import { describe, expect, it } from 'vitest';
import { Doc, HEAD, OutOfRangeError, type Op } from './doc';

function typed(d: Doc, pos: number, text: string): Op[] {
  return Array.from(text).map((ch, i) => d.insertAt(pos + i, ch));
}

describe('positions as IDs', () => {
  it('maps carets to IDs and back', () => {
    const d = new Doc(1);
    typed(d, 0, 'a👋c');
    expect(d.idBefore(0)).toEqual(HEAD);
    expect(d.positionAfter(HEAD)).toBe(0);
    for (let pos = 0; pos <= d.length; pos++) {
      expect(d.positionAfter(d.idBefore(pos))).toBe(pos);
    }
    expect(() => d.idAt(3)).toThrow(OutOfRangeError);
    expect(d.positionAfter({ lamport: 99, replica: 9 })).toBeNull();
  });

  it('keeps a remote caret in place while others edit before it', () => {
    const d = new Doc(1);
    typed(d, 0, 'hello world');
    const caret = d.idBefore(6); // before "world"
    typed(d, 0, '>> '); // someone types at the start
    d.deleteAt(3); // and deletes the "h"
    expect(d.text()).toBe('>> ello world');
    expect(d.text().slice(d.positionAfter(caret)!)).toBe('world');
  });

  it('keeps a caret after a deleted character where the character was', () => {
    const d = new Doc(1);
    typed(d, 0, 'abc');
    const caret = d.idBefore(2); // after "b"
    d.deleteAt(1); // "b" is gone
    expect(d.positionAfter(caret)).toBe(1); // between "a" and "c"
  });
});

describe('comment ranges', () => {
  function setup() {
    const d = new Doc(1);
    typed(d, 0, 'Введение. Выводы.');
    const from = d.idAt(10);
    const to = d.idAt(15);
    const covered = () => {
      const r = d.rangeOf(from, to);
      return r === null ? null : Array.from(d.text()).slice(r.start, r.end).join('');
    };
    return { d, from, to, covered };
  }

  it('covers its characters', () => {
    expect(setup().covered()).toBe('Выводы');
  });

  it('does not move when text is inserted or deleted around it', () => {
    const { d, covered } = setup();
    typed(d, 0, 'Аннотация. ');
    typed(d, d.length, ' Конец.');
    d.deleteAt(0);
    expect(covered()).toBe('Выводы');
  });

  it('grows with insertions inside and shrinks with deletions inside', () => {
    const { d, covered } = setup();
    typed(d, 13, 'XY'); // "Выв|оды"
    expect(covered()).toBe('ВывXYоды');
    d.deleteAt(10); // its first character
    expect(covered()).toBe('ывXYоды');
  });

  it('collapses where its text was when all of it is deleted', () => {
    const { d, from, to } = setup();
    for (let i = 0; i < 6; i++) {
      d.deleteAt(10);
    }
    expect(d.text()).toBe('Введение. .');
    expect(d.rangeOf(from, to)).toEqual({ start: 10, end: 10 });
  });

  it('rejects unknown or reversed ends', () => {
    const { d } = setup();
    expect(d.rangeOf({ lamport: 99, replica: 9 }, d.idAt(0))).toBeNull();
    expect(d.rangeOf(d.idAt(5), d.idAt(1))).toBeNull();
    expect(d.rangeOf(HEAD, d.idAt(1))).toBeNull();
  });
});
