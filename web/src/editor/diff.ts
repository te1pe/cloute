// Turns textarea changes into edits. All positions here are in Unicode code
// points (what the CRDT counts), not UTF-16 code units (what the DOM counts).

export interface Change {
  /** Code point index where the change starts. */
  pos: number;
  /** Number of code points removed at pos. */
  deleted: number;
  /** Code points inserted at pos. */
  inserted: string[];
}

/**
 * Finds the single changed region between two texts by trimming the common
 * prefix and suffix. A textarea input event changes one contiguous range
 * (typing, deleting, pasting over a selection), so this recovers it exactly
 * up to ambiguity in repeated characters, which does not affect the result.
 */
export function diff(before: string, after: string): Change {
  const a = Array.from(before);
  const b = Array.from(after);

  let prefix = 0;
  while (prefix < a.length && prefix < b.length && a[prefix] === b[prefix]) {
    prefix++;
  }
  let suffix = 0;
  while (
    suffix < a.length - prefix &&
    suffix < b.length - prefix &&
    a[a.length - 1 - suffix] === b[b.length - 1 - suffix]
  ) {
    suffix++;
  }
  return {
    pos: prefix,
    deleted: a.length - prefix - suffix,
    inserted: b.slice(prefix, b.length - suffix),
  };
}

/** Maps a caret position across a change made by someone else. */
export function transformPosition(pos: number, change: Change): number {
  if (pos <= change.pos) {
    return pos;
  }
  if (pos >= change.pos + change.deleted) {
    return pos - change.deleted + change.inserted.length;
  }
  // The caret was inside the replaced range: put it after the new text.
  return change.pos + change.inserted.length;
}

/** Converts a UTF-16 offset in text to a code point index. */
export function toCodePoints(text: string, utf16Offset: number): number {
  return Array.from(text.slice(0, utf16Offset)).length;
}

/** Converts a code point index in text to a UTF-16 offset. */
export function toUTF16(text: string, codePoints: number): number {
  let offset = 0;
  let i = 0;
  for (const ch of text) {
    if (i === codePoints) {
      break;
    }
    offset += ch.length;
    i++;
  }
  return offset;
}
