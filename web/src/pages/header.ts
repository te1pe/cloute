import { api, type User } from '../api';
import { el } from '../ui';

export function header(user: User, title: string, back = false): HTMLElement {
  const logout = el('button', { type: 'button', class: 'link' }, 'Выйти');
  logout.onclick = async () => {
    await api.logout();
    location.href = '/';
  };
  return el(
    'header',
    {},
    back && el('a', { href: '/' }, '← Мои документы'),
    el('h1', {}, title),
    el('span', { class: 'muted' }, user.name),
    logout,
  );
}
