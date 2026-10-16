// Keeps a local CRDT replica in sync with the server over WebSocket, and
// keeps it editable while offline: unacknowledged operations are queued and
// resent after reconnecting.

import { Doc, type ID, type Op } from '../crdt/doc';
import {
  PROTOCOL_VERSION,
  decodeID,
  decodeOp,
  decodeSnapshot,
  encodeID,
  encodeOp,
  parseServerMessage,
  type ClientMessage,
  type WireComment,
} from './protocol';

/** Another participant's selection; anchor === head is a plain caret. */
export interface RemoteCursor {
  replica: number;
  userId: number;
  name: string;
  anchor: ID;
  head: ID;
}

export interface CommentInfo {
  id: number;
  from: ID;
  to: ID;
  text: string;
  authorId: number;
  authorName: string;
  createdAt: string;
}

/** Keeps every message well below the server's 1 MiB limit. */
export const MAX_OPS_PER_MESSAGE = 1000;
export const RECONNECT_DELAY_MS = 1000;
const OPEN = 1; // WebSocket.OPEN

export type Status =
  | { state: 'connecting'; pending: number }
  | { state: 'online'; replica: number; role: string; seq: number; pending: number }
  | { state: 'offline'; reason: string; pending: number };

/** Errors after which reconnecting cannot help: the client stops. */
const FATAL_ERRORS = new Set(['unauthorized', 'forbidden']);

export interface SyncHandlers {
  /** The replica object was (re)built; render it from scratch. */
  onReset(doc: Doc): void;
  /** Operations from other clients were applied to the current replica. */
  onRemoteChange(doc: Doc): void;
  onStatus(status: Status): void;
  /** The server refused the session or the document; the client has stopped. */
  onFatal(code: string, message: string): void;
  /** Other participants' cursors changed (cleared while offline). */
  onPresence(cursors: ReadonlyMap<number, RemoteCursor>): void;
  /** The unresolved comments changed. */
  onComments(comments: readonly CommentInfo[]): void;
}

/** The part of WebSocket the client uses; tests substitute a fake. */
export interface Socket {
  readonly readyState: number;
  onopen: ((ev: Event) => void) | null;
  onmessage: ((ev: MessageEvent) => void) | null;
  onclose: ((ev: CloseEvent) => void) | null;
  send(data: string): void;
  close(code?: number, reason?: string): void;
}

export class SyncClient {
  private socket: Socket | null = null;
  private online = false; // welcome received on the current socket
  private doc: Doc | null = null;
  private replica = 0; // 0: none yet, the server assigns one
  private role = '';
  private lastSeq = 0;
  /** Sent batches waiting for their ack, oldest first: one ack per batch. */
  private inflight: Op[][] = [];
  /** Operations created while not online. */
  private unsent: Op[] = [];
  private readonly cursors = new Map<number, RemoteCursor>();
  private readonly comments = new Map<number, CommentInfo>();
  private stopped = true;
  private paused = false;
  private retry: ReturnType<typeof setTimeout> | null = null;

  constructor(
    private readonly url: string,
    private readonly docId: string,
    private readonly handlers: SyncHandlers,
    private readonly open: (url: string) => Socket = (u) => new WebSocket(u),
  ) {}

  start(): void {
    this.stopped = false;
    this.connect();
  }

  stop(): void {
    this.stopped = true;
    this.cancelRetry();
    this.socket?.close(1000, 'client stopped');
  }

  /** The browser reports no network: go offline now instead of waiting for TCP to notice. */
  pause(): void {
    this.paused = true;
    this.cancelRetry();
    this.socket?.close(1000, 'browser offline');
  }

  resume(): void {
    this.paused = false;
    if (this.socket === null && !this.stopped) {
      this.cancelRetry();
      this.connect();
    }
  }

  /** The replica to edit, online or offline; null until the first welcome. */
  get replicaDoc(): Doc | null {
    return this.doc;
  }

  /** Number of local operations the server has not acknowledged yet. */
  get pending(): number {
    return this.inflight.reduce((n, b) => n + b.length, 0) + this.unsent.length;
  }

  /** Whether the server can be reached right now (cursors and comments need it). */
  get isOnline(): boolean {
    return this.online;
  }

