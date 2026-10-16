// A <textarea> cannot show other people's carets or highlight ranges, so a
// backdrop with the same font, padding and wrapping lies under the
// transparent textarea and draws them. Its own text is transparent: only
// the backgrounds and carets show through.

export interface Highlight {
  /** Code point positions, [start, end). */
  start: number;
  end: number;
  className: string;
  /** Background colour; the class decides if absent. */
  color?: string;
}

export interface Caret {
  /** Code point position. */
  pos: number;
  color: string;
  label: string;
}

/** A stable colour per replica, so that a person keeps theirs. */
export function colorFor(replica: number, alpha = 1): string {
  return `hsl(${(replica * 137) % 360} 70% 42% / ${alpha})`;
}

/**
 * Draws text with highlights (later ones win where they overlap) and carets
 * into the backdrop. Text goes in as text nodes only, never as HTML.
 */
export function paintBackdrop(backdrop: HTMLElement, text: string, highlights: Highlight[], carets: Caret[]): void {
  const chars = Array.from(text);
  const style: (Highlight | undefined)[] = new Array(chars.length);
  for (const h of highlights) {
    for (let i = Math.max(0, h.start); i < Math.min(chars.length, h.end); i++) {
      style[i] = h;
    }
  }
  const caretsAt = new Map<number, Caret[]>();
  for (const c of carets) {
    const pos = Math.min(Math.max(0, c.pos), chars.length);
    caretsAt.set(pos, [...(caretsAt.get(pos) ?? []), c]);
  }

  const nodes: Node[] = [];
  for (let i = 0; i <= chars.length; ) {
    for (const c of caretsAt.get(i) ?? []) {
      nodes.push(caretNode(c));
    }
    if (i === chars.length) {
      break;
    }
    const h = style[i];
    let j = i + 1;
    while (j < chars.length && style[j] === h && !caretsAt.has(j)) {
      j++;
    }
    const run = document.createTextNode(chars.slice(i, j).join(''));
    if (h === undefined) {
      nodes.push(run);
    } else {
      const mark = document.createElement('mark');
      mark.className = h.className;
      if (h.color !== undefined) {
        mark.style.backgroundColor = h.color;
      }
      mark.append(run);
      nodes.push(mark);
    }
    i = j;
  }
  // A trailing newline in a textarea shows an empty line; in a div it does
  // not, unless something follows it.
  nodes.push(document.createTextNode('​'));
  backdrop.replaceChildren(...nodes);
}

/** A zero-width marker: it must not shift the text after it. */
function caretNode(c: Caret): HTMLElement {
  const caret = document.createElement('span');
  caret.className = 'caret';
  caret.style.setProperty('--caret', c.color);
  const bar = document.createElement('i');
  const label = document.createElement('b');
  label.textContent = c.label;
  caret.append(bar, label);
  return caret;
}
