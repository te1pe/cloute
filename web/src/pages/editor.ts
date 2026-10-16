// The editor: a <textarea> bound to a CRDT replica, others' cursors and
// comments drawn on a backdrop under it, the comments sidebar, link sharing
// for the owner and the version history.

import { api, canEdit, roleLabel, type DocumentInfo, type Role, type User } from '../api';
import { isValidChar, type Doc, type Op } from '../crdt/doc';
import { diff, toCodePoints, toUTF16, transformPosition } from '../editor/diff';
import { colorFor, paintBackdrop, type Caret, type Highlight } from '../editor/overlay';
import { SyncClient, type CommentInfo, type RemoteCursor, type Status } from '../sync/client';
import { el, mount } from '../ui';
import { commentsPanel } from './comments';
import { header } from './header';

/** Cursor updates are sent at most this often. */
const CURSOR_INTERVAL_MS = 100;

export function renderEditor(root: HTMLElement, user: User, info: DocumentInfo): void {
  const textarea = el('textarea', { id: 'editor', spellcheck: false, readOnly: true });
  const backdrop = el('div', { class: 'backdrop' });
  backdrop.setAttribute('aria-hidden', 'true');
  const statusLine = el('p', { id: 'status' }, 'загрузка…');
  let role: string = info.role;
  let stopped = false;
  let wasOnline = false;

  // The text the textarea showed after the last sync; input events are
  // diffed against it to find out what the user changed.
  let shown = '';
  let cursors: ReadonlyMap<number, RemoteCursor> = new Map();
  let comments: readonly CommentInfo[] = [];
  let activeComment: number | null = null;

  const panel = commentsPanel({
    canComment: info.role !== 'viewer',
    onAdd(text) {
      const doc = client.replicaDoc;
      const [start, end] = selection();
      if (doc === null || start === end) {
        return false;
      }
      return client.addComment(doc.idAt(start), doc.idAt(end - 1), text);
    },
    onResolve: (id) => client.resolveComment(id),
    onSelect(id) {
      const doc = client.replicaDoc;
      const c = comments.find((x) => x.id === id);
      const range = doc !== null && c !== undefined ? doc.rangeOf(c.from, c.to) : null;
      activeComment = id;
      if (range !== null) {
        textarea.focus();
        textarea.setSelectionRange(toUTF16(shown, range.start), toUTF16(shown, range.end));
      }
      paint();
    },
  });

  const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
  const client = new SyncClient(`${scheme}://${location.host}/ws`, info.id, {
    onReset: render,
    onRemoteChange: render,
    onStatus(status) {
      if (stopped) {
        return; // the page has been replaced by the refusal
      }
      if (status.state === 'online') {
        role = status.role;
        if (role !== info.role) {
          // The role changed (e.g. a link was revoked): the panels below
          // were built for the old one. Rebuild the page for the new role.
          if (client.pending === 0) {
            client.stop();
            location.reload();
            return;
          }
          statusLine.textContent = 'Ваша роль в документе изменилась: обновите страницу.';
          return;
        }
        if (!wasOnline) {
          sendCursor(); // the server forgot it with the old connection
        }
      }
      wasOnline = status.state === 'online';
      updateReadOnly();
      updateCommentForm();
      statusLine.textContent = describe(status);
    },
    onFatal(code) {
      // No session, or no role in the document any more (e.g. the link was
      // revoked): the text must not stay on screen either.
      stopped = true;
      client.stop();
      const text =
        code === 'unauthorized' ? 'Сессия закончилась: войдите снова.' : 'У вас больше нет доступа к этому документу.';
      mount(root, el('div', { class: 'card' }, el('p', {}, text), el('a', { href: '/' }, 'К моим документам')));
    },
    onPresence(c) {
      cursors = c;
      paint();
    },
    onComments(c) {
      comments = c;
      if (activeComment !== null && !c.some((x) => x.id === activeComment)) {
        activeComment = null;
      }
      paint();
    },
  });

  function updateReadOnly(): void {
    // Editable online and offline, if the role allows; before the first
    // welcome there is no replica to edit.
    textarea.readOnly = stopped || client.replicaDoc === null || !canEdit(role);
    backdrop.classList.toggle('readonly', textarea.readOnly);
  }

  /** The local selection in code points. */
  function selection(): [number, number] {
    return [toCodePoints(shown, textarea.selectionStart), toCodePoints(shown, textarea.selectionEnd)];
  }

  function updateCommentForm(): void {
    const [start, end] = selection();
    panel.setCanAdd(client.isOnline && start !== end);
  }

  textarea.addEventListener('input', () => {
    const doc = client.replicaDoc;
    if (doc === null || !canEdit(role)) {
      textarea.value = shown; // readOnly should prevent this; undo just in case
      return;
    }

    const change = diff(shown, textarea.value);
    const ops: Op[] = [];
    for (let i = 0; i < change.deleted; i++) {
      ops.push(doc.deleteAt(change.pos));
    }
    change.inserted.forEach((ch, i) => {
      // A lone surrogate (possible in a JS string) is not a character; store
      // U+FFFD, as the server would, instead of failing halfway through.
      ops.push(doc.insertAt(change.pos + i, isValidChar(ch) ? ch : '\uFFFD'));
    });
    shown = doc.text();
    if (textarea.value !== shown) {
      const caret = toUTF16(shown, change.pos + change.inserted.length);
      textarea.value = shown;
      textarea.setSelectionRange(caret, caret);
    }
    client.submit(ops);
    paint();
  });

  // Share the local selection, as IDs: it stays put while others edit.
  let cursorTimer: ReturnType<typeof setTimeout> | null = null;
  function scheduleCursor(): void {
    updateCommentForm();
    if (cursorTimer === null) {
      cursorTimer = setTimeout(() => {
        cursorTimer = null;
        sendCursor();
      }, CURSOR_INTERVAL_MS);
    }
  }
  function sendCursor(): void {
    const doc = client.replicaDoc;
    if (doc === null) {
      return;
    }
    const [start, end] = selection();
    const backward = textarea.selectionDirection === 'backward';
    client.setCursor(doc.idBefore(backward ? end : start), doc.idBefore(backward ? start : end));
  }
  for (const event of ['select', 'keyup', 'mouseup', 'input', 'focus']) {
    textarea.addEventListener(event, scheduleCursor);
  }
  textarea.addEventListener('scroll', () => {
    backdrop.scrollTop = textarea.scrollTop;
  });

  /** Shows the replica's text while keeping the local selection in place. */
  function render(doc: Doc): void {
    const next = doc.text();
    if (next !== shown) {
      const change = diff(shown, next);
      const start = transformPosition(toCodePoints(shown, textarea.selectionStart), change);
      const end = transformPosition(toCodePoints(shown, textarea.selectionEnd), change);
      const scroll = textarea.scrollTop;

      shown = next;
      textarea.value = next;
      textarea.setSelectionRange(toUTF16(next, start), toUTF16(next, end));
      textarea.scrollTop = scroll;
    }
    paint();
  }

  /** Redraws comment highlights, others' cursors and the sidebar. */
  function paint(): void {
    const doc = client.replicaDoc;
    if (doc === null) {
      backdrop.replaceChildren();
      panel.show([], null);
      return;
    }
    const chars = Array.from(shown);
    const highlights: Highlight[] = [];
    const views = comments.map((c) => {
      const range = doc.rangeOf(c.from, c.to);
      const quote = range === null ? '' : chars.slice(range.start, range.end).join('');
      if (range !== null && range.start < range.end) {
        highlights.push({ ...range, className: c.id === activeComment ? 'comment active' : 'comment' });
      }
      return { comment: c, quote, canResolve: c.authorId === user.id || canEdit(role) };
    });

    const carets: Caret[] = [];
    for (const cur of cursors.values()) {
      const anchor = doc.positionAfter(cur.anchor);
      const head = doc.positionAfter(cur.head);
      if (anchor === null || head === null) {
        continue; // points at something not applied here yet
      }
      if (anchor !== head) {
        highlights.push({
          start: Math.min(anchor, head),
          end: Math.max(anchor, head),
          className: 'selection',
          color: colorFor(cur.replica, 0.2),
        });
      }
      carets.push({ pos: head, color: colorFor(cur.replica), label: cur.name });
    }

    paintBackdrop(backdrop, shown, highlights, carets);
    backdrop.scrollTop = textarea.scrollTop;
    panel.show(views, activeComment);
  }

  function describe(status: Status): string {
    const who = `${roleLabel[role as Role] ?? role}`;
    const pending = status.pending > 0 ? ` · не отправлено: ${status.pending}` : '';
    switch (status.state) {
      case 'connecting':
        return `${who} · подключение…${pending}`;
      case 'online': {
        const others = cursors.size > 0 ? ` · ещё здесь: ${cursors.size}` : '';
        return `${who} · онлайн · seq ${status.seq}${others}${pending}`;
      }
      case 'offline':
        return `${who} · офлайн (${status.reason}), правки сохраняются локально${pending}`;
    }
  }

  // The browser knows about a lost network long before TCP does.
  window.addEventListener('offline', () => client.pause());
  window.addEventListener('online', () => client.resume());
  // Offline edits live in memory only: warn before they are thrown away.
  window.addEventListener('beforeunload', (ev) => {
    if (client.pending > 0) {
      ev.preventDefault();
    }
  });

  mount(
    root,
    header(user, info.title, true),
    info.role === 'owner' && sharePanel(info.id),
    versionsPanel(info.id, canEdit(info.role)),
    statusLine,
    el('div', { class: 'editor-layout' }, el('div', { class: 'editor-wrap' }, backdrop, textarea), panel.element),
  );
  client.start();
}

