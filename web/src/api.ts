// REST API of the server (api/http.md). The session lives in an HttpOnly
// cookie that the browser sends by itself; scripts never see it.

export type Role = 'owner' | 'editor' | 'commenter' | 'viewer';

export interface User {
  id: number;
  email: string;
  name: string;
}

export interface DocumentInfo {
  id: string;
  title: string;
  role: Role;
}

export interface LinkInfo {
  id: number;
  role: Role;
  createdAt: string;
  /** Only in the answer that creates the link. */
  token?: string;
}

export interface VersionInfo {
  id: number;
  name: string;
  seq: number;
  author: string;
  createdAt: string;
  /** Only when a single version is requested. */
  text?: string;
}

export class ApiError extends Error {
  override name = 'ApiError';
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    credentials: 'same-origin',
    headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) {
    return undefined as T;
  }
  const data: unknown = await res.json().catch(() => ({}));
  if (!res.ok) {
    const e = data as { error?: string; message?: string };
    throw new ApiError(res.status, e.error ?? 'http_error', e.message ?? res.statusText);
  }
  return data as T;
}

const doc = (id: string) => `/api/documents/${encodeURIComponent(id)}`;

export const api = {
  me: () => request<User>('GET', '/api/me'),
  register: (email: string, name: string, password: string) =>
    request<User>('POST', '/api/register', { email, name, password }),
  login: (email: string, password: string) => request<User>('POST', '/api/login', { email, password }),
  logout: () => request<void>('POST', '/api/logout'),

  documents: () => request<DocumentInfo[]>('GET', '/api/documents'),
  createDocument: (title: string) => request<DocumentInfo>('POST', '/api/documents', { title }),
  document: (id: string) => request<DocumentInfo>('GET', doc(id)),

  links: (id: string) => request<LinkInfo[]>('GET', `${doc(id)}/links`),
  createLink: (id: string, role: Role) => request<LinkInfo>('POST', `${doc(id)}/links`, { role }),
  revokeLink: (id: string, linkId: number) => request<void>('DELETE', `${doc(id)}/links/${linkId}`),
  join: (token: string) => request<DocumentInfo>('POST', `/api/links/${encodeURIComponent(token)}/join`),

  versions: (id: string) => request<VersionInfo[]>('GET', `${doc(id)}/versions`),
  saveVersion: (id: string, name: string) => request<VersionInfo>('POST', `${doc(id)}/versions`, { name }),
  version: (id: string, versionId: number) => request<VersionInfo>('GET', `${doc(id)}/versions/${versionId}`),
  restoreVersion: (id: string, versionId: number) =>
    request<{ seq: number }>('POST', `${doc(id)}/versions/${versionId}/restore`),
};

export function canEdit(role: string): boolean {
  return role === 'owner' || role === 'editor';
}

export const roleLabel: Record<Role, string> = {
  owner: 'владелец',
  editor: 'редактор',
  commenter: 'комментатор',
  viewer: 'читатель',
};
