// The user's documents and creating a new one.

import { api, roleLabel, type User } from '../api';
import { el, mount } from '../ui';
import { header } from './header';

export async function renderDocuments(root: HTMLElement, user: User): Promise<void> {
  const docs = await api.documents();

  const title = el('input', { placeholder: 'Название нового документа', maxLength: 200 });
  const create = el('form', { class: 'row' }, title, el('button', { type: 'submit' }, 'Создать'));
  create.onsubmit = async (ev) => {
    ev.preventDefault();
    const d = await api.createDocument(title.value);
    location.href = `/?doc=${encodeURIComponent(d.id)}`;
  };

  const list =
    docs.length === 0
      ? el('p', { class: 'muted' }, 'Документов пока нет: создайте свой или откройте ссылку, которой с вами поделились.')
      : el(
          'ul',
          { class: 'docs' },
          ...docs.map((d) =>
            el(
              'li',
              {},
              el('a', { href: `/?doc=${encodeURIComponent(d.id)}` }, d.title),
              el('span', { class: 'muted' }, ` · ${roleLabel[d.role]}`),
            ),
          ),
        );

  mount(root, header(user, 'Мои документы'), create, list);
}