/**
 * The version history: everyone sees versions and their text; editors save
 * new ones and restore old ones. A restore arrives in the editor as ordinary
 * operations over the WebSocket, like anyone else's edit.
 */
function versionsPanel(docId: string, editable: boolean): HTMLElement {
  const list = el('ul', { class: 'versions' });
  const preview = el('pre', { class: 'preview', hidden: true });
  const error = el('p', { class: 'error' });

  async function refresh(): Promise<void> {
    const versions = await api.versions(docId);
    list.replaceChildren(
      ...versions.reverse().map((v) => {
        const show = el('button', { type: 'button', class: 'link' }, 'показать');
        show.onclick = async () => {
          const full = await api.version(docId, v.id);
          preview.textContent = full.text || '(пусто)';
          preview.hidden = false;
        };
        const item = el(
          'li',
          {},
          el('strong', {}, v.name),
          ` · ${new Date(v.createdAt).toLocaleString('ru-RU')} · ${v.author} `,
          show,
        );
        if (editable) {
          item.append(' ', restoreButton(v.id));
        }
        return item;
      }),
    );
  }

  // Two clicks instead of confirm(): a modal dialog would block the page.
  function restoreButton(versionId: number): HTMLButtonElement {
    const button = el('button', { type: 'button', class: 'link' }, 'откатить');
    let armed: ReturnType<typeof setTimeout> | null = null;
    button.onclick = async () => {
      if (armed === null) {
        button.textContent = 'точно откатить? (правки других участников сохранятся)';
        armed = setTimeout(() => {
          armed = null;
          button.textContent = 'откатить';
        }, 4000);
        return;
      }
      clearTimeout(armed);
      armed = null;
      button.textContent = 'откатить';
      try {
        await api.restoreVersion(docId, versionId);
        error.textContent = '';
      } catch (err) {
        error.textContent = `Не удалось откатить: ${err instanceof Error ? err.message : String(err)}`;
      }
    };
    return button;
  }

  const panel = el('details', { class: 'versions-panel' }, el('summary', {}, 'История версий'));
  if (editable) {
    const name = el('input', { placeholder: 'Название версии', maxLength: 100 });
    const save = el('form', { class: 'row' }, name, el('button', { type: 'submit' }, 'Сохранить версию'));
    save.onsubmit = async (ev) => {
      ev.preventDefault();
      try {
        await api.saveVersion(docId, name.value.trim() || `Версия от ${new Date().toLocaleString('ru-RU')}`);
        name.value = '';
        error.textContent = '';
        await refresh();
      } catch (err) {
        error.textContent = `Не удалось сохранить: ${err instanceof Error ? err.message : String(err)}`;
      }
    };
    panel.append(save);
  }
  panel.append(error, list, preview);
  panel.addEventListener('toggle', () => {
    if (panel.open) {
      void refresh();
    }
  });
  return panel;
}

