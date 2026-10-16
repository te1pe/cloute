import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Doc, type Op } from '../crdt/doc';
import {
  MAX_OPS_PER_MESSAGE,
  RECONNECT_DELAY_MS,
  SyncClient,
  type CommentInfo,
  type RemoteCursor,
  type Socket,
  type Status,
} from './client';
import { encodeOp, type WireOp } from './protocol';

class FakeSocket implements Socket {
  readyState = 0;
  sent: Record<string, unknown>[] = [];
  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onclose: ((ev: CloseEvent) => void) | null = null;

  send(data: string): void {
    this.sent.push(JSON.parse(data));
  }
  close(code = 1000, reason = ''): void {
    if (this.readyState === 3) return;
    this.readyState = 3;
    this.onclose?.({ code, reason } as CloseEvent);
  }
  // Server side of the fake:
  accept(): void {
    this.readyState = 1;
    this.onopen?.({} as Event);
  }
  deliver(msg: object): void {
    this.onmessage?.({ data: JSON.stringify({ v: 1, ...msg }) } as MessageEvent);
  }
  drop(): void {
    this.close(1006, '');
  }
  last(): Record<string, unknown> {
    return this.sent[this.sent.length - 1];
  }
}

function setup() {
  const sockets: FakeSocket[] = [];
  const statuses: Status[] = [];
  const fatal: string[] = [];
  let doc: Doc | null = null;
  let cursors = new Map<number, RemoteCursor>();
  let comments: CommentInfo[] = [];
  const client = new SyncClient(
    'ws://test/ws',
    'report',
    {
      onReset: (d) => (doc = d),
      onRemoteChange: (d) => (doc = d),
      onStatus: (s) => statuses.push(s),
      onFatal: (code) => fatal.push(code),
      onPresence: (c) => (cursors = new Map(c)),
      onComments: (c) => (comments = [...c]),
    },
    () => {
      const s = new FakeSocket();
      sockets.push(s);
      return s;
    },
  );
  client.start();
  return {
    client,
    sockets,
    statuses,
    fatal,
    cursors: () => cursors,
    comments: () => comments,
    socket: () => sockets[sockets.length - 1],
    text: () => doc?.text(),
  };
}

/** Another client's op typed at the start of an empty document. */
function remoteInsert(lamport: number, replica: number, char: string): WireOp {
  return { type: 'insert', id: [lamport, replica], after: [0, 0], char };
}

