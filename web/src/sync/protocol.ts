// Wire format of the realtime protocol; specified in api/protocol.md.

import { isValidChar, type Entry, type ID, type Op } from '../crdt/doc';

export const PROTOCOL_VERSION = 1;

export type WireID = [lamport: number, replica: number];

export type WireOp =
  | { type: 'insert'; id: WireID; after: WireID; char: string }
  | { type: 'delete'; id: WireID };

export interface WireEntry {
  id: WireID;
  after: WireID;
  char: string;
  deleted?: boolean;
}

export interface WireSnapshot {
  chars: WireEntry[];
}

export interface WireCursor {
  anchor: WireID;
  head: WireID;
}

export interface WireUser {
  id: number;
  name: string;
}

export interface WireComment {
  id: number;
  from: WireID;
  to: WireID;
  text: string;
  author: WireUser;
  createdAt: string;
  resolved: boolean;
}

export type ClientMessage =
  | { v: number; type: 'hello'; docId: string; replica?: number; lastSeq?: number; pending?: WireOp[] }
  | { v: number; type: 'ops'; ops: WireOp[] }
  | { v: number; type: 'presence'; cursor: WireCursor }
  | { v: number; type: 'comment'; from: WireID; to: WireID; text: string }
  | { v: number; type: 'resolve'; id: number };

export type ServerMessage =
  // Either a snapshot or, for a client that resumed with lastSeq, the ops after it.
  | { v: number; type: 'welcome'; replica: number; role: string; seq: number; snapshot?: WireSnapshot; ops?: WireOp[] }
  | { v: number; type: 'ops'; seq: number; ops: WireOp[] }
  | { v: number; type: 'ack'; seq: number }
  | { v: number; type: 'presence'; replica: number; user?: WireUser; cursor: WireCursor | null }
  | { v: number; type: 'comments'; comments: WireComment[] }
  | { v: number; type: 'comment'; comment: WireComment }
  | { v: number; type: 'error'; code: string; message: string };

/** Thrown for data that does not follow the protocol. */
export class ProtocolError extends Error {
  override name = 'ProtocolError';
}

export function encodeOp(op: Op): WireOp {
  if (op.type === 'insert') {
    return { type: 'insert', id: encodeID(op.id), after: encodeID(op.after), char: op.char };
  }
  return { type: 'delete', id: encodeID(op.id) };
}

export function encodeID(id: ID): WireID {
  return [id.lamport, id.replica];
}

export function decodeOp(w: unknown): Op {
  if (typeof w !== 'object' || w === null) {
    throw new ProtocolError('op must be an object');
  }
  const o = w as Record<string, unknown>;
  const id = decodeID(o.id);
  if (o.type === 'insert') {
    if (typeof o.char !== 'string' || !isValidChar(o.char)) {
      throw new ProtocolError(`char ${JSON.stringify(o.char)} must be one Unicode code point`);
    }
    return { type: 'insert', id, after: decodeID(o.after), char: o.char };
  }
  if (o.type === 'delete') {
    return { type: 'delete', id };
  }
  throw new ProtocolError(`unknown op type ${JSON.stringify(o.type)}`);
}

export function decodeSnapshot(s: unknown): Entry[] {
  if (typeof s !== 'object' || s === null || !Array.isArray((s as { chars?: unknown }).chars)) {
    throw new ProtocolError('snapshot must be {chars: [...]}');
  }
  return (s as { chars: unknown[] }).chars.map((c, i) => {
    if (typeof c !== 'object' || c === null) {
      throw new ProtocolError(`snapshot char ${i} must be an object`);
    }
    const o = c as Record<string, unknown>;
    if (typeof o.char !== 'string' || !isValidChar(o.char)) {
      throw new ProtocolError(`snapshot char ${i}: ${JSON.stringify(o.char)} must be one Unicode code point`);
    }
    if (o.deleted !== undefined && typeof o.deleted !== 'boolean') {
      throw new ProtocolError(`snapshot char ${i}: deleted must be a boolean`);
    }
    return { id: decodeID(o.id), after: decodeID(o.after), char: o.char, deleted: o.deleted === true };
  });
}

export function decodeID(w: unknown): ID {
  if (!Array.isArray(w) || w.length !== 2) {
    throw new ProtocolError(`ID must be [lamport, replica], got ${JSON.stringify(w)}`);
  }
  const [lamport, replica] = w as unknown[];
  if (!isUint(lamport, Number.MAX_SAFE_INTEGER) || !isUint(replica, 0xffffffff)) {
    throw new ProtocolError(`invalid ID ${JSON.stringify(w)}`);
  }
  return { lamport, replica };
}

function isUint(v: unknown, max: number): v is number {
  return typeof v === 'number' && Number.isInteger(v) && v >= 0 && v <= max;
}

/** Parses a server message; ops are validated by decodeOp when applied. */
export function parseServerMessage(data: string): ServerMessage {
  let m: unknown;
  try {
    m = JSON.parse(data);
  } catch {
    throw new ProtocolError('malformed JSON from server');
  }
  if (typeof m !== 'object' || m === null) {
    throw new ProtocolError('server message must be an object');
  }
  const msg = m as { v?: unknown; type?: unknown };
  if (msg.v !== PROTOCOL_VERSION) {
    throw new ProtocolError(`unsupported protocol version ${JSON.stringify(msg.v)}`);
  }
  switch (msg.type) {
    case 'welcome':
    case 'ops':
    case 'ack':
    case 'presence':
    case 'comments':
    case 'comment':
    case 'error':
      return m as ServerMessage;
    default:
      throw new ProtocolError(`unknown message type ${JSON.stringify(msg.type)}`);
  }
}