  /** Shares the local selection; ignored while offline (cursors are not queued). */
  setCursor(anchor: ID, head: ID): void {
    if (this.online && this.socket !== null) {
      this.write(this.socket, {
        v: PROTOCOL_VERSION,
        type: 'presence',
        cursor: { anchor: encodeID(anchor), head: encodeID(head) },
      });
    }
  }

  /** Comments the characters from..to; false while offline. */
  addComment(from: ID, to: ID, text: string): boolean {
    if (!this.online || this.socket === null) {
      return false;
    }
    this.write(this.socket, { v: PROTOCOL_VERSION, type: 'comment', from: encodeID(from), to: encodeID(to), text });
    return true;
  }

  /** Resolves a comment; false while offline. */
  resolveComment(id: number): boolean {
    if (!this.online || this.socket === null) {
      return false;
    }
    this.write(this.socket, { v: PROTOCOL_VERSION, type: 'resolve', id });
    return true;
  }

  /** Hands over operations produced by local edits of the replica. */
  submit(ops: Op[]): void {
    if (ops.length === 0) {
      return;
    }
    if (this.online) {
      this.sendBatches(ops);
    } else {
      this.unsent.push(...ops);
    }
    this.emitStatus();
  }

  private connect(): void {
    this.retry = null;
    this.online = false;
    const socket = this.open(this.url);
    this.socket = socket;
    this.emitStatus();

    socket.onopen = () => {
      // Everything not acknowledged is resent: the server skips operations
      // it already stored (when only the ack was lost). The first chunk
      // goes into hello, the rest is sent after the welcome.
      const pending = [...this.inflight.flat(), ...this.unsent];
      const first = pending.slice(0, MAX_OPS_PER_MESSAGE);
      this.inflight = first.length > 0 ? [first] : [];
      this.unsent = pending.slice(MAX_OPS_PER_MESSAGE);
      this.write(socket, {
        v: PROTOCOL_VERSION,
        type: 'hello',
        docId: this.docId,
        ...(this.replica > 0 && { replica: this.replica }),
        ...(this.lastSeq > 0 && { lastSeq: this.lastSeq }),
        ...(first.length > 0 && { pending: first.map(encodeOp) }),
      });
    };
    socket.onmessage = (ev) => {
      try {
        this.handle(String(ev.data));
      } catch (err) {
        console.error('sync: dropping connection', err);
        socket.close(1002, 'protocol error');
      }
    };
    socket.onclose = (ev) => {
      if (this.socket !== socket) {
        return;
      }
      this.socket = null;
      this.online = false;
      // Others' cursors are unknown until the next welcome.
      if (this.cursors.size > 0) {
        this.cursors.clear();
        this.handlers.onPresence(this.cursors);
      }
      this.emitStatus(ev.reason || `code ${ev.code}`);
      if (!this.stopped && !this.paused) {
        this.retry = setTimeout(() => this.connect(), RECONNECT_DELAY_MS);
      }
    };
  }