/** Lets the owner create, see and revoke access links. */
function sharePanel(docId: string): HTMLElement {
  const role = el(
    'select',
    {},
    ...(['editor', 'commenter', 'viewer'] as const).map((r) => el('option', { value: r }, roleLabel[r])),
  );
  const create = el('button', { type: 'button' }, 'Создать ссылку');
  const created = el('div');
  const list = el('ul', { class: 'links' });

  async function refresh(): Promise<void> {
    const links = await api.links(docId);
    list.replaceChildren(
      ...links.map((l) => {
        const revoke = el('button', { type: 'button', class: 'link' }, 'отозвать');
        revoke.onclick = async () => {
          await api.revokeLink(docId, l.id);
          created.replaceChildren();
          await refresh();
        };
        const date = new Date(l.createdAt).toLocaleString('ru-RU');
        return el('li', {}, `${roleLabel[l.role]} · создана ${date} `, revoke);
      }),
    );
  }

  create.onclick = async () => {
    const link = await api.createLink(docId, role.value as Role);
    // The token is shown only now: the server keeps just its hash.
    const url = `${location.origin}/?join=${encodeURIComponent(link.token ?? '')}`;
    const field = el('input', { readOnly: true, value: url, class: 'url' });
    const copy = el('button', { type: 'button' }, 'Копировать');
    copy.onclick = () => void navigator.clipboard.writeText(url);
    created.replaceChildren(
      el('p', { class: 'muted' }, `Ссылка для роли «${roleLabel[link.role]}» (видна только сейчас):`),
      el('div', { class: 'row' }, field, copy),
    );
    field.select();
    await refresh();
  };

  void refresh();
  return el(
    'details',
    { class: 'share' },
    el('summary', {}, 'Доступ по ссылке'),
    el('div', { class: 'row' }, role, create),
    created,
    list,
  );
}
