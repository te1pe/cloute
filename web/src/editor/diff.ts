
export interface Change {
  pos: number;
  deleted: number;
  inserted: string[];
}

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

export function transformPosition(pos: number, change: Change): number {
  if (pos <= change.pos) {
    return pos;
  }
  if (pos >= change.pos + change.deleted) {
    return pos - change.deleted + change.inserted.length;
  }
  return change.pos + change.inserted.length;
}

export function toCodePoints(text: string, utf16Offset: number): number {
  return Array.from(text.slice(0, utf16Offset)).length;
}

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