  private handle(data: string): void {
    const msg = parseServerMessage(data);
    switch (msg.type) {
      case 'welcome': {
        if (this.replica !== 0 && msg.replica !== this.replica) {
          throw new Error(`sync: asked to resume replica ${this.replica}, got ${msg.replica}`);
        }
        this.replica = msg.replica;
        this.role = msg.role;
        if (msg.snapshot !== undefined) {
          const doc = Doc.restore(msg.replica, decodeSnapshot(msg.snapshot));
          // The snapshot does not contain our unacknowledged operations
          // (or contains them already; applying again is a no-op).
          for (const op of [...this.inflight.flat(), ...this.unsent]) {
            doc.apply(op);
          }
          this.doc = doc;
          this.handlers.onReset(doc);
        } else {
          const fresh = this.doc === null;
          const doc = this.doc ?? new Doc(msg.replica);
          for (const w of msg.ops ?? []) {
            doc.apply(decodeOp(w));
          }
          this.doc = doc;
          if (fresh) {
            this.handlers.onReset(doc);
          } else {
            this.handlers.onRemoteChange(doc);
          }
        }
        this.lastSeq = msg.seq;
        this.online = true;
        // Comments and cursors follow the welcome if there are any.
        this.comments.clear();
        this.handlers.onComments([]);
        this.cursors.clear();
        this.handlers.onPresence(this.cursors);
        const unsent = this.unsent;
        this.unsent = [];
        this.sendBatches(unsent);
        break;
      }
      case 'ops': {
        const doc = this.requireOnline();
        for (const w of msg.ops) {
          doc.apply(decodeOp(w));
        }
        this.lastSeq = Math.max(this.lastSeq, msg.seq);
        this.handlers.onRemoteChange(doc);
        break;
      }
      case 'presence': {
        this.requireOnline();
        if (msg.replica === this.replica) {
          break; // our own replica's old connection went away
        }
        if (msg.cursor === null || msg.user === undefined) {
          this.cursors.delete(msg.replica);
        } else {
          this.cursors.set(msg.replica, {
            replica: msg.replica,
            userId: msg.user.id,
            name: msg.user.name,
            anchor: decodeID(msg.cursor.anchor),
            head: decodeID(msg.cursor.head),
          });
        }
        this.handlers.onPresence(this.cursors);
        break;
      }
      case 'comments':
        this.requireOnline();
        this.comments.clear();
        for (const c of msg.comments) {
          this.comments.set(c.id, toComment(c));
        }
        this.handlers.onComments(this.commentList());
        break;
      case 'comment':
        this.requireOnline();
        if (msg.comment.resolved) {
          this.comments.delete(msg.comment.id);
        } else {
          this.comments.set(msg.comment.id, toComment(msg.comment));
        }
        this.handlers.onComments(this.commentList());
        break;
      case 'ack':
        this.requireOnline();
        if (this.inflight.shift() === undefined) {
          throw new Error('sync: ack without a pending batch');
        }
        this.lastSeq = Math.max(this.lastSeq, msg.seq);
        break;
      case 'error':
        if (FATAL_ERRORS.has(msg.code)) {
          // Not logged in, or no rights for this: retrying cannot help.
          this.stopped = true;
          this.handlers.onFatal(msg.code, msg.message);
          break;
        }
        // The server refused our state (e.g. it lost data we had seen) and
        // will close the connection. Our replica can never be accepted
        // again: start over from the server's version.
        console.error(`sync: server error ${msg.code}: ${msg.message}; dropping ${this.pending} local operations and resyncing`);
        this.doc = null;
        this.replica = 0;
        this.lastSeq = 0;
        this.inflight = [];
        this.unsent = [];
        break;
    }
    this.emitStatus();
  }

  private sendBatches(ops: Op[]): void {
    const socket = this.socket;
    if (socket === null || socket.readyState !== OPEN) {
      this.unsent.push(...ops);
      return;
    }
    for (let i = 0; i < ops.length; i += MAX_OPS_PER_MESSAGE) {
      const batch = ops.slice(i, i + MAX_OPS_PER_MESSAGE);
      this.inflight.push(batch);
      this.write(socket, { v: PROTOCOL_VERSION, type: 'ops', ops: batch.map(encodeOp) });
    }
  }

  private commentList(): CommentInfo[] {
    return [...this.comments.values()].sort((a, b) => a.id - b.id);
  }

  private requireOnline(): Doc {
    if (!this.online || this.doc === null) {
      throw new Error('sync: message before welcome');
    }
    return this.doc;
  }

  private emitStatus(reason?: string): void {
    const pending = this.pending;
    if (this.online) {
      this.handlers.onStatus({ state: 'online', replica: this.replica, role: this.role, seq: this.lastSeq, pending });
    } else if (this.socket !== null) {
      this.handlers.onStatus({ state: 'connecting', pending });
    } else {
      this.handlers.onStatus({ state: 'offline', reason: reason ?? (this.paused ? 'browser offline' : 'disconnected'), pending });
    }
  }

  private cancelRetry(): void {
    if (this.retry !== null) {
      clearTimeout(this.retry);
      this.retry = null;
    }
  }

  private write(socket: Socket, msg: ClientMessage): void {
    socket.send(JSON.stringify(msg));
  }
}

function toComment(c: WireComment): CommentInfo {
  return {
    id: c.id,
    from: decodeID(c.from),
    to: decodeID(c.to),
    text: c.text,
    authorId: c.author.id,
    authorName: c.author.name,
    createdAt: c.createdAt,
  };
}