function typeInto(client: SyncClient, text: string): Op[] {
  const doc = client.replicaDoc!;
  const ops = Array.from(text).map((ch) => doc.insertAt(doc.length, ch));
  client.submit(ops);
  return ops;
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe('SyncClient', () => {
  it('starts with a plain hello and builds the replica from the welcome', () => {
    const t = setup();
    t.socket().accept();
    expect(t.socket().sent).toEqual([{ v: 1, type: 'hello', docId: 'report' }]);

    t.socket().deliver({ type: 'welcome', role: 'editor', replica: 3, seq: 1, snapshot: { chars: [{ id: [1, 1], after: [0, 0], char: 'A' }] } });
    expect(t.text()).toBe('A');
    expect(t.statuses.at(-1)).toEqual({ state: 'online', replica: 3, role: 'editor', seq: 1, pending: 0 });
  });

  it('keeps editing offline and resends everything unacknowledged on reconnect', () => {
    const t = setup();
    t.socket().accept();
    t.socket().deliver({ type: 'welcome', role: 'editor', replica: 2, seq: 5, snapshot: { chars: [] } });

    const sent = typeInto(t.client, 'ab'); // sent, but the ack never arrives
    expect(t.socket().last()).toEqual({ v: 1, type: 'ops', ops: sent.map(encodeOp) });
    t.socket().drop();
    expect(t.statuses.at(-1)?.state).toBe('offline');

    const offline = typeInto(t.client, 'c'); // offline: queued
    expect(t.client.pending).toBe(3);
    expect(t.client.replicaDoc?.text()).toBe('abc');
    expect(t.sockets).toHaveLength(1);

    vi.advanceTimersByTime(RECONNECT_DELAY_MS);
    expect(t.sockets).toHaveLength(2);
    t.socket().accept();
    expect(t.socket().sent).toEqual([
      {
        v: 1,
        type: 'hello',
        docId: 'report',
        replica: 2,
        lastSeq: 5,
        pending: [...sent, ...offline].map(encodeOp),
      },
    ]);

    // Catch-up: someone else typed "X" meanwhile; then the ack for hello.pending.
    t.socket().deliver({ type: 'welcome', role: 'editor', replica: 2, seq: 6, ops: [remoteInsert(6, 9, 'X')] });
    t.socket().deliver({ type: 'ack', seq: 9 });
    expect(t.client.pending).toBe(0);
    expect(t.text()).toBe('Xabc');
    expect(t.statuses.at(-1)).toEqual({ state: 'online', replica: 2, role: 'editor', seq: 9, pending: 0 });
  });

  it('re-applies its pending ops on top of a snapshot welcome', () => {
    const t = setup();
    t.socket().accept();
    t.socket().deliver({ type: 'welcome', role: 'editor', replica: 2, seq: 0, snapshot: { chars: [] } });
    t.socket().drop();
    typeInto(t.client, 'me');

    vi.advanceTimersByTime(RECONNECT_DELAY_MS);
    t.socket().accept();
    // Far behind: the server sends a snapshot without our ops.
    t.socket().deliver({
      type: 'welcome',
      replica: 2,
      seq: 50,
      snapshot: { chars: [{ id: [50, 7], after: [0, 0], char: 'S' }] },
    });
    // S (50,7) and m (1,2) both follow the head: the greater ID comes first.
    expect(t.text()).toBe('Sme');
    expect(t.client.pending).toBe(2); // until the ack
  });

  it('sends only the first chunk in hello and the rest after the welcome', () => {
    const t = setup();
    t.socket().accept();
    t.socket().deliver({ type: 'welcome', role: 'editor', replica: 1, seq: 0, snapshot: { chars: [] } });
    t.socket().drop();
    typeInto(t.client, 'x'.repeat(MAX_OPS_PER_MESSAGE + 5));

    vi.advanceTimersByTime(RECONNECT_DELAY_MS);
    t.socket().accept();
    const hello = t.socket().sent[0] as { pending: unknown[] };
    expect(hello.pending).toHaveLength(MAX_OPS_PER_MESSAGE);

    t.socket().deliver({ type: 'welcome', role: 'editor', replica: 1, seq: 0, ops: [] });
    const rest = t.socket().last() as { type: string; ops: unknown[] };
    expect(rest.type).toBe('ops');
    expect(rest.ops).toHaveLength(5);

    t.socket().deliver({ type: 'ack', seq: MAX_OPS_PER_MESSAGE });
    t.socket().deliver({ type: 'ack', seq: MAX_OPS_PER_MESSAGE + 5 });
    expect(t.client.pending).toBe(0);
  });

  it('starts over from the server version after an error', () => {
    const t = setup();
    t.socket().accept();
    t.socket().deliver({ type: 'welcome', role: 'editor', replica: 4, seq: 3, snapshot: { chars: [] } });
    typeInto(t.client, 'z');
    vi.spyOn(console, 'error').mockImplementation(() => {});
    t.socket().deliver({ type: 'error', code: 'invalid_op', message: 'refused' });
    t.socket().drop();
    expect(t.client.replicaDoc).toBeNull();
    expect(t.client.pending).toBe(0);

    vi.advanceTimersByTime(RECONNECT_DELAY_MS);
    t.socket().accept();
    expect(t.socket().sent).toEqual([{ v: 1, type: 'hello', docId: 'report' }]);
  });

  it('stays offline while paused and reconnects at once on resume', () => {
    const t = setup();
    t.socket().accept();
    t.socket().deliver({ type: 'welcome', role: 'editor', replica: 1, seq: 0, snapshot: { chars: [] } });

    t.client.pause();
    expect(t.statuses.at(-1)?.state).toBe('offline');
    vi.advanceTimersByTime(10 * RECONNECT_DELAY_MS);
    expect(t.sockets).toHaveLength(1);

    t.client.resume();
    expect(t.sockets).toHaveLength(2);
  });

  it('rejects a welcome that does not resume its replica', () => {
    const t = setup();
    t.socket().accept();
    t.socket().deliver({ type: 'welcome', role: 'editor', replica: 2, seq: 0, snapshot: { chars: [] } });
    t.socket().drop();
    vi.advanceTimersByTime(RECONNECT_DELAY_MS);
    t.socket().accept();
    vi.spyOn(console, 'error').mockImplementation(() => {});
    t.socket().deliver({ type: 'welcome', role: 'editor', replica: 3, seq: 0, snapshot: { chars: [] } });
    expect(t.socket().readyState).toBe(3); // closed as a protocol error
  });

  it('stops for good when the server refuses the session or the document', () => {
    for (const code of ['unauthorized', 'forbidden']) {
      const t = setup();
      t.socket().accept();
      t.socket().deliver({ type: 'error', code, message: 'no' });
      t.socket().close(1008, code);
      vi.advanceTimersByTime(10 * RECONNECT_DELAY_MS);
      expect(t.fatal).toEqual([code]);
      expect(t.sockets).toHaveLength(1);
    }
  });

  it('tracks other cursors and comments, and resets them on reconnect', () => {
    const t = setup();
    t.socket().accept();
    t.socket().deliver({ type: 'welcome', role: 'editor', replica: 2, seq: 1, snapshot: { chars: [{ id: [1, 1], after: [0, 0], char: 'A' }] } });
    const comment = { id: 7, from: [1, 1], to: [1, 1], text: 'note', author: { id: 5, name: 'Т' }, createdAt: 'now', resolved: false };
    t.socket().deliver({ type: 'comments', comments: [comment] });
    t.socket().deliver({ type: 'presence', replica: 3, user: { id: 5, name: 'Т' }, cursor: { anchor: [1, 1], head: [0, 0] } });
    t.socket().deliver({ type: 'presence', replica: 2, cursor: null }); // our own replica: ignored

    expect(t.comments().map((c) => c.text)).toEqual(['note']);
    expect(t.cursors().get(3)).toEqual({ replica: 3, userId: 5, name: 'Т', anchor: { lamport: 1, replica: 1 }, head: { lamport: 0, replica: 0 } });

    t.socket().deliver({ type: 'comment', comment: { ...comment, resolved: true } });
    expect(t.comments()).toEqual([]);
    t.socket().deliver({ type: 'presence', replica: 3, cursor: null });
    expect(t.cursors().size).toBe(0);

    t.socket().deliver({ type: 'presence', replica: 4, user: { id: 6, name: 'Б' }, cursor: { anchor: [0, 0], head: [0, 0] } });
    t.socket().drop();
    expect(t.cursors().size).toBe(0); // unknown while offline
  });

  it('sends cursors and comments only while online', () => {
    const t = setup();
    const id = { lamport: 1, replica: 1 };
    t.client.setCursor(id, id);
    expect(t.client.addComment(id, id, 'x')).toBe(false);
    t.socket().accept();
    t.socket().deliver({ type: 'welcome', role: 'editor', replica: 2, seq: 0, snapshot: { chars: [] } });
    t.client.setCursor(id, id);
    expect(t.client.addComment(id, id, 'x')).toBe(true);
    expect(t.client.resolveComment(7)).toBe(true);
    expect(t.socket().sent.slice(1)).toEqual([
      { v: 1, type: 'presence', cursor: { anchor: [1, 1], head: [1, 1] } },
      { v: 1, type: 'comment', from: [1, 1], to: [1, 1], text: 'x' },
      { v: 1, type: 'resolve', id: 7 },
    ]);
  });
});
