import { describe, expect, it } from 'vitest';
import { diff, toCodePoints, toUTF16, transformPosition } from './diff';

describe('diff', () => {
  it.each([
    { name: 'typing', before: 'helo', after: 'hello', want: { pos: 3, deleted: 0, inserted: ['l'] } },
    { name: 'backspace', before: 'hello', after: 'helo', want: { pos: 3, deleted: 1, inserted: [] } },
    { name: 'paste over selection', before: 'a big cat', after: 'a small cat', want: { pos: 2, deleted: 3, inserted: Array.from('small') } },
    { name: 'no change', before: 'same', after: 'same', want: { pos: 4, deleted: 0, inserted: [] } },
    { name: 'clear all', before: 'abc', after: '', want: { pos: 0, deleted: 3, inserted: [] } },
    { name: 'emoji counts as one', before: 'a👋b', after: 'a👋🎉b', want: { pos: 2, deleted: 0, inserted: ['🎉'] } },
    { name: 'delete emoji', before: 'a👋b', after: 'ab', want: { pos: 1, deleted: 1, inserted: [] } },
  ])('$name', ({ before, after, want }) => {
    expect(diff(before, after)).toEqual(want);
  });

  it('reconstructs the new text', () => {
    const before = Array.from('кот спит');
    const after = 'кит не спит';
    const c = diff(before.join(''), after);
    before.splice(c.pos, c.deleted, ...c.inserted);
    expect(before.join('')).toBe(after);
  });
});

describe('transformPosition', () => {
  const change = { pos: 2, deleted: 2, inserted: ['x', 'y', 'z'] }; // ab[cd]ef -> ab[xyz]ef

  it.each([
    [0, 0],
    [2, 2],
    [3, 5],
    [4, 5],
    [6, 7],
  ])('%i -> %i', (pos, want) => {
    expect(transformPosition(pos, change)).toBe(want);
  });
});

describe('UTF-16 offsets', () => {
  const text = 'a👋b';
  it('converts both ways', () => {
    expect(toCodePoints(text, 3)).toBe(2); // after the emoji
    expect(toUTF16(text, 2)).toBe(3);
    expect(toUTF16(text, 3)).toBe(4);
    expect(toUTF16(text, 0)).toBe(0);
  });
});
