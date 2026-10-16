// The comments sidebar of the editor.

import type { CommentInfo } from '../sync/client';
import { el } from '../ui';

export interface CommentView {
  comment: CommentInfo;
  /** The text the comment covers now; '' when all of it was deleted. */
  quote: string;
  canResolve: boolean;
}

export interface CommentsPanel {
  element: HTMLElement;
  /** Redraws the list. */
  show(views: CommentView[], active: number | null): void;
  /** Enables the form when there is a selection to comment and a connection. */
  setCanAdd(enabled: boolean): void;
}

export function commentsPanel(opts: {
  canComment: boolean;
  onAdd(text: string): boolean;
  onResolve(id: number): void;
  onSelect(id: number): void;
}): CommentsPanel {
  const list = el('ul', { class: 'comment-list' });
  const element = el('aside', { class: 'comments' }, el('h2', {}, 'Комментарии'));

  let submit: HTMLButtonElement | null = null;
  if (opts.canComment) {
    const text = el('textarea', { placeholder: 'Выделите фрагмент текста и напишите комментарий', maxLength: 2000, rows: 3 });
    submit = el('button', { type: 'submit', disabled: true }, 'Комментировать выделение');
    const form = el('form', { class: 'comment-form' }, text, submit);
    form.onsubmit = (ev) => {
      ev.preventDefault();
      if (text.value.trim() !== '' && opts.onAdd(text.value.trim())) {
        text.value = '';
      }
    };
    element.append(form);
  }
  element.append(list);

  return {
    element,
    show(views, active) {
      if (views.length === 0) {
        list.replaceChildren(el('li', { class: 'muted' }, 'Комментариев нет.'));
        return;
      }
      list.replaceChildren(
        ...views.map(({ comment, quote, canResolve }) => {
          const quoteNode = el(
            'blockquote',
            { class: quote === '' ? 'muted' : '' },
            quote === '' ? 'фрагмент удалён' : `«${quote.length > 80 ? `${quote.slice(0, 80)}…` : quote}»`,
          );
          const item = el(
            'li',
            { class: comment.id === active ? 'active' : '' },
            quoteNode,
            el('p', {}, comment.text),
            el('span', { class: 'muted' }, `${comment.authorName} · ${new Date(comment.createdAt).toLocaleString('ru-RU')} `),
          );
          if (canResolve) {
            const resolve = el('button', { type: 'button', class: 'link' }, 'закрыть');
            resolve.onclick = (ev) => {
              ev.stopPropagation();
              opts.onResolve(comment.id);
            };
            item.append(resolve);
          }
          item.onclick = () => opts.onSelect(comment.id);
          return item;
        }),
      );
    },
    setCanAdd(enabled) {
      if (submit !== null) {
        submit.disabled = !enabled;
      }
    },
  };
}
