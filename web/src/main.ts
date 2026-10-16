// Entry point: picks the page from the URL.
//   /            my documents (or login)
//   /?doc=ID     the editor
//   /?join=TOKEN open an access link, then the editor

import { ApiError, api, type User } from './api';
import { renderAuth } from './pages/auth';
import { renderDocuments } from './pages/documents';
import { renderEditor } from './pages/editor';
import { el, mount } from './ui';

const app = document.querySelector<HTMLElement>('#app')!;

async function route(): Promise<void> {
  const params = new URLSearchParams(location.search);

  let user: User | null = null;
  try {
    user = await api.me();
  } catch (err) {
    if (!(err instanceof ApiError && err.status === 401)) {
      throw err;
    }
  }
  if (user === null) {
    const note = params.has('join') ? 'Войдите или зарегистрируйтесь, чтобы открыть документ по ссылке.' : '';
    renderAuth(app, note, () => void route().catch(showError));
    return;
  }

  const join = params.get('join');
  if (join !== null) {
    try {
      const d = await api.join(join);
      // replace: the token does not stay in the browser history.
      location.replace(`/?doc=${encodeURIComponent(d.id)}`);
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) {
        message('Ссылка недействительна или отозвана.');
        return;
      }
      throw err;
    }
    return;
  }

  const docId = params.get('doc');
  if (docId !== null) {
    try {
      renderEditor(app, user, await api.document(docId));
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) {
        message('Документ не найден или у вас нет к нему доступа.');
        return;
      }
      throw err;
    }
    return;
  }

  await renderDocuments(app, user);
}

function message(text: string): void {
  mount(app, el('div', { class: 'card' }, el('p', {}, text), el('a', { href: '/' }, 'К моим документам')));
}

function showError(err: unknown): void {
  message(`Ошибка: ${err instanceof Error ? err.message : String(err)}`);
}

route().catch(showError);
